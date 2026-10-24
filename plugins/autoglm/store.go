package main

// Credential storage.
//
// The reference keeps one JSON file per account (auths/autoclaw-<uid>.json) in
// the clear, because it is a server-side panel. A plugin that runs on the
// operator's own machine must not do that: this port keeps every account in a
// single DPAPI-sealed blob, DATA_DIR\accounts.dat, so a copied file is useless
// on another machine or user.
//
// Field names inside the sealed JSON stay identical to the reference's Auth
// struct, so an operator who has an autoclaw-<uid>.json can paste it through
// POST /admin/credentials without renaming anything.

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

const (
	accountsFile   = "accounts.dat"
	maxStoreSize   = 4 << 20
	storeVersion   = 1
	regionCN       = "cn"
	regionGlobal   = "global"
	accessTokenTTL = 28 * 86400
)

// accountData is one AutoGLM login, field for field the reference's Auth struct.
// It is a plain value type so it can be sealed and copied freely.
type accountData struct {
	AccessToken     string `json:"accessToken"`
	RefreshToken    string `json:"refreshToken"`
	ExpiresAt       int64  `json:"expiresAt"`
	UserID          string `json:"userId"`
	UserName        string `json:"userName"`
	Phone           string `json:"phone"`
	DeviceID        string `json:"deviceId"`
	Region          string `json:"region"`
	SandboxID       string `json:"sandboxId"`
	SandboxEndpoint string `json:"sandboxEndpoint"`
	EndTimestamp    int64  `json:"endTimestamp"`
}

// account guards one accountData. The lock is kept out of the data struct so a
// snapshot can be copied without dragging a mutex along.
type account struct {
	mu sync.Mutex
	d  accountData
}

// snapshot copies the data out; callers never hold the lock across I/O.
func (a *account) snapshot() accountData {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.d
}

// update mutates the data under the account's lock.
func (a *account) update(fn func(*accountData)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	fn(&a.d)
}

// newAccount wraps a data value in a guarded account.
func newAccount(d accountData) *account { return &account{d: d} }

// fresh reports whether the account carries a usable access token.
func (a *account) fresh() bool {
	d := a.snapshot()
	return strings.TrimSpace(d.AccessToken) != "" && strings.TrimSpace(d.UserID) != ""
}

// needsRefresh reports whether the token expires within `within` seconds.
func (a *account) needsRefresh(within float64) bool {
	d := a.snapshot()
	if d.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(time.Duration(within*float64(time.Second))).Unix() >= d.ExpiresAt
}

// sealBody is the sealed document. Accounts hold credentials; State holds the
// scheduler's per-account bookkeeping (points, cooldowns). Both live in the one
// sealed file on purpose: the reference writes state to a plain JSON file, but a
// plugin must not scatter operator data into the clear.
type sealBody struct {
	Version  int                      `json:"version"`
	Accounts []accountData            `json:"accounts"`
	State    map[string]*accountState `json:"state,omitempty"`
}

// store owns the sealed credential file.
type store struct {
	mu       sync.RWMutex
	path     string
	accounts []*account
	states   map[string]*accountState
	sealed   string
}

func newStore(dir string) (*store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("autoglm: plugin data dir is empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("autoglm: create data dir: %w", err)
	}
	s := &store{
		path:   filepath.Join(dir, accountsFile),
		states: map[string]*accountState{},
		sealed: sealKind(),
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// sealKindName is the /health surface, so an operator can see whether the
// credentials are actually protected on this host.
func (s *store) sealKindName() string { return s.sealed }

// load reads and unseals accounts.dat. A missing file means "no accounts yet",
// which is not an error.
func (s *store) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("autoglm: read %s: %w", accountsFile, err)
	}
	if len(raw) == 0 || len(raw) > maxStoreSize {
		return fmt.Errorf("autoglm: %s has an unexpected size (%d bytes)", accountsFile, len(raw))
	}
	plain, err := open(raw)
	if err != nil {
		return fmt.Errorf("autoglm: unseal %s: %w", accountsFile, err)
	}
	var doc sealBody
	if err := json.Unmarshal(plain, &doc); err != nil {
		return fmt.Errorf("autoglm: parse %s: %w", accountsFile, err)
	}
	for i := range doc.Accounts {
		d := doc.Accounts[i]
		if d.Region == "" {
			d.Region = regionCN
		}
		s.accounts = append(s.accounts, &account{d: d})
	}
	for k, st := range doc.State {
		if st != nil {
			s.states[k] = st
		}
	}
	return nil
}

// list returns a snapshot slice of the stored accounts.
func (s *store) list() []*account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*account, len(s.accounts))
	copy(out, s.accounts)
	return out
}

// byID finds an account by user id.
func (s *store) byID(uid string) *account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, a := range s.accounts {
		if a.snapshot().UserID == uid {
			return a
		}
	}
	return nil
}

// configured reports whether at least one usable account exists.
func (s *store) configured() bool {
	for _, a := range s.list() {
		if a.fresh() {
			return true
		}
	}
	return false
}
