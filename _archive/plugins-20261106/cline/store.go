package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The plugin owns its Cline credential. It is sealed with DPAPI under the current
// Windows user (dpapi_windows.go) and stored in DATA_DIR\accounts.dat. The host
// never passes a token in through the environment.
const (
	accountsFile = "accounts.dat"
	storeVersion = 1
	maxStoreSize = 1 << 20
)

// clineCredential is one Cline session. The field names mirror the reference's
// credential shape (buildClineCredential / applyClineRefresh) so the same data can
// move between the two implementations.
type clineCredential struct {
	// AccessToken is stored WITHOUT the workos: prefix; bearerValue adds it on
	// the wire. Storing it bare keeps re-logins from stacking prefixes.
	AccessToken string `json:"access_token"`
	// RefreshToken is the WorkOS refresh token.
	RefreshToken string `json:"refresh_token,omitempty"`
	// ExpireTime is Unix ms; 0 means "unknown" and is never treated as expired.
	ExpireTime int64 `json:"expire_time,omitempty"`
	// AccountID is Cline's user id, when the register/refresh response carries it.
	AccountID string `json:"account_id,omitempty"`
	// Email / Nickname are display-only, never secrets.
	Email     string `json:"email,omitempty"`
	Nickname  string `json:"nickname,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// empty reports whether the credential has no access token.
func (c clineCredential) empty() bool { return strings.TrimSpace(c.AccessToken) == "" }

// refreshable reports whether a WorkOS refresh token is present.
func (c clineCredential) refreshable() bool { return strings.TrimSpace(c.RefreshToken) != "" }

// expired reports whether the credential's known expiry has passed. An unknown
// expiry (0) is never expired: pretending it is would force a pointless refresh.
func (c clineCredential) expired(now int64) bool {
	return c.ExpireTime > 0 && c.ExpireTime <= now
}

// label is the human display name.
func (c clineCredential) label() string {
	return firstNonEmpty(c.Nickname, c.Email, c.AccountID, "Cline")
}

// buildCredential projects a register/refresh response onto the credential.
//
// The response envelope is either the payload itself or {"data": {...}}; the
// reference accepts both (parseClineTokenPayload), and the access token may be
// spelled accessToken or access_token.
func buildCredential(payload map[string]any) *clineCredential {
	inner := payload
	if data, ok := payload["data"].(map[string]any); ok {
		inner = data
	}
	access := firstNonEmpty(stringField(inner, "accessToken"), stringField(inner, "access_token"))
	refresh := firstNonEmpty(stringField(inner, "refreshToken"), stringField(inner, "refresh_token"))
	accountID := firstNonEmpty(stringField(inner, "accountId"), stringField(inner, "account_id"))
	email := stringField(inner, "email")
	nickname := email
	if userInfo, ok := inner["userInfo"].(map[string]any); ok {
		accountID = firstNonEmpty(stringField(userInfo, "clineUserId"), stringField(userInfo, "accountId"), accountID)
		email = firstNonEmpty(stringField(userInfo, "email"), email)
		first := stringField(userInfo, "firstName")
		last := stringField(userInfo, "lastName")
		if name := strings.TrimSpace(first + " " + last); name != "" {
			nickname = name
		}
	}
	return &clineCredential{
		// Stored bare; bearerValue re-adds the prefix.
		AccessToken:  bareToken(access),
		RefreshToken: refresh,
		ExpireTime:   timestampMs(inner, "expiresAt", "expires_at", "expire_time"),
		AccountID:    accountID,
		Email:        email,
		Nickname:     nickname,
	}
}

// timestampMs parses a timestamp that may be seconds, milliseconds, or an RFC3339
// string (parseClineTimestamp).
func timestampMs(m map[string]any, keys ...string) int64 {
	for _, key := range keys {
		raw, ok := m[key]
		if !ok {
			continue
		}
		switch v := raw.(type) {
		case float64:
			if v <= 0 {
				continue
			}
			// Anything below year 2001 in ms is really a seconds value.
			if v < 1e12 {
				return int64(v * 1000)
			}
			return int64(v)
		case string:
			if t, err := time.Parse(time.RFC3339, strings.TrimSpace(v)); err == nil {
				return t.UnixMilli()
			}
		}
	}
	return 0
}

// store is the DPAPI-sealed credential file.
type store struct {
	mu       sync.RWMutex
	path     string
	creds    clineCredential
	sealKind string
}

func newStore(dir string) (*store, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, errors.New("cline: plugin data dir is empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("cline: create data dir: %w", err)
	}
	s := &store{path: filepath.Join(dir, accountsFile), sealKind: sealKind()}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// load reads the sealed file. A missing file is not an error: a fresh install
// simply has no login yet.
func (s *store) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("cline: read %s: %w", accountsFile, err)
	}
	if len(data) == 0 || len(data) > maxStoreSize {
		return fmt.Errorf("cline: %s has an unexpected size (%d bytes)", accountsFile, len(data))
	}
	plain, err := open(data)
	if err != nil {
		return fmt.Errorf("cline: unseal %s: %w", accountsFile, err)
	}
	var payload struct {
		Version    int             `json:"version"`
		Credential clineCredential `json:"credential"`
	}
	if err := json.Unmarshal(plain, &payload); err != nil {
		return fmt.Errorf("cline: parse %s: %w", accountsFile, err)
	}
	if payload.Version != 0 && payload.Version != storeVersion {
		return fmt.Errorf("cline: %s version %d is not supported", accountsFile, payload.Version)
	}
	s.creds = payload.Credential
	return nil
}

// get returns the current credential and whether one is present.
func (s *store) get() (clineCredential, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.creds, !s.creds.empty()
}

// configured reports whether a login is stored.
func (s *store) configured() bool {
	_, ok := s.get()
	return ok
}

// set replaces the credential atomically. An empty access token is rejected:
// writing it would silently wipe a working login.
func (s *store) set(c *clineCredential) error {
	if c == nil {
		return errors.New("cline: no credential")
	}
	c.AccessToken = bareToken(c.AccessToken)
	c.RefreshToken = strings.TrimSpace(c.RefreshToken)
	c.AccountID = strings.TrimSpace(c.AccountID)
	c.Email = strings.TrimSpace(c.Email)
	c.Nickname = strings.TrimSpace(c.Nickname)
	if c.AccessToken == "" {
		return errors.New("cline: refusing to store an empty access token")
	}
	c.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	plain, err := json.Marshal(map[string]any{
		"version":    storeVersion,
		"credential": c,
	})
	if err != nil {
		return fmt.Errorf("cline: encode credentials: %w", err)
	}
	sealed, err := seal(plain)
	if err != nil {
		return fmt.Errorf("cline: seal credentials: %w", err)
	}
	if err := writeFileAtomic(s.path, sealed, 0o600); err != nil {
		return fmt.Errorf("cline: write %s: %w", accountsFile, err)
	}
	s.mu.Lock()
	s.creds = *c
	s.mu.Unlock()
	return nil
}

// clear removes the stored login.
func (s *store) clear() error {
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("cline: remove %s: %w", accountsFile, err)
	}
	s.mu.Lock()
	s.creds = clineCredential{}
	s.mu.Unlock()
	return nil
}

// readFileLimited reads at most limit bytes from path.
func readFileLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, limit))
}

// writeFileAtomic writes via a temp file in the same directory and renames it, so
// a crash never leaves a half-written credential file behind.
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
