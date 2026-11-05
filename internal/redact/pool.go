// 从 magpie internal/redact 移植（逐节等价切分自原单文件 redact.go；
// 拆分只调整文件边界与 import，不改变任何逻辑，见 README 的来源与完整性节）。
package redact

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// ---- placeholders ---------------------------------------------------------------

var (
	mu     sync.RWMutex
	values = map[string]string{} // placeholder → value
	kinds  = map[string]bool{}   // the kinds of values, for wordTail
	// used is when each placeholder was last made or met again, by
	// clock: makeRoom lets the ones used longest ago go first
	used    = map[string]uint64{}
	clock   uint64
	keyOf   []byte
	keyPath string // guarded by mu, as keyOf is
)

// maxValues bounds what is held; past it the half used longest ago is let
// go, and a request that still has them brings them back.
const maxValues = 200_000

func key() []byte {
	mu.RLock()
	k := keyOf
	mu.RUnlock()
	if k != nil {
		return k
	}
	mu.Lock()
	defer mu.Unlock()
	if keyOf != nil {
		return keyOf
	}
	keyOf = loadKey()
	return keyOf
}

// SetKeyPath chooses where the placeholder key is kept across restarts;
// "" keeps it in memory only. The gateway's owner sets it before requests
// arrive. Once the key is loaded, both it and its path stay fixed.
func SetKeyPath(path string) {
	mu.Lock()
	defer mu.Unlock()
	if keyOf == nil {
		keyPath = path
	}
}

// loadKey is called with mu held.
func loadKey() []byte {
	if keyPath != "" {
		if b, err := os.ReadFile(keyPath); err == nil && len(b) >= 32 {
			return b[:32]
		}
	}
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	if keyPath != "" {
		_ = os.MkdirAll(filepath.Dir(keyPath), 0o700)
		_ = os.WriteFile(keyPath, b, 0o600)
	}
	return b
}

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// tag is the placeholder v of kind goes as, made and not remembered: a
// value sent as written (Warn) is named by it in the record of what was
// found, and a preview names each value by it.
func tag(kind, v string) string {
	h := hmac.New(sha256.New, key())
	h.Write([]byte(kind + "\x00" + v))
	return "{{" + kind + "_" + strings.ToLower(b32.EncodeToString(h.Sum(nil))[:8]) + "}}"
}

func placeholder(kind, v string) string {
	p := tag(kind, v)
	mu.Lock()
	if _, ok := values[p]; !ok {
		makeRoom()
		values[p] = v
		kinds[kind] = true
		addKnown(p, v)
	}
	touch(p)
	mu.Unlock()
	return p
}

// touch says p was used now; called with mu held.
func touch(p string) {
	clock++
	used[p] = clock
}

// makeRoom lets the half of values used longest ago go when they are
// full; called with mu held. What a request in flight has just masked, or
// met again, is the newest: let go at random, a value masked a moment
// before could be gone by the time its placeholder came back, and the
// answer kept the placeholder.
func makeRoom() {
	if len(values) < maxValues {
		return
	}
	ps := make([]string, 0, len(values))
	for p := range values {
		ps = append(ps, p)
	}
	sort.Slice(ps, func(i, j int) bool { return used[ps[i]] < used[ps[j]] })
	for _, p := range ps[:len(ps)/2] {
		delete(values, p)
		delete(shapes, p)
		delete(used, p)
	}
	reindexKnown()
}

// put is how o sends a value: as its placeholder, or as a made-up key of
// its form (Shape).
func (o Options) put() func(kind, v string) string {
	if o.Shape {
		return standIn(shaped)
	}
	return standIn(placeholder)
}

// ---- masking --------------------------------------------------------------------
