package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// v2UserAgent is the UA shape the upstream accepts:
// opencode/<channel>/<major>.<minor>.<patch>/<client>. Ported from the
// reference's OPENCODE_V2_UA, which is what an operator override is checked
// against before it is honoured.
var v2UserAgent = regexp.MustCompile(`^opencode/[a-z0-9-]+/(\d+)\.\d+\.\d+/[a-z0-9-]+$`)

// The plugin owns its credential. It is sealed with DPAPI under the current
// Windows user (dpapi_windows.go) and stored in DATA_DIR\accounts.dat. The host
// never passes a key in through the environment.
//
// Only one credential is stored. The free tier works anonymously with the
// literal key `public`, so a freshly installed plugin is immediately usable and
// an operator only stores something here to reach the paid models with a real
// sk- key.
const (
	accountsFile = "accounts.dat"
	storeVersion = 1
	maxStoreSize = 1 << 20
)

// credentials is the single OpenCode identity.
type credentials struct {
	// APIKey is the bearer value. `public` means the anonymous free tier.
	APIKey string `json:"api_key"`
	// Label is a human tag, never a secret.
	Label string `json:"label,omitempty"`
	// Generation rotates the project fingerprint without changing the key, so a
	// caller can present a fresh device identity (reference: nextFingerprintGeneration).
	Generation int `json:"generation"`
	// UserAgent overrides the v2-shaped UA. Only a v2-shaped value is honoured
	// (see userAgentForVersion); anything else falls back to the default.
	UserAgent string `json:"user_agent,omitempty"`
	// UpdatedAt records when the credential was last written.
	UpdatedAt string `json:"updated_at,omitempty"`
}

func (c credentials) empty() bool { return strings.TrimSpace(c.APIKey) == "" }

// effectiveKey returns the key actually sent upstream, defaulting to the
// anonymous literal so an unconfigured plugin still works.
func (c credentials) effectiveKey() string {
	if key := strings.TrimSpace(c.APIKey); key != "" {
		return key
	}
	return anonymousKey
}

// anonymous reports whether this credential is the shared free-tier literal.
func (c credentials) anonymous() bool { return c.effectiveKey() == anonymousKey }

// effectiveUserAgent returns the override when it is v2-shaped, else the default.
func (c credentials) effectiveUserAgent() string {
	if ua := strings.TrimSpace(c.UserAgent); ua != "" && v2UserAgent.MatchString(ua) {
		return ua
	}
	return defaultUserAgent
}

// projectIDFor derives this credential's x-opencode-project value.
func (c credentials) projectIDFor() string {
	identity := c.effectiveKey()
	if c.anonymous() {
		// The reference keys the fingerprint on the account id for an anonymous
		// slot, so two anonymous installs do not share one fingerprint. With a
		// single implicit slot the plugin id is that stable identity.
		identity = "opencode-anonymous"
	}
	return projectID(identity, c.Generation)
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
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, errors.New("opencode: plugin data dir is empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("opencode: create data dir: %w", err)
	}
	s := &store{path: filepath.Join(dir, accountsFile), sealKind: sealKind()}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// load reads the sealed file. A missing file is not an error: the plugin falls
// back to the anonymous key.
func (s *store) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loaded = true
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("opencode: read %s: %w", accountsFile, err)
	}
	if len(data) == 0 || len(data) > maxStoreSize {
		return fmt.Errorf("opencode: %s has an unexpected size (%d bytes)", accountsFile, len(data))
	}
	plain, err := open(data)
	if err != nil {
		return fmt.Errorf("opencode: unseal %s: %w", accountsFile, err)
	}
	var c credentials
	if err := json.Unmarshal(plain, &c); err != nil {
		return fmt.Errorf("opencode: parse %s: %w", accountsFile, err)
	}
	s.creds = c
	return nil
}

// get returns the current credential and whether one was stored.
func (s *store) get() (credentials, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.creds, !s.creds.empty()
}

// current returns the credential to use, synthesising the anonymous one when
// nothing is stored. This is why the plugin works without any login.
func (s *store) current() credentials {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.creds.empty() {
		return credentials{APIKey: anonymousKey, Label: "匿名通道"}
	}
	return s.creds
}

// configured reports whether a real (non-anonymous) key is stored.
func (s *store) configured() bool {
	c, ok := s.get()
	return ok && !c.anonymous()
}

// set replaces the credential. An empty key is rejected: writing it would
// silently wipe a working login. Storage of the anonymous literal is allowed
// (it is a real, official value) but is not treated as configured.
func (s *store) set(c credentials) error {
	c.APIKey = strings.TrimSpace(c.APIKey)
	c.Label = strings.TrimSpace(c.Label)
	c.UserAgent = strings.TrimSpace(c.UserAgent)
	if c.APIKey == "" {
		return errors.New("opencode: refusing to store an empty api key")
	}
	if c.Generation < 0 {
		c.Generation = 0
	}
	c.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	plain, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("opencode: encode credentials: %w", err)
	}
	sealed, err := seal(plain)
	if err != nil {
		return fmt.Errorf("opencode: seal credentials: %w", err)
	}
	if err := writeFileAtomic(s.path, sealed, 0o600); err != nil {
		return fmt.Errorf("opencode: write %s: %w", accountsFile, err)
	}
	s.mu.Lock()
	s.creds = c
	s.mu.Unlock()
	return nil
}

// rotateFingerprint bumps the generation so the next request presents a new
// project identity while keeping the same key.
func (s *store) rotateFingerprint() (int, error) {
	c := s.current()
	c.Generation++
	if c.APIKey == "" {
		return 0, errors.New("opencode: no credential to rotate")
	}
	if err := s.set(c); err != nil {
		return 0, err
	}
	return c.Generation, nil
}

// clear removes the stored credential, returning the plugin to anonymous use.
func (s *store) clear() error {
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("opencode: remove %s: %w", accountsFile, err)
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
