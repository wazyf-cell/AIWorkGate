package main

// thoughtSignature preservation for Gemini tool call turns.
//
// The Cloud Code upstream rejects a replayed functionCall turn that lost the
// signature it issued with that call ("thought signature is invalid"), and its
// context quality degrades when the chain of thought is dropped. The reference
// solves this with a signature cache (gemini-sigstore.ts):
//
//	key      sha256("tool:" + name + "\0" + canonicalArgs(args))[:16]
//	lookup   exact key first, then the most recent entry for the same tool name
//	capture  every functionCall part's thoughtSignature during a stream
//	retry    on a signature rejection, resend once with no signatures at all
//
// This file is that cache. It is a cache, not a credential: a write failure only
// lowers the hit rate and must never fail an inference request.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	sigFileName    = "gemini-sigs.json"
	sigMaxEntries  = 2000
	sigKeyBodyMax  = 512
	sigKeyHexChars = 16
)

// sigEntry is one cached signature.
type sigEntry struct {
	Sig  string `json:"sig"`
	At   int64  `json:"at"`
	Name string `json:"name,omitempty"`
}

// sigStore is the in-memory cache plus its best-effort JSON mirror.
type sigStore struct {
	mu      sync.Mutex
	path    string
	entries map[string]sigEntry
	// order keeps insertion order (re-put moves a key to the end) because the
	// "most recent signature for this tool" fallback is defined by write order,
	// not by the second-resolution At timestamp.
	order []string
	dirty bool
}

// newSigStore opens the cache at dir. An empty dir keeps it memory-only, which
// is what the handler tests use.
func newSigStore(dir string) *sigStore {
	s := &sigStore{entries: map[string]sigEntry{}}
	if strings.TrimSpace(dir) != "" {
		s.path = filepath.Join(dir, sigFileName)
		s.load()
	}
	return s
}

// sigToolKey is geminiToolSigKey.
func sigToolKey(name, argsJSON string) string {
	body := argsJSON
	if len(body) > sigKeyBodyMax {
		body = body[:sigKeyBodyMax]
	}
	sum := sha256.Sum256([]byte("tool:" + name + "\x00" + body))
	return hex.EncodeToString(sum[:])[:sigKeyHexChars]
}

// lookup is the reference's lookupSignature callback: exact key, then the most
// recent signature for the same tool name.
func (s *sigStore) lookup(name, argsJSON string) string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[sigToolKey(name, argsJSON)]; ok && e.Sig != "" {
		return e.Sig
	}
	return s.latestLocked(name)
}

func (s *sigStore) latestLocked(name string) string {
	for i := len(s.order) - 1; i >= 0; i-- {
		if e, ok := s.entries[s.order[i]]; ok && e.Name == name && e.Sig != "" {
			return e.Sig
		}
	}
	return ""
}

// put records one signature. An empty signature is dropped: storing it would
// make the replay side believe a signature exists and send an empty field.
func (s *sigStore) put(name, argsJSON, signature string) {
	if s == nil {
		return
	}
	sig := strings.TrimSpace(signature)
	if sig == "" || strings.TrimSpace(name) == "" {
		return
	}
	key := sigToolKey(name, argsJSON)
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.entries[key]; exists {
		for i, k := range s.order {
			if k == key {
				s.order = append(s.order[:i], s.order[i+1:]...)
				break
			}
		}
	}
	s.order = append(s.order, key)
	s.entries[key] = sigEntry{Sig: sig, At: time.Now().Unix(), Name: name}
	if len(s.entries) > sigMaxEntries {
		s.evictLocked()
	}
	s.dirty = true
}

// evictLocked drops the oldest half, which is what the reference's evict does.
func (s *sigStore) evictLocked() {
	type pair struct {
		key string
		at  int64
	}
	all := make([]pair, 0, len(s.order))
	for _, k := range s.order {
		all = append(all, pair{k, s.entries[k].At})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at < all[j].at })
	drop := len(all) / 2
	kept := make([]string, 0, len(s.order)-drop)
	for i, p := range all {
		if i < drop {
			delete(s.entries, p.key)
			continue
		}
		kept = append(kept, p.key)
	}
	// Preserve write order among the survivors.
	keep := make(map[string]struct{}, len(kept))
	for _, k := range kept {
		keep[k] = struct{}{}
	}
	out := s.order[:0]
	for _, k := range s.order {
		if _, ok := keep[k]; ok {
			out = append(out, k)
		}
	}
	s.order = out
}

// flush persists the cache. Failure is logged by the caller at most; it never
// affects a request.
func (s *sigStore) flush() error {
	if s == nil || s.path == "" {
		return nil
	}
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	s.dirty = false
	payload, err := json.Marshal(s.entries)
	path := s.path
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return writeFileAtomic(path, payload, 0o600)
}

// load reads the mirror; a missing or corrupt file means an empty cache.
func (s *sigStore) load() {
	raw, err := os.ReadFile(s.path)
	if err != nil || len(raw) == 0 || len(raw) > 1<<20 {
		return
	}
	var entries map[string]sigEntry
	if json.Unmarshal(raw, &entries) != nil {
		return
	}
	s.entries = entries
	// The file is a map, so order was not persisted; derive a stable order from
	// the timestamps so latestForTool still prefers the freshest entry.
	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return entries[keys[i]].At < entries[keys[j]].At })
	s.order = keys
}

// canonicalArgsJSON renders parsed tool arguments with sorted keys, which is
// what the reference uses as the cache key body.
func canonicalArgsJSON(args map[string]any) string {
	b, err := json.Marshal(sortValue(args))
	if err != nil {
		return "{}"
	}
	return string(b)
}

// isSignatureError is isGeminiSignatureError: the upstream names the problem
// either as "signature" or as "thought ... invalid".
func isSignatureError(text string) bool {
	lower := strings.ToLower(text)
	if strings.Contains(lower, "signature") {
		return true
	}
	return strings.Contains(lower, "thought") && strings.Contains(lower, "invalid")
}
