package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The plugin owns its credentials. They are sealed with DPAPI under the current
// Windows user (see dpapi_windows.go) and stored in DATA_DIR\accounts.dat. The
// host never passes a token in through the environment, and the page never sees
// a plaintext secret: this is the same rule the zcode and aichat plugins follow.
const (
	accountsFile = "accounts.dat"
	maxStoreSize = 1 << 20
)

// credentials is one MiniMax login, shaped like the reference's credential
// object (parseMinimaxCredential) so a value exported from the reference can be
// pasted in unchanged.
type credentials struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	TokenType    string `json:"token_type,omitempty"`
	// ExpiresAt is written as a string by the reference (String(fromJwt ?? ...)),
	// so it is kept as a string here rather than normalised to an int.
	ExpiresAt string `json:"expires_at,omitempty"`
	Scope     string `json:"scope,omitempty"`
	AccountID string `json:"account_id,omitempty"`
	// Nickname is a human tag, never a secret.
	Nickname string `json:"nickname,omitempty"`
	// BaseURL lets a test or a mirror redirect the API origin.
	BaseURL string `json:"base_url,omitempty"`
	// AccountBaseURL redirects the OAuth origin only.
	AccountBaseURL string `json:"account_base_url,omitempty"`
}

func (c credentials) empty() bool { return strings.TrimSpace(c.AccessToken) == "" }

func (c credentials) refreshable() bool { return strings.TrimSpace(c.RefreshToken) != "" }

// expiresAtMs resolves the expiry from expires_at, and failing that from the JWT
// exp claim — the same two-step minimaxCredentialExpiresAtMs implements.
func (c credentials) expiresAtMs() (int64, bool) {
	if raw := strings.TrimSpace(c.ExpiresAt); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
			// Seconds vs milliseconds: anything below 1e12 is seconds.
			if n > 1e12 {
				return n, true
			}
			return n * 1000, true
		}
	}
	if exp, ok := jwtExpMs(c.AccessToken); ok {
		return exp, true
	}
	return 0, false
}

// jwtExpMs reads the exp claim of a JWT without verifying it. The token is opaque
// to this plugin beyond its expiry, which is all the reference reads too.
func jwtExpMs(token string) (int64, bool) {
	segments := strings.Split(token, ".")
	if len(segments) != 3 {
		return 0, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(segments[1], "="))
	if err != nil {
		return 0, false
	}
	var claims struct {
		Exp json.Number `json:"exp"`
		Sub string      `json:"sub"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return 0, false
	}
	v, err := claims.Exp.Int64()
	if err != nil || v <= 0 {
		return 0, false
	}
	return v * 1000, true
}

// jwtSub reads the sub claim, which the reference stores as account_id.
func jwtSub(token string) string {
	segments := strings.Split(token, ".")
	if len(segments) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(segments[1], "="))
	if err != nil {
		return ""
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return claims.Sub
}

// store is the DPAPI-sealed credential file.
type store struct {
	mu       sync.RWMutex
	path     string
	creds    credentials
	loaded   bool
	sealKind string
}

func newStore(dir string) (*store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("minimax: plugin data dir is empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("minimax: create data dir: %w", err)
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
	s.loaded = true
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("minimax: read %s: %w", accountsFile, err)
	}
	if len(data) == 0 || len(data) > maxStoreSize {
		return fmt.Errorf("minimax: %s has an unexpected size (%d bytes)", accountsFile, len(data))
	}
	plain, err := open(data)
	if err != nil {
		return fmt.Errorf("minimax: unseal %s: %w", accountsFile, err)
	}
	var c credentials
	if err := json.Unmarshal(plain, &c); err != nil {
		return fmt.Errorf("minimax: parse %s: %w", accountsFile, err)
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

// expired reports whether the stored access token is past its expiry. A token
// with no readable expiry is treated as usable, matching isMinimaxExpired.
func (s *store) expired() bool {
	c, ok := s.get()
	if !ok {
		return false
	}
	exp, known := c.expiresAtMs()
	return known && time.Now().UnixMilli() >= exp
}

// set replaces the credentials atomically. An empty access token is rejected:
// writing it would silently wipe a working login.
func (s *store) set(c credentials) error {
	c.AccessToken = strings.TrimSpace(c.AccessToken)
	c.RefreshToken = strings.TrimSpace(c.RefreshToken)
	c.ExpiresAt = strings.TrimSpace(c.ExpiresAt)
	c.Nickname = strings.TrimSpace(c.Nickname)
	c.BaseURL = strings.TrimSpace(c.BaseURL)
	c.AccountBaseURL = strings.TrimSpace(c.AccountBaseURL)
	if c.AccessToken == "" {
		return errors.New("minimax: refusing to store an empty access token")
	}
	if c.AccountID == "" {
		c.AccountID = jwtSub(c.AccessToken)
	}
	if c.TokenType == "" {
		c.TokenType = "Bearer"
	}
	plain, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("minimax: encode credentials: %w", err)
	}
	sealed, err := seal(plain)
	if err != nil {
		return fmt.Errorf("minimax: seal credentials: %w", err)
	}
	if err := writeFileAtomic(s.path, sealed, 0o600); err != nil {
		return fmt.Errorf("minimax: write %s: %w", accountsFile, err)
	}
	s.mu.Lock()
	s.creds = c
	s.mu.Unlock()
	return nil
}

// grantToCredentials converts a token grant into stored credentials, computing
// expires_at from the JWT exp when it is readable and otherwise from expires_in.
func grantToCredentials(g tokenGrant, previous credentials) credentials {
	c := credentials{
		AccessToken:    g.AccessToken,
		RefreshToken:   firstNonEmpty(g.RefreshToken, previous.RefreshToken),
		TokenType:      firstNonEmpty(g.TokenType, "Bearer"),
		Scope:          g.Scope,
		AccountID:      jwtSub(g.AccessToken),
		Nickname:       previous.Nickname,
		BaseURL:        previous.BaseURL,
		AccountBaseURL: previous.AccountBaseURL,
	}
	expMs, ok := jwtExpMs(g.AccessToken)
	if !ok {
		ttl := g.ExpiresIn
		if ttl <= 0 {
			ttl = 3600
		}
		expMs = time.Now().Add(time.Duration(ttl) * time.Second).UnixMilli()
	}
	c.ExpiresAt = strconv.FormatInt(expMs, 10)
	return c
}

// clear removes the stored credentials.
func (s *store) clear() error {
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("minimax: remove %s: %w", accountsFile, err)
	}
	s.mu.Lock()
	s.creds = credentials{}
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
