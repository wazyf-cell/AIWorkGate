package main

// Egress bindings (host-written egress.json).
//
// The host materialises runtime/<plugin>/egress.json and passes its path in
// AIWORKGATE_EGRESS_FILE:
//
//	{"version":1,"strict":false,"default":{"mode":"direct"},
//	 "accounts":{"cn/u1":{"mode":"proxy","url":"http://127.0.0.1:7890"}}}
//
// Account ids are exactly the ids GET /accounts reports ("cn/<uid>" or
// "global/<uid>"). Under strict mode a direct/bare entry is refused instead of
// leaking the request out of the unproxied interface. Requests that are not
// account-scoped (the login handshakes) use the default entry.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	egressDirect  = "direct"
	egressProxy   = "proxy"
	egressBlocked = "blocked"
)

// egressEntry is one account -> local forwarder binding.
type egressEntry struct {
	Mode    string `json:"mode"`
	URL     string `json:"url,omitempty"`
	ProxyID string `json:"proxy_id,omitempty"`
}

// egressFile is the host-written document.
type egressFile struct {
	Version  int                    `json:"version"`
	Strict   bool                   `json:"strict"`
	Default  egressEntry            `json:"default"`
	Accounts map[string]egressEntry `json:"accounts"`
}

func loadEgressFile(path string) (egressFile, error) {
	ef := egressFile{Version: 1, Default: egressEntry{Mode: egressDirect}, Accounts: map[string]egressEntry{}}
	if strings.TrimSpace(path) == "" {
		return ef, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ef, nil
		}
		return ef, fmt.Errorf("autoglm: read egress file: %w", err)
	}
	if err := json.Unmarshal(raw, &ef); err != nil {
		return ef, fmt.Errorf("autoglm: parse egress file: %w", err)
	}
	if ef.Accounts == nil {
		ef.Accounts = map[string]egressEntry{}
	}
	return ef, nil
}

// applyEgress re-reads path and installs it on the upstream. Startup and
// /reload share this one path; a load failure still installs the default direct
// entry, so a missing or broken file never stops the plugin.
func (u *upstream) applyEgress(path string) (egressFile, error) {
	ef, err := loadEgressFile(path)
	u.setEgress(ef)
	return ef, err
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

func resolveEgress(ef egressFile, accountID string) (egressEntry, error) {
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

// egressRouter caches one transport per (account, proxy URL).
type egressRouter struct {
	mu      sync.Mutex
	file    egressFile
	clients map[string]*http.Client
	timeout time.Duration
}

func newEgressRouter(timeout time.Duration) *egressRouter {
	return &egressRouter{file: egressFile{Default: egressEntry{Mode: egressDirect}}, clients: map[string]*http.Client{}, timeout: timeout}
}

func (r *egressRouter) set(ef egressFile) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.clients {
		if tr, ok := c.Transport.(*http.Transport); ok {
			tr.CloseIdleConnections()
		}
	}
	r.clients = map[string]*http.Client{}
	r.file = ef
}

func (r *egressRouter) client(accountID string) (*http.Client, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, err := resolveEgress(r.file, accountID)
	if err != nil {
		return nil, err
	}
	proxyURL := ""
	if entry.Mode == egressProxy {
		proxyURL = entry.URL
	}
	key := accountID + "|" + proxyURL
	if c, ok := r.clients[key]; ok {
		return c, nil
	}
	tr := &http.Transport{
		MaxIdleConns:          4,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 2 * time.Minute,
		ForceAttemptHTTP2:     true,
	}
	if proxyURL != "" {
		u, perr := url.Parse(proxyURL)
		if perr != nil {
			return nil, fmt.Errorf("autoglm: account %s has an unparseable egress url: %w", accountID, perr)
		}
		tr.Proxy = http.ProxyURL(u)
	}
	c := &http.Client{Timeout: r.timeout, Transport: tr}
	r.clients[key] = c
	return c, nil
}
