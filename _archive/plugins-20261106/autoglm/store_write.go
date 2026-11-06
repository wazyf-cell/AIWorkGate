package main

// Credential mutations: upsert, remove, seal-and-flush, and the atomic writer.
//
// Split from store.go to keep each file inside the 300-line budget. The seal is
// DPAPI (dpapi_windows.go); on a non-Windows host seal() refuses, so the file is
// never written in the clear.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// upsert inserts or replaces an account keyed by (region, userId) and flushes.
func (s *store) upsert(a *account) error {
	if a == nil || !a.fresh() {
		return errors.New("autoglm: refusing to store an account without accessToken/userId")
	}
	a.update(func(d *accountData) {
		if d.Region == "" {
			d.Region = regionCN
		}
		if d.DeviceID == "" {
			d.DeviceID = newDeviceID()
		}
	})

	s.mu.Lock()
	replaced := false
	key := a.snapshot()
	for i, ex := range s.accounts {
		snap := ex.snapshot()
		if snap.UserID == key.UserID && snap.Region == key.Region {
			s.accounts[i] = a
			replaced = true
			break
		}
	}
	if !replaced {
		s.accounts = append(s.accounts, a)
	}
	err := s.flushLocked()
	s.mu.Unlock()
	return err
}

// remove deletes one account and flushes.
func (s *store) remove(uid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, a := range s.accounts {
		if a.snapshot().UserID == uid {
			s.accounts = append(s.accounts[:i], s.accounts[i+1:]...)
			_ = s.flushLocked()
			return true
		}
	}
	return false
}

// flushLocked seals and atomically rewrites the file. The caller holds s.mu.
func (s *store) flushLocked() error {
	doc := sealBody{
		Version:  storeVersion,
		Accounts: make([]accountData, 0, len(s.accounts)),
		State:    make(map[string]*accountState, len(s.states)),
	}
	for _, a := range s.accounts {
		doc.Accounts = append(doc.Accounts, a.snapshot())
	}
	for k, st := range s.states {
		doc.State[k] = st
	}
	plain, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("autoglm: encode credentials: %w", err)
	}
	sealed, err := seal(plain)
	if err != nil {
		return fmt.Errorf("autoglm: seal credentials: %w", err)
	}
	if err := writeFileAtomic(s.path, sealed, 0o600); err != nil {
		return fmt.Errorf("autoglm: write %s: %w", accountsFile, err)
	}
	return nil
}

// syncFlush persists accounts the upstream refreshed in place.
func (s *store) syncFlush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushLocked()
}

// newDeviceID mints the per-account uuid-like device id the upstream
// fingerprints (the reference derives it from md5(phone+nanotime)).
func newDeviceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strings.Repeat("0", 32)
	}
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// writeFileAtomic writes to a temp file in the same directory then renames, so
// a crash mid-write cannot truncate the credential file.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".accounts-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
