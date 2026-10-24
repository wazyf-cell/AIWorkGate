package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// storeFile is the file this plugin owns inside AIWORKGATE_PLUGIN_DATA_DIR.
// Per Q18 the assistant's base_url and api_key belong to the plugin, not to the
// host: the host must never hand the key over through an environment variable
// (that would only move the hardcoding), and the plugin must never read the
// assistant's own vault (%APPDATA%\AIWorkAssistant\conf\vault.stronghold).
const storeFile = "accounts.dat"

// storeVersion is written into the envelope so a later multi-account layout can
// migrate an existing file instead of guessing.
const storeVersion = 1

// upstreamRecord is the single upstream this skeleton forwards to. The field
// names are stable: the host's one-time migration (R1, not implemented in this
// round) is expected to POST the same pair to /admin/upstream.
type upstreamRecord struct {
	BaseURL   string `json:"base_url"`
	APIKey    string `json:"api_key"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

type storeEnvelope struct {
	Version  int            `json:"version"`
	Upstream upstreamRecord `json:"upstream"`
}

// store is the plugin-private credential store. The bytes on disk are sealed by
// DPAPI on Windows (see dpapi_windows.go); on other platforms seal falls back to
// plaintext guarded by a 0600 file mode, because DPAPI does not exist there and
// the plugin itself only ships for windows (manifest "os").
type store struct {
	dir  string
	path string

	mu  sync.RWMutex
	cur upstreamRecord
	// sealing says how the current file was written, for logs and /meta.
	sealing string
}

func newStore(dir string) (*store, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, errors.New("store: data dir is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("store: create data dir: %w", err)
	}
	s := &store{dir: dir, path: filepath.Join(dir, storeFile), sealing: sealKind()}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// load reads the sealed file if present. A missing file is not an error: a
// freshly installed plugin simply has no upstream configured yet.
func (s *store) load() error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("store: read %s: %w", storeFile, err)
	}
	plain, err := open(raw)
	if err != nil {
		return fmt.Errorf("store: unseal %s: %w", storeFile, err)
	}
	var env storeEnvelope
	if err := json.Unmarshal(plain, &env); err != nil {
		return fmt.Errorf("store: parse %s: %w", storeFile, err)
	}
	if env.Version != 0 && env.Version != storeVersion {
		return fmt.Errorf("store: %s version %d is not supported", storeFile, env.Version)
	}
	s.mu.Lock()
	s.cur = env.Upstream
	s.mu.Unlock()
	return nil
}

// get returns the configured upstream, if any.
func (s *store) get() (baseURL, apiKey string, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	base := strings.TrimRight(strings.TrimSpace(s.cur.BaseURL), "/")
	key := strings.TrimSpace(s.cur.APIKey)
	if base == "" {
		return "", "", false
	}
	return base, key, true
}

func (s *store) configured() bool {
	_, _, ok := s.get()
	return ok
}

// set persists a new upstream. base_url must be an absolute http(s) URL; the
// key may be empty (the assistant also answers 401 without one, which the
// caller surfaces verbatim) but a non-empty key is the normal case.
func (s *store) set(baseURL, apiKey string) error {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return errors.New("base_url is required")
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		return fmt.Errorf("base_url %q must start with http:// or https://", base)
	}
	rec := upstreamRecord{
		BaseURL:   base,
		APIKey:    strings.TrimSpace(apiKey),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	plain, err := json.Marshal(storeEnvelope{Version: storeVersion, Upstream: rec})
	if err != nil {
		return fmt.Errorf("store: encode: %w", err)
	}
	sealed, err := seal(plain)
	if err != nil {
		return fmt.Errorf("store: seal: %w", err)
	}
	if err := writeFileAtomic(s.path, sealed, 0o600); err != nil {
		return fmt.Errorf("store: write %s: %w", storeFile, err)
	}
	s.mu.Lock()
	s.cur = rec
	s.mu.Unlock()
	return nil
}

// writeFileAtomic writes via a temp file in the same directory, fsyncs it and
// renames it over the destination, so a crash never leaves a half-written
// credential file (and never a partially sealed blob).
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
