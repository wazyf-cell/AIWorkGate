// Package main — account-level egress for Raccoon.
//
// The host writes runtime\raccoon\egress.json and points AIWORKGATE_EGRESS_FILE
// at it:
//
//	{"version":1,"strict":false,"default":{"mode":"direct"},
//	 "accounts":{"default":{"mode":"proxy","url":"http://127.0.0.1:PORT"}}}
//
// mode "proxy" routes every upstream call through entry.url; "direct" and
// "blocked" are honoured, and strict mode refuses a non-proxy account instead
// of silently going direct (contract §18.4).
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strings"
)

const (
	egressDirect  = "direct"
	egressProxy   = "proxy"
	egressBlocked = "blocked"
)

// egressEntry is one account -> local forwarder binding.
type egressEntry struct {
	Mode string `json:"mode"`
	URL  string `json:"url,omitempty"`
}

// egressFile is the host-written egress.json (no proxy credentials inside).
type egressFile struct {
	Version  int                    `json:"version"`
	Strict   bool                   `json:"strict"`
	Default  egressEntry            `json:"default"`
	Accounts map[string]egressEntry `json:"accounts"`
}

func loadEgress(path string) (egressFile, error) {
	ef := egressFile{Version: 1, Default: egressEntry{Mode: egressDirect}, Accounts: map[string]egressEntry{}}
	if strings.TrimSpace(path) == "" {
		return ef, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ef, nil
		}
		return ef, fmt.Errorf("raccoon: read egress: %w", err)
	}
	if err := json.Unmarshal(raw, &ef); err != nil {
		return ef, fmt.Errorf("raccoon: parse egress: %w", err)
	}
	if ef.Accounts == nil {
		ef.Accounts = map[string]egressEntry{}
	}
	return ef, nil
}

type egressError struct {
	accountID string
	mode      string
	strict    bool
}

func (e *egressError) Error() string {
	if e.strict && e.mode != egressBlocked {
		return fmt.Sprintf("egress_missing: 账号 %s 是 %q 但严格出网要求绑定代理", e.accountID, e.mode)
	}
	return fmt.Sprintf("egress_missing: 账号 %s 没有可用的出口代理（mode=%q）", e.accountID, e.mode)
}

// resolveEgress returns the binding for accountID and enforces strict mode.
func (s *server) resolveEgress(accountID string) (egressEntry, error) {
	s.egressMu.RLock()
	ef := s.egress
	s.egressMu.RUnlock()

	entry, ok := ef.Accounts[accountID]
	if !ok {
		entry = ef.Default
	}
	mode := strings.TrimSpace(entry.Mode)
	if mode == "" {
		mode = egressDirect
	}
	if mode == egressProxy && strings.TrimSpace(entry.URL) == "" {
		mode = egressBlocked
	}
	if mode == egressBlocked {
		return entry, &egressError{accountID: accountID, mode: mode, strict: ef.Strict}
	}
	if ef.Strict && mode != egressProxy {
		return entry, &egressError{accountID: accountID, mode: mode, strict: true}
	}
	entry.Mode = mode
	return entry, nil
}

// proxyFor is the convenience form used by the request handlers: the proxy URL
// to use, or an error that has already been shaped for the wire.
func (s *server) proxyFor(w http.ResponseWriter, accountID string) (string, bool) {
	entry, err := s.resolveEgress(accountID)
	if err == nil {
		if entry.Mode == egressProxy {
			return strings.TrimSpace(entry.URL), true
		}
		return "", true
	}
	var ee *egressError
	if errors.As(err, &ee) {
		writeError(w, http.StatusForbidden, "egress_missing", ee.Error())
		return "", false
	}
	writeError(w, http.StatusInternalServerError, "egress_error", err.Error())
	return "", false
}

// reloadEgress re-reads the file and drops cached transports (POST /reload).
func (s *server) reloadEgress() (int, error) {
	ef, err := loadEgress(s.egressPath)
	if err != nil {
		return 0, err
	}
	s.egressMu.Lock()
	s.egress = ef
	s.egressMu.Unlock()
	s.client.resetClients()
	return len(ef.Accounts), nil
}
