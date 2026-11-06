package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// The plugin owns its credentials. They are sealed with DPAPI under the current
// Windows user (see dpapi_windows.go) and stored in DATA_DIR\accounts.dat. The
// host never passes a token in through the environment, and the page never sees
// a plaintext secret: same rule as the zcode/aichat plugins.
const (
	accountsFile = "accounts.dat"
	maxStoreSize = 1 << 20
)

// credentials is one Loomy login.
//
// Ported field names from the reference (channel-pack/src/loomy.ts and
// loomy-adapter.ts): the adapter reads credential.access_token and
// credential.expires_at, and the remote catalog call sends the token as a
// `token` header rather than a bearer.
type credentials struct {
	// AccessToken is the session token (LOOMY_ACCESS_TOKEN in the reference).
	AccessToken string `json:"access_token"`
	// UserID is the account id the login response returns (`userid`).
	UserID string `json:"userid,omitempty"`
	// Session is the account session the login response returns (`session`).
	Session string `json:"session,omitempty"`
	// ExpiresAt is the epoch-millisecond expiry the reference stores as a string.
	ExpiresAt string `json:"expires_at,omitempty"`
	// BaseURL lets a test or a mirror redirect the API origin.
	BaseURL string `json:"base_url,omitempty"`
	// Label is a human tag, never a secret.
	Label string `json:"label,omitempty"`
}

func (c credentials) empty() bool { return strings.TrimSpace(c.AccessToken) == "" }

// store is the DPAPI-sealed credential file.
type store struct {
	mu       sync.RWMutex
	path     string
	creds    credentials
	sealKind string
}

func newStore(dir string) (*store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("loomy: plugin data dir is empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("loomy: create data dir: %w", err)
	}
	s := &store{path: filepath.Join(dir, accountsFile), sealKind: sealKind()}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// load reads the sealed file. A missing file is not an error: the plugin simply
// has no credentials yet.
func (s *store) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("loomy: read %s: %w", accountsFile, err)
	}
	if len(data) == 0 || len(data) > maxStoreSize {
		return fmt.Errorf("loomy: %s has an unexpected size (%d bytes)", accountsFile, len(data))
	}
	plain, err := open(data)
	if err != nil {
		return fmt.Errorf("loomy: unseal %s: %w", accountsFile, err)
	}
	var c credentials
	if err := json.Unmarshal(plain, &c); err != nil {
		return fmt.Errorf("loomy: parse %s: %w", accountsFile, err)
	}
	s.creds = c
	return nil
}

// get returns the current credentials and whether any are present.
func (s *store) get() (credentials, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.creds, !s.creds.empty()
}

// configured reports whether a token is stored, without exposing it.
func (s *store) configured() bool {
	_, ok := s.get()
	return ok
}

// set replaces the credentials atomically. An empty token is rejected: writing it
// would silently wipe a working login.
func (s *store) set(c credentials) error {
	c.AccessToken = strings.TrimSpace(c.AccessToken)
	c.BaseURL = strings.TrimSpace(c.BaseURL)
	if c.AccessToken == "" {
		return errors.New("loomy: refusing to store an empty token")
	}
	plain, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("loomy: encode credentials: %w", err)
	}
	sealed, err := seal(plain)
	if err != nil {
		return fmt.Errorf("loomy: seal credentials: %w", err)
	}
	if err := writeFileAtomic(s.path, sealed, 0o600); err != nil {
		return fmt.Errorf("loomy: write %s: %w", accountsFile, err)
	}
	s.mu.Lock()
	s.creds = c
	s.mu.Unlock()
	return nil
}

// writeFileAtomic writes to a temp file in the same directory and renames it, so
// a crash mid-write cannot leave a truncated credential file behind.
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
