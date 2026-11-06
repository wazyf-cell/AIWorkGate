package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// server holds the plugin's dependencies.
type server struct {
	cfg     pluginConfig
	store   *store
	client  *upstreamClient
	signing xfyunSecret
	// logins tracks pending WeChat scans so /accounts/login/{sid} can report
	// their progress. The scan itself lives in login.go.
	logins *loginManager
}

// pluginVersion is stamped through the manifest and /meta.
const pluginVersion = "0.1.0"

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/meta", s.requireToken(s.handleMeta))
	mux.HandleFunc("/accounts", s.requireToken(s.handleAccounts))
	mux.HandleFunc("/accounts/login", s.requireToken(s.handleLoginStart))
	mux.HandleFunc("/accounts/login/", s.requireToken(s.handleLoginPoll))
	mux.HandleFunc("/v1/models", s.requireToken(s.handleModels))
	mux.HandleFunc("/v1/chat/completions", s.requireToken(s.handleChat))
	mux.HandleFunc("/reload", s.requireToken(s.handleReload))
	mux.HandleFunc("/admin/credentials", s.requireToken(s.handleAdminCredentials))
	return mux
}

// requireToken rejects any request that does not carry the plugin token. /health
// stays outside it because a human debugging by hand has no token and the
// endpoint leaks only liveness.
func (s *server) requireToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Token == "" {
			// No token was injected: refuse rather than serve authenticated
			// endpoints to every local process.
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or missing bearer token")
			return
		}
		got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if got != s.cfg.Token {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid plugin token")
			return
		}
		next(w, r)
	}
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"status":     "ok",
		"version":    pluginVersion,
		"configured": s.store.configured(),
		"sealing":    s.store.sealKind,
	})
}

func (s *server) handleMeta(w http.ResponseWriter, r *http.Request) {
	creds, ok := s.store.get()
	out := map[string]any{
		"id":            s.cfg.ID,
		"name":          "Loomy（讯飞）",
		"version":       pluginVersion,
		"api_level":     s.cfg.APILevel,
		"capabilities":  capabilities,
		"account_count": boolToInt(ok),
		"configured":    ok,
		"sealing":       s.store.sealKind,
		"base_url":      firstNonEmpty(creds.BaseURL, apiBase),
	}
	if creds.Label != "" {
		out["label"] = creds.Label
	}
	writeJSON(w, http.StatusOK, out)
}

// capabilities mirrors manifest.json and the routes really registered in
// routes(). `checkin`, `quota` and `refresh` are absent on purpose: the
// reference implements no check-in for Loomy, /accounts reports an empty quota
// object (no balance source) and there is no /accounts/{id}/refresh route.
// `login_oauth` IS declared because POST /accounts/login really opens a WeChat
// scan session (login_handlers.go), which is what makes the host render the
// login entry instead of "该插件没有声明登录方式".
var capabilities = []string{"upstream", "accounts", "login_oauth"}

// defaultAccountID is the one account this plugin exposes once a token is
// stored: the host has no notion of 讯飞 user ids as account handles.
const defaultAccountID = "default"

// handleAccounts reports exactly one account while a token is stored, or an
// empty list before login.
//
// The account exists so the host's scheduler has a candidate: with an api_level
// 2 plugin an empty account list makes every request die with 503
// ErrNoCandidate before this plugin is reached. quota is left empty (unknown)
// rather than fabricated — the reference reads a balance from a signed endpoint
// this skeleton cannot call (see BLOCKED-ON-REF in the lane document).
func (s *server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	creds, ok := s.store.get()
	accounts := []map[string]any{}
	if ok {
		accounts = append(accounts, map[string]any{
			"id":      defaultAccountID,
			"label":   firstNonEmpty(creds.Label, "Loomy"),
			"enabled": true,
			"status":  "ok",
			"credential": map[string]any{
				"present":     true,
				"refreshable": false,
			},
			"quota": map[string]any{},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": accounts})
}

// handleAdminCredentials lets the operator hand a token to the plugin directly.
// The reference's login is a WeChat QR dance that needs 讯飞 signed requests
// (accessKeyId/accessKeySecret) this plugin must not carry, so this endpoint is
// the supported path in this skeleton.
func (s *server) handleAdminCredentials(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST or PUT")
		return
	}
	var in struct {
		AccessToken string `json:"access_token"`
		UserID      string `json:"userid"`
		Session     string `json:"session"`
		ExpiresAt   string `json:"expires_at"`
		Label       string `json:"label"`
		BaseURL     string `json:"base_url"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if err := s.store.set(credentials{
		AccessToken: in.AccessToken, UserID: in.UserID, Session: in.Session,
		ExpiresAt: in.ExpiresAt, Label: in.Label, BaseURL: in.BaseURL,
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

// handleLoginStart / handleLoginPoll live in login_handlers.go: they drive the
// WeChat scan implemented in login.go and the signed exchange in
// login_account.go. The host contract needs the 400-with-variable-names answer
// when the signing pair is absent, which is why the 501 stub that used to sit
// here is gone ("501 = this plugin has no login" hides the entry entirely).

func (s *server) handleReload(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "rules": 0, "egress": 0})
}
