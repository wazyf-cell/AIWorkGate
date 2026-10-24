package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const stateFileName = "state.json"

// Status values allowed by the host contract (§17.1).
const (
	statusOK          = "ok"
	statusExpired     = "expired"
	statusInvalid     = "invalid"
	statusRateLimited = "rate_limited"
	statusUnknown     = "unknown"
)

// State is the plugin state.json. It never contains platform credentials.
type State struct {
	Version    int        `json:"version"`
	Accounts   []*Account `json:"accounts"`
	RuleHits   int64      `json:"rule_hits"`
	StripHits  int64      `json:"strip_hits"`
	LastReload int64      `json:"last_reload_at"`
	RulesCount int        `json:"last_reload_rules"`
	StripCount int        `json:"last_reload_strip"`
	EgressAccs int        `json:"last_reload_egress_accounts"`
}

func loadState(dir string) (*State, error) {
	path := filepath.Join(dir, stateFileName)
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &State{Version: 1, Accounts: []*Account{}}, nil
		}
		return nil, fmt.Errorf("read state: %w", err)
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if st.Accounts == nil {
		st.Accounts = []*Account{}
	}
	if st.Version == 0 {
		st.Version = 1
	}
	return &st, nil
}

// save writes state.json atomically. Callers must not hold p.mu.
func (p *Plugin) save() error {
	p.saveMu.Lock()
	defer p.saveMu.Unlock()
	p.mu.RLock()
	b, err := json.MarshalIndent(p.state, "", "  ")
	p.mu.RUnlock()
	if err != nil {
		return err
	}
	tmp := filepath.Join(p.DataDir, stateFileName+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(p.DataDir, stateFileName)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func hashHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// accountIDFor derives a stable account id from the OfficeAce user identity.
func accountIDFor(userKey string) string {
	return "acc_" + hashHex("officeace:" + userKey)[:12]
}

// labelFor prefers a human name/email from the session, otherwise a masked id.
func labelFor(userName, displayName, userID string) string {
	for _, s := range []string{strings.TrimSpace(userName), strings.TrimSpace(displayName)} {
		if s != "" && !strings.EqualFold(s, "unknown") {
			return s
		}
	}
	return "officeace-" + maskID(userID)
}

func maskID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return "unknown"
	}
	if len(id) <= 8 {
		return id[:1] + "***"
	}
	return id[:4] + "…" + id[len(id)-4:]
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		for i := range b {
			b[i] = byte(i*31 + 7)
		}
	}
	return hex.EncodeToString(b)
}

func (p *Plugin) accountByIDLocked(id string) *Account {
	for _, a := range p.state.Accounts {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// ensureAccount registers/updates the single account derived from the newest
// session. A different OfficeAce user replaces the previous one (single-account
// plugin), while the same user keeps check-in bookkeeping stable. The bool
// result reports whether persistent state should be saved.
func (p *Plugin) ensureAccount(c *Credential, now time.Time) (Account, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	userKey := c.UserKey()
	id := accountIDFor(userKey)
	if existing := p.accountByIDLocked(id); existing != nil {
		changed := false
		if label := labelFor(c.UserName, c.DisplayName, c.UserID); label != existing.Label {
			existing.Label = label
			changed = true
		}
		if existing.UserID != c.UserID || existing.AccountID != c.AccountID {
			existing.UserID = c.UserID
			existing.AccountID = c.AccountID
			changed = true
		}
		if c.SessionExpiresAt.Unix() > 0 && existing.ExpiresAt != c.SessionExpiresAt.Unix() {
			existing.ExpiresAt = c.SessionExpiresAt.Unix()
			changed = true
		}
		existing.LastSeenAt = now.Unix()
		if existing.Status == "" {
			existing.Status = statusOK
			changed = true
		}
		return *existing, changed
	}
	a := &Account{
		ID:         id,
		Label:      labelFor(c.UserName, c.DisplayName, c.UserID),
		UserID:     c.UserID,
		AccountID:  c.AccountID,
		Enabled:    true,
		Status:     statusOK,
		AddedAt:    now.Unix(),
		LastSeenAt: now.Unix(),
		ExpiresAt:  c.SessionExpiresAt.Unix(),
	}
	if len(p.state.Accounts) > 0 {
		p.logLine("info", "officeace user changed; replacing single account", map[string]any{
			"old": p.state.Accounts[0].ID,
			"new": a.ID,
		})
	}
	p.state.Accounts = []*Account{a}
	return *a, true
}

func (p *Plugin) setAccountStatus(accountID, status, lastError string) {
	if accountID == "" {
		return
	}
	changed := false
	p.mu.Lock()
	if a := p.accountByIDLocked(accountID); a != nil {
		if a.Status != status || a.LastError != lastError {
			a.Status = status
			a.LastError = lastError
			changed = true
		}
	}
	p.mu.Unlock()
	if changed {
		if err := p.save(); err != nil {
			p.logLine("warn", "save state failed", map[string]any{"error": err.Error()})
		}
	}
}

func (p *Plugin) recordCheckin(accountID, result string, at int64) {
	p.mu.Lock()
	if a := p.accountByIDLocked(accountID); a != nil {
		a.CheckedInAt = at
		a.CheckinResult = result
	}
	p.mu.Unlock()
	if err := p.save(); err != nil {
		p.logLine("warn", "save state failed", map[string]any{"error": err.Error()})
	}
}

func (p *Plugin) findAccount(id string) (Account, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if a := p.accountByIDLocked(id); a != nil {
		return *a, true
	}
	return Account{}, false
}

func (p *Plugin) accountCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.state.Accounts)
}
