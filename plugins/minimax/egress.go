package main

// Account-level egress for MiniMax. The host writes runtime\minimax\egress.json
// (path in AIWORKGATE_EGRESS_FILE) and calls POST /reload after a change. The
// file carries no proxy credentials, only the loopback forwarder URL.
//
// Fail-closed: "blocked", or "direct" under strict mode, must stop the request.
// A proxy failure is never retried as a direct connection.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strings"
)

const (
	egressDirect  = "direct"
	egressProxy   = "proxy"
	egressBlocked = "blocked"
	// egressAccountID is the single account this plugin exposes.
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

// loadEgress reads the binding file. A missing file means direct, the behaviour
// before egress existed.
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
		return doc, fmt.Errorf("minimax: read egress file: %w", err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return doc, fmt.Errorf("minimax: parse egress file: %w", err)
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
// binding is a valid state, not a reload failure.
func (s *server) reloadEgress() error {
	doc, err := loadEgress(s.egressPath)
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
