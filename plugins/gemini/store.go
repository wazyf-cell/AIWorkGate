package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The plugin owns its credentials. They are sealed with DPAPI under the current
// Windows user (see dpapi_windows.go) and stored in DATA_DIR\accounts.dat. The
// host never passes a token in through the environment, and the page never sees a
// plaintext secret: this is the same rule the zcode and aichat plugins follow.
const (
	accountsFile = "accounts.dat"
	maxStoreSize = 1 << 20
)

// credentials is one Gemini login, shaped like the reference's credential object
// (parseGeminiCredential) so a value exported from the reference can be pasted in
// unchanged.
type credentials struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	TokenType    string `json:"token_type,omitempty"`
	ExpiresIn    int    `json:"expires_in,omitempty"`
	Scope        string `json:"scope,omitempty"`
	// Expiry is an ISO-8601 timestamp, as the reference writes it.
	Expiry string `json:"expiry,omitempty"`
	Sub    string `json:"sub,omitempty"`
	Email  string `json:"email,omitempty"`
	// CloudaicompanionProject is discovered by loadCodeAssist and cached here.
	CloudaicompanionProject string `json:"cloudaicompanionProject,omitempty"`
	// Nickname is a human tag, never a secret.
	Nickname string `json:"nickname,omitempty"`
	// EndpointBase lets a test or a mirror redirect the inference origin.
	EndpointBase string `json:"endpoint_base,omitempty"`
	// Identity is the plugin-owned device/session identity (see identity.go).
	Identity *identityState `json:"identity,omitempty"`
}

func (c credentials) empty() bool { return strings.TrimSpace(c.AccessToken) == "" }

func (c credentials) refreshable() bool { return strings.TrimSpace(c.RefreshToken) != "" }

// label is geminiAccountLabel: email, then sub, then a fixed fallback.
func (c credentials) label() string {
	switch {
	case strings.TrimSpace(c.Email) != "":
		return c.Email
	case strings.TrimSpace(c.Sub) != "":
		return c.Sub
	}
	return "Gemini"
}

// expiresAtMs parses the ISO expiry. An unparseable or absent expiry is treated
// as "unknown", which isGeminiExpired also treats as not expired.
func (c credentials) expiresAtMs() (int64, bool) {
	if strings.TrimSpace(c.Expiry) == "" {
		return 0, false
	}
	t, err := time.Parse(time.RFC3339, c.Expiry)
	if err != nil {
		return 0, false
	}
	return t.UnixMilli(), true
}

// nearExpiry applies the reference's 60 s lead time, so a token that is about to
// die is refreshed before it is used.
func (c credentials) nearExpiry(now time.Time) bool {
	exp, known := c.expiresAtMs()
	if !known {
		return false
	}
	return exp-now.UnixMilli() <= expiryLeadTime.Milliseconds()
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
		return nil, errors.New("gemini: plugin data dir is empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("gemini: create data dir: %w", err)
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
	s.loaded = true
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("gemini: read %s: %w", accountsFile, err)
	}
	if len(data) == 0 || len(data) > maxStoreSize {
		return fmt.Errorf("gemini: %s has an unexpected size (%d bytes)", accountsFile, len(data))
	}
	plain, err := open(data)
	if err != nil {
		return fmt.Errorf("gemini: unseal %s: %w", accountsFile, err)
	}
	var c credentials
	if err := json.Unmarshal(plain, &c); err != nil {
		return fmt.Errorf("gemini: parse %s: %w", accountsFile, err)
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

// set replaces the credentials atomically. An empty access token is rejected:
// writing it would silently wipe a working login.
func (s *store) set(c credentials) error {
	c.AccessToken = strings.TrimSpace(c.AccessToken)
	c.RefreshToken = strings.TrimSpace(c.RefreshToken)
	c.Nickname = strings.TrimSpace(c.Nickname)
	c.EndpointBase = strings.TrimSpace(c.EndpointBase)
	if c.AccessToken == "" {
		return errors.New("gemini: refusing to store an empty access token")
	}
	if c.TokenType == "" {
		c.TokenType = "Bearer"
	}
	plain, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("gemini: encode credentials: %w", err)
	}
	sealed, err := seal(plain)
	if err != nil {
		return fmt.Errorf("gemini: seal credentials: %w", err)
	}
	if err := writeFileAtomic(s.path, sealed, 0o600); err != nil {
		return fmt.Errorf("gemini: write %s: %w", accountsFile, err)
	}
	s.mu.Lock()
	s.creds = c
	s.mu.Unlock()
	return nil
}

// grantToCredentials is credentialFromGeminiGrant: expiry from expires_in, identity
// from the id_token when present, and the discovered project carried forward.
func grantToCredentials(g tokenGrant, previous credentials) credentials {
	c := credentials{
		AccessToken:             g.AccessToken,
		RefreshToken:            firstNonEmpty(g.RefreshToken, previous.RefreshToken),
		TokenType:               firstNonEmpty(g.TokenType, "Bearer"),
		ExpiresIn:               g.ExpiresIn,
		Scope:                   firstNonEmpty(g.Scope, previous.Scope),
		Expiry:                  time.Now().Add(time.Duration(g.ExpiresIn) * time.Second).UTC().Format(time.RFC3339),
		Sub:                     previous.Sub,
		Email:                   previous.Email,
		CloudaicompanionProject: previous.CloudaicompanionProject,
		Nickname:                previous.Nickname,
		EndpointBase:            previous.EndpointBase,
		Identity:                previous.Identity,
	}
	if sub, email := identityFromIDToken(g.IDToken); sub != "" || email != "" {
		c.Sub = firstNonEmpty(sub, previous.Sub)
		c.Email = firstNonEmpty(email, previous.Email)
	}
	return c
}

// identityFromIDToken reads sub/email out of an id_token without verifying it.
// The reference does the same (geminiIdentityFromIdToken): the token was just
// received from Google over TLS, so signature verification adds nothing here.
func identityFromIDToken(idToken string) (sub, email string) {
	if idToken == "" {
		return "", ""
	}
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return "", ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return "", ""
	}
	var claims struct {
		Sub   string `json:"sub"`
		Email string `json:"email"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return "", ""
	}
	return claims.Sub, claims.Email
}

func (s *store) clear() error {
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("gemini: remove %s: %w", accountsFile, err)
	}
	s.mu.Lock()
	s.creds = credentials{}
	s.mu.Unlock()
	return nil
}

// writeFileAtomic writes to a temp file in the same directory and renames it.
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
