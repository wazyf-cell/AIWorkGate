package main

// Account-level egress for OpenCode. The host writes runtime\opencode\egress.json
// (path in AIWORKGATE_EGRESS_FILE) and calls POST /reload after a change. The
// file never contains proxy credentials, only the loopback forwarder URL.
//
// Why this plugin needs it at all: the free tier decides region server-side, so
// every request from a CN egress address comes back region-blocked or throttled.
// Routing through the host's forwarder is the only way to reach the same upstream
// from elsewhere; there is no request-shape trick that substitutes for it.
//
// The contract rule is fail-closed: a "blocked" binding, or "direct" under
// strict mode, must stop the request instead of silently connecting directly.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strings"
)

const (
	egressDirect  = "direct"
	egressProxy   = "proxy"
	egressBlocked = "blocked"
	// egressAccountID is the single account this plugin exposes (the anonymous
	// lane). A stored sk- key is the same account with a different credential,
	// not a second row, so one binding covers both.
	egressAccountID = "default"
)

// egressEntry is one account -> local forwarder binding.
type egressEntry struct {
	Mode    string `json:"mode"`
	URL     string `json:"url,omitempty"`
	ProxyID string `json:"proxy_id,omitempty"`
}

// egressDoc is the host-written egress.json.
type egressDoc struct {
	Version  int                    `json:"version"`
	Strict   bool                   `json:"strict"`
	Default  egressEntry            `json:"default"`
	Accounts map[string]egressEntry `json:"accounts"`
}

var errEgressBlocked = errors.New("egress_missing: this account has no usable egress route")

// loadEgress reads the binding file. A missing file is the normal case: the
// plugin then connects directly, exactly as it did before egress existed.
func loadEgress(path string) (egressDoc, error) {
	doc := egressDoc{
		Version:  1,
		Default:  egressEntry{Mode: egressDirect},
		Accounts: map[string]egressEntry{},
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return doc, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return doc, nil
		}
		return doc, fmt.Errorf("opencode: read egress file: %w", err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return doc, fmt.Errorf("opencode: parse egress file: %w", err)
	}
	if doc.Accounts == nil {
		doc.Accounts = map[string]egressEntry{}
	}
	return doc, nil
}

// resolve returns the binding for one account and enforces strict mode.
func (d egressDoc) resolve(accountID string) (egressEntry, error) {
	entry, ok := d.Accounts[accountID]
	if !ok {
		entry = d.Default
	}
	mode := strings.ToLower(strings.TrimSpace(entry.Mode))
	if mode == "" {
		mode = egressDirect
	}
	entry.Mode = mode
	switch mode {
	case egressProxy:
		raw := strings.TrimSpace(entry.URL)
		if raw == "" {
			return entry, fmt.Errorf("%w (mode proxy without url)", errEgressBlocked)
		}
		u, err := url.Parse(raw)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return entry, fmt.Errorf("%w (invalid proxy url)", errEgressBlocked)
		}
		return entry, nil
	case egressDirect:
		if d.Strict {
			return entry, fmt.Errorf("%w (strict mode forbids direct)", errEgressBlocked)
		}
		return entry, nil
	case egressBlocked:
		return entry, errEgressBlocked
	}
	return entry, fmt.Errorf("%w (unsupported mode %q)", errEgressBlocked, mode)
}

// reloadEgress re-reads the file and re-points the upstream client. A blocked
// binding is not an error here: it is a valid state that fails requests closed.
func (s *server) reloadEgress() error {
	doc, err := loadEgress(s.cfg.EgressFile)
	if err != nil {
		return err
	}
	s.egressMu.Lock()
	s.egress = doc
	s.egressMu.Unlock()
	entry, resolveErr := doc.resolve(egressAccountID)
	if resolveErr != nil {
		s.client.setEgress("", resolveErr)
		return nil
	}
	if entry.Mode == egressProxy {
		s.client.setEgress(entry.URL, nil)
	} else {
		s.client.setEgress("", nil)
	}
	return nil
}

// egressError reports why this account may not call upstream, if it may not.
func (s *server) egressError() error {
	s.egressMu.RLock()
	doc := s.egress
	s.egressMu.RUnlock()
	_, err := doc.resolve(egressAccountID)
	return err
}

// handleReload re-reads the binding file after the host rewrites it. It also
// drops the capability table so the next model listing refetches it, which is
// what this endpoint did before egress existed.
func (s *server) handleReload(w http.ResponseWriter, r *http.Request) {
	s.caps.Set(nil)
	if err := s.reloadEgress(); err != nil {
		writeError(w, http.StatusInternalServerError, "egress_error", err.Error())
		return
	}
	body := map[string]any{"ok": true, "rules": 0, "egress": 1}
	if err := s.egressError(); err != nil {
		body["egress_blocked"] = true
		body["egress_error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, body)
}
