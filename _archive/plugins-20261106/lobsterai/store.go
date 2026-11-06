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
// Windows user (see dpapi_windows.go) and stored in DATA_DIR\accounts.dat.
const (
	accountsFile = "accounts.dat"
	maxStoreSize = 1 << 20
)

// credentials is one LobsterAI (有道) login.
//
// Field names ported from the reference (channel-pack/src/lobsterai.ts,
// buildLobsteraiCredential): the refresh and models calls both send
// firstKeyfrom/latestKeyfrom/uuid/userId, and the reference is explicit that
// latestKeyfrom is NOT updated on refresh — "严格对齐 Go 的 KeyfromBody()：它读的
// 就是 a.LatestKeyfrom，而 RefreshToken 从不更新该字段". So both timestamps are
// frozen at login time here too.
type credentials struct {
	AccessToken   string `json:"access_token"`
	RefreshToken  string `json:"refresh_token,omitempty"`
	FirstKeyfrom  string `json:"first_keyfrom,omitempty"`
	LatestKeyfrom string `json:"latest_keyfrom,omitempty"`
	UUID          string `json:"uuid,omitempty"`
	UserID        string `json:"user_id,omitempty"`
	YID           string `json:"yid,omitempty"`
	AccountUserID string `json:"account_user_id,omitempty"`
	Nickname      string `json:"nickname,omitempty"`
	ExpiresAt     string `json:"expires_at,omitempty"`
	// BaseURL lets a test or a mirror redirect the API origin.
	BaseURL string `json:"base_url,omitempty"`
	// Label is a human tag, never a secret.
	Label string `json:"label,omitempty"`
	// UpdatedAt is when the credential or its identity last changed.
	UpdatedAt int64 `json:"updated_at,omitempty"`
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
		return nil, errors.New("lobsterai: plugin data dir is empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("lobsterai: create data dir: %w", err)
	}
	s := &store{path: filepath.Join(dir, accountsFile), sealKind: sealKind()}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *store) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("lobsterai: read %s: %w", accountsFile, err)
	}
	if len(data) == 0 || len(data) > maxStoreSize {
		return fmt.Errorf("lobsterai: %s has an unexpected size (%d bytes)", accountsFile, len(data))
	}
	plain, err := open(data)
	if err != nil {
		return fmt.Errorf("lobsterai: unseal %s: %w", accountsFile, err)
	}
	var c credentials
	if err := json.Unmarshal(plain, &c); err != nil {
		return fmt.Errorf("lobsterai: parse %s: %w", accountsFile, err)
	}
	s.creds = c
	return nil
}

func (s *store) get() (credentials, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.creds, !s.creds.empty()
}

func (s *store) configured() bool {
	_, ok := s.get()
	return ok
}

// clear removes the stored credential (the host's account delete).
func (s *store) clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creds = credentials{}
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("lobsterai: remove %s: %w", accountsFile, err)
	}
	return nil
}

func (s *store) set(c credentials) error {
	c.AccessToken = strings.TrimSpace(c.AccessToken)
	c.BaseURL = strings.TrimSpace(c.BaseURL)
	c.UUID = strings.TrimSpace(c.UUID)
	if c.AccessToken == "" {
		return errors.New("lobsterai: refusing to store an empty token")
	}
	if c.UpdatedAt == 0 {
		c.UpdatedAt = timeNowUnix()
	}
	plain, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("lobsterai: encode credentials: %w", err)
	}
	sealed, err := seal(plain)
	if err != nil {
		return fmt.Errorf("lobsterai: seal credentials: %w", err)
	}
	if err := writeFileAtomic(s.path, sealed, 0o600); err != nil {
		return fmt.Errorf("lobsterai: write %s: %w", accountsFile, err)
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
