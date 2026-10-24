package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// server holds the plugin's dependencies.
type server struct {
	cfg    pluginConfig
	store  *store
	client *upstreamClient

	// loginMu guards logins, the in-flight device flows.
	loginMu sync.Mutex
	logins  map[string]*loginFlow

	// egress is the host-written account binding, re-read on /reload.
	egressMu   sync.RWMutex
	egressPath string
	egress     egressDoc

	// pollSleep/slowDownStep are per-server seams for the device-flow tests.
	pollSleep    func(time.Duration) <-chan time.Time
	slowDownStep time.Duration
}

// pluginVersion is stamped through the manifest and /health.
const pluginVersion = "0.1.0"

// capabilities mirrors manifest.json exactly.
var capabilities = []string{"upstream", "accounts", "quota", "refresh", "login_oauth", "login_paste_code", "egress"}

// loginFlow is one in-flight login. The reference drives the poll in the
// background so the login URL can be handed to a browser immediately
// (startLogin returns loginUrl before the user has approved); this port does the
// same, but keeps the polling loop in the plugin so /accounts/login/{sid} can be
// answered without the host blocking.
type loginFlow struct {
	auth   deviceAuthorization
	method string
	status string
	token  *tokenGrant
	errMsg string
	done   chan struct{}
	cancel context.CancelFunc
	// finishedAt is when the terminal status was set, for the sweep.
	finishedAt time.Time
	// createdAt is set for paste flows, which have no device expiry of their own.
	createdAt time.Time
	// pollState/attempts are diagnostics exposed on the poll endpoint.
	pollState string
	attempts  int
}

// newServer builds the plugin and loads the host-written egress binding.
func newServer(cfg pluginConfig) (*server, error) {
	st, err := newStore(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	s := &server{
		cfg:        cfg,
		store:      st,
		client:     newUpstreamClient(),
		logins:     map[string]*loginFlow{},
		egressPath: strings.TrimSpace(cfg.EgressFile),
	}
	if err := s.reloadEgress(); err != nil {
		// A malformed binding file must not prevent startup; direct is the
		// meaning of an absent file anyway.
		fmt.Fprintln(os.Stderr, "minimax: egress:", err)
	}
	return s, nil
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/meta", s.requireToken(s.handleMeta))
	mux.HandleFunc("/accounts", s.requireToken(s.handleAccounts))
	mux.HandleFunc("/accounts/login", s.requireToken(s.handleLoginStart))
	mux.HandleFunc("/accounts/login/", s.requireToken(s.handleLoginPoll))
	mux.HandleFunc("/accounts/default/refresh", s.requireToken(s.handleRefresh))
	mux.HandleFunc("/accounts/", s.requireToken(s.handleAccountSub))
	mux.HandleFunc("/v1/models", s.requireToken(s.handleModels))
	mux.HandleFunc("/v1/chat/completions", s.requireToken(s.handleChat))
	mux.HandleFunc("/reload", s.requireToken(s.handleReload))
	mux.HandleFunc("/admin/credentials", s.requireToken(s.handleAdminCredentials))
	return mux
}

// handleAccountSub serves the one account-management verb this channel
// supports: DELETE /accounts/{id}, which drops the stored credential.
func (s *server) handleAccountSub(w http.ResponseWriter, r *http.Request) {
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/accounts/"), "/")
	if id != "default" {
		writeError(w, http.StatusConflict, "account_unavailable", "unknown account "+id)
		return
	}
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint: "+r.Method+" "+r.URL.Path)
		return
	}
	if err := s.store.clear(); err != nil {
		writeError(w, http.StatusInternalServerError, "delete_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// requireToken rejects any request that does not carry the plugin token. /health
// stays outside it because the host's supervisor probes it without a token.
func (s *server) requireToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Token != "" {
			got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
			if got != s.cfg.Token {
				writeError(w, http.StatusUnauthorized, "unauthorized", "invalid plugin token")
				return
			}
		}
		next(w, r)
	}
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                  true,
		"status":              "ok",
		"version":             pluginVersion,
		"configured":          s.store.configured(),
		"sealing":             s.store.sealKind,
		"upstream_configured": s.store.configured(),
		"expired":             s.store.expired(),
	})
}

func (s *server) handleMeta(w http.ResponseWriter, r *http.Request) {
	creds, ok := s.store.get()
	writeJSON(w, http.StatusOK, map[string]any{
		"id":            s.cfg.ID,
		"name":          "MiniMax Code",
		"version":       pluginVersion,
		"api_level":     s.cfg.APILevel,
		"capabilities":  capabilities,
		"account_count": boolToInt(ok),
		"configured":    ok,
		"label":         creds.Nickname,
		"base_url":      firstNonEmpty(creds.BaseURL, apiHost),
	})
}

// handleAccounts reports exactly one account while a token is stored, or an empty
// list before login. quota is filled from the live credit endpoint when it
// answers; when it does not, quota stays empty rather than fabricated.
func (s *server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	creds, ok := s.store.get()
	accounts := []map[string]any{}
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"accounts": accounts})
		return
	}
	cred := map[string]any{
		"present":     true,
		"refreshable": creds.refreshable(),
	}
	if exp, known := creds.expiresAtMs(); known {
		cred["expires_at"] = exp / 1000
	}
	status := "ok"
	if s.store.expired() {
		status = "expired"
	}
	acct := map[string]any{
		"id":         "default",
		"label":      firstNonEmpty(creds.Nickname, creds.AccountID, "MiniMax"),
		"enabled":    true,
		"status":     status,
		"credential": cred,
		"quota":      s.quotaSnapshot(r.Context(), creds),
	}
	accounts = append(accounts, acct)
	writeJSON(w, http.StatusOK, map[string]any{"accounts": accounts})
}

// handleAdminCredentials lets the operator hand a credential to the plugin
// directly. Accepts the reference's credential JSON verbatim.
func (s *server) handleAdminCredentials(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST or PUT")
		return
	}
	var in struct {
		AccessToken    string `json:"access_token"`
		RefreshToken   string `json:"refresh_token"`
		TokenType      string `json:"token_type"`
		ExpiresAt      string `json:"expires_at"`
		Scope          string `json:"scope"`
		AccountID      string `json:"account_id"`
		Nickname       string `json:"nickname"`
		Label          string `json:"label"`
		BaseURL        string `json:"base_url"`
		AccountBaseURL string `json:"account_base_url"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if in.Nickname == "" {
		in.Nickname = in.Label
	}
	if err := s.store.set(credentials{
		AccessToken:    in.AccessToken,
		RefreshToken:   in.RefreshToken,
		TokenType:      in.TokenType,
		ExpiresAt:      in.ExpiresAt,
		Scope:          in.Scope,
		AccountID:      in.AccountID,
		Nickname:       in.Nickname,
		BaseURL:        in.BaseURL,
		AccountBaseURL: in.AccountBaseURL,
	}); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	creds, _ := s.store.get()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"configured":      true,
		"key_fingerprint": fingerprint(creds.AccessToken),
	})
}

func (s *server) handleReload(w http.ResponseWriter, r *http.Request) {
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

func (s *server) handleModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   manifestModels(),
	})
}
