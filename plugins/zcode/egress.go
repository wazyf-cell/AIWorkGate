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
	"sync"
	"time"
)

// Egress modes, matching the host's model.Egress* constants.
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

// egressFile is the host-written egress.json (no proxy credentials).
type egressFile struct {
	Version  int                    `json:"version"`
	Strict   bool                   `json:"strict"`
	Default  egressEntry            `json:"default"`
	Accounts map[string]egressEntry `json:"accounts"`
}

// egressState is the cached file plus the per-proxy transports.
type egressState struct {
	mu         sync.RWMutex
	file       egressFile
	path       string
	trMu       sync.Mutex
	transports map[string]*http.Transport
}

func emptyEgress() egressFile {
	return egressFile{Version: 1, Default: egressEntry{Mode: egressDirect}, Accounts: map[string]egressEntry{}}
}

// loadEgress reads the host's file. A missing file means "direct everywhere".
func loadEgress(path string) (egressFile, error) {
	ef := emptyEgress()
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
		ef.Accounts = map[string]egressEntry{}
	}
	return ef, nil
}

// reloadEgress re-reads the file named by AIWORKGATE_EGRESS_FILE.
func (s *server) reloadEgress() error {
	path := s.cfg.Egress
	ef, err := loadEgress(path)
	if err != nil {
		return err
	}
	s.egress.mu.Lock()
	s.egress.file = ef
	s.egress.path = path
	s.egress.mu.Unlock()
	return nil
}

// egressMissingError reports an account that strict mode will not serve direct.
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

// resolveEgress returns the binding for one account and enforces strict mode.
func (s *server) resolveEgress(accountID string) (egressEntry, error) {
	s.egress.mu.RLock()
	ef := s.egress.file
	s.egress.mu.RUnlock()

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

// proxyFor is resolveEgress reduced to the proxy URL ("" means direct).
func (s *server) proxyFor(accountID string) (string, error) {
	entry, err := s.resolveEgress(accountID)
	if err != nil {
		return "", err
	}
	if entry.Mode == egressProxy {
		return strings.TrimSpace(entry.URL), nil
	}
	return "", nil
}

// transportFor caches one transport per proxy URL and closes the previous
// idle connections when the binding changes.
func (s *server) transportFor(proxyURL string) *http.Transport {
	s.egress.trMu.Lock()
	defer s.egress.trMu.Unlock()
	if s.egress.transports == nil {
		s.egress.transports = map[string]*http.Transport{}
	}
	if tr, ok := s.egress.transports[proxyURL]; ok {
		return tr
	}
	tr := &http.Transport{
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 2 * time.Minute,
	}
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	s.egress.transports[proxyURL] = tr
	return tr
}

// resetTransports drops every cached transport (called on /reload).
func (s *server) resetTransports() {
	s.egress.trMu.Lock()
	defer s.egress.trMu.Unlock()
	for k, tr := range s.egress.transports {
		tr.CloseIdleConnections()
		delete(s.egress.transports, k)
	}
}
