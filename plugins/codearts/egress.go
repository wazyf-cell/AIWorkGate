package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	egressDirect  = "direct"
	egressProxy   = "proxy"
	egressBlocked = "blocked"
)

// EgressEntry is one account -> local forwarder binding (§18.4).
type EgressEntry struct {
	Mode    string `json:"mode"`
	URL     string `json:"url,omitempty"`
	ProxyID string `json:"proxy_id,omitempty"`
}

// EgressFile is the host-written egress.json (no proxy credentials).
type EgressFile struct {
	Version  int                    `json:"version"`
	Strict   bool                   `json:"strict"`
	Default  EgressEntry            `json:"default"`
	Accounts map[string]EgressEntry `json:"accounts"`
}

func loadEgress(path string) (EgressFile, error) {
	ef := EgressFile{
		Version:  1,
		Default:  EgressEntry{Mode: egressDirect},
		Accounts: map[string]EgressEntry{},
	}
	if strings.TrimSpace(path) == "" {
		return ef, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ef, nil
		}
		return ef, fmt.Errorf("read egress: %w", err)
	}
	if err := json.Unmarshal(b, &ef); err != nil {
		return ef, fmt.Errorf("parse egress: %w", err)
	}
	if ef.Accounts == nil {
		ef.Accounts = map[string]EgressEntry{}
	}
	return ef, nil
}

type egressMissingError struct {
	accountID string
	mode      string
	strict    bool
}

func (e *egressMissingError) Error() string {
	if e.strict && e.mode != egressBlocked {
		return fmt.Sprintf("egress_missing: account %s has mode %q but strict mode requires a proxy", e.accountID, e.mode)
	}
	return fmt.Sprintf("egress_missing: account %s has no usable egress proxy (mode %q)", e.accountID, e.mode)
}

// resolveEgress resolves the account binding and enforces strict mode.
func (p *Plugin) resolveEgress(accountID string) (EgressEntry, error) {
	p.egressMu.RLock()
	ef := p.egress
	p.egressMu.RUnlock()

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
		return entry, &egressMissingError{accountID: accountID, mode: mode, strict: ef.Strict}
	}
	if ef.Strict && mode != egressProxy {
		return entry, &egressMissingError{accountID: accountID, mode: mode, strict: true}
	}
	entry.Mode = mode
	return entry, nil
}

// checkEgress writes the contract error and returns false when the account has
// no usable egress route.
func (p *Plugin) checkEgress(w http.ResponseWriter, accountID string) bool {
	_, err := p.resolveEgress(accountID)
	if err == nil {
		return true
	}
	var em *egressMissingError
	if errors.As(err, &em) {
		writeError(w, http.StatusForbidden, "egress_missing", em.Error())
		return false
	}
	writeError(w, http.StatusInternalServerError, "egress_error", err.Error())
	return false
}

// transportFor returns the cached transport for an account. A proxy binding
// always wins; no direct fallback is attempted when the proxy fails.
func (p *Plugin) transportFor(accountID, proxyURL string) *http.Transport {
	p.trMu.Lock()
	defer p.trMu.Unlock()
	if e, ok := p.transports[accountID]; ok {
		if e.proxyURL == proxyURL {
			return e.tr
		}
		e.tr.CloseIdleConnections()
	}
	tr := &http.Transport{
		MaxIdleConns:          4,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 2 * time.Minute,
		ForceAttemptHTTP2:     true,
	}
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	p.transports[accountID] = &transportEntry{proxyURL: proxyURL, tr: tr}
	return tr
}

func (p *Plugin) resetTransports() {
	p.trMu.Lock()
	defer p.trMu.Unlock()
	for k, e := range p.transports {
		e.tr.CloseIdleConnections()
		delete(p.transports, k)
	}
}

func (p *Plugin) dropTransport(accountID string) {
	p.trMu.Lock()
	defer p.trMu.Unlock()
	if e, ok := p.transports[accountID]; ok {
		e.tr.CloseIdleConnections()
		delete(p.transports, accountID)
	}
}
