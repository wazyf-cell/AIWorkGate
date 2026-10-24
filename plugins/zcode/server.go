package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// server holds the plugin's dependencies.
type server struct {
	cfg    pluginConfig
	store  *store
	client *upstreamClient
	egress egressState
	now    func() time.Time

	// logins holds the paste_code sessions still waiting for the operator's
	// submit. An existing credential must not make a fresh session look complete:
	// the UI closes the dialog on success.
	loginMu   sync.Mutex
	logins    map[string]time.Time
	cancelled map[string]time.Time

	// importOverride / validateOverride let tests exercise the login flow
	// without the operator's real client state or a network call. Production
	// leaves both nil, which means "read the file and ask the upstream".
	importOverride   func(ctx context.Context) (localImport, error)
	validateOverride func(token string) error
}

// pluginVersion is stamped through the manifest and /meta.
const pluginVersion = "0.2.0"

// capabilities must match manifest.json exactly: every entry here has a route
// behind it. quota/refresh were removed because the plugin has neither a live
// quota endpoint for the api-key route nor a way to renew a credential.
var capabilities = []string{
	"upstream", "accounts", "login_local_state", "login_paste_code",
	"identity_request", "egress",
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/meta", s.requireToken(s.handleMeta))
	mux.HandleFunc("/accounts", s.requireToken(s.handleAccounts))
	mux.HandleFunc("/accounts/login", s.requireToken(s.handleLoginStart))
	mux.HandleFunc("/accounts/login/", s.requireToken(s.handleLoginPoll))
	mux.HandleFunc("/accounts/", s.requireToken(s.handleAccountSub))
	mux.HandleFunc("/v1/models", s.requireToken(s.handleModels))
	mux.HandleFunc("/v1/chat/completions", s.requireToken(s.handleChat))
	mux.HandleFunc("/reload", s.requireToken(s.handleReload))
	mux.HandleFunc("/admin/credentials", s.requireToken(s.handleAdminCredentials))
	return mux
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
	creds, ok := s.store.get()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                  true,
		"status":              "ok",
		"version":             pluginVersion,
		"configured":          ok,
		"kind":                creds.kind(),
		"sealing":             s.store.sealKind,
		"upstream_configured": ok,
	})
}

func (s *server) handleMeta(w http.ResponseWriter, r *http.Request) {
	creds, ok := s.store.get()
	writeJSON(w, http.StatusOK, map[string]any{
		"id":            s.cfg.ID,
		"name":          "ZCode（智谱）",
		"version":       pluginVersion,
		"api_level":     s.cfg.APILevel,
		"capabilities":  capabilities,
		"account_count": boolToInt(ok),
		"configured":    ok,
		"label":         creds.Label,
		"kind":          creds.kind(),
		"base_url":      creds.origin(),
	})
}

// handleAccounts reports exactly one account while a credential is stored, or
// an empty list before login. quota is left empty (unknown) rather than
// fabricated: the api-key route exposes no balance the plugin can read.
func (s *server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	creds, ok := s.store.get()
	accounts := []map[string]any{}
	if ok {
		accounts = append(accounts, map[string]any{
			"id":      "default",
			"label":   firstNonEmpty(creds.Label, "ZCode"),
			"enabled": true,
			"status":  "ok",
			"credential": map[string]any{
				"present":     true,
				"refreshable": false,
			},
			"identity": s.identityOf(creds),
			"quota":    map[string]any{},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": accounts})
}

// handleAdminCredentials lets the operator hand a credential to the plugin
// directly. The value is validated upstream first, so a typo cannot masquerade
// as a working login.
func (s *server) handleAdminCredentials(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST or PUT")
		return
	}
	var in struct {
		JWT       string `json:"jwt"`
		APIKey    string `json:"api_key"`
		Kind      string `json:"kind"`
		DeviceMid string `json:"device_mid"`
		Label     string `json:"label"`
		BaseURL   string `json:"base_url"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	token := firstNonEmpty(strings.TrimSpace(in.APIKey), strings.TrimSpace(in.JWT))
	if token == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "jwt or api_key is required")
		return
	}
	if err := s.validateToken(r.Context(), token); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_credential", "上游拒绝了这个凭据："+err.Error())
		return
	}
	kind := strings.ToLower(strings.TrimSpace(in.Kind))
	if kind == "" {
		if strings.TrimSpace(in.APIKey) != "" {
			kind = credentialKindAPIKey
		} else {
			kind = credentialKindJWT
		}
	}
	creds := credentials{Kind: kind, DeviceMid: in.DeviceMid, Label: in.Label, BaseURL: in.BaseURL,
		UpdatedAt: s.clock().Unix()}
	if kind == credentialKindAPIKey {
		creds.APIKey = token
	} else {
		creds.JWT = token
	}
	if err := s.store.set(creds); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	stored, _ := s.store.get()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"configured":      true,
		"kind":            stored.kind(),
		"key_fingerprint": fingerprint(token),
	})
}

func (s *server) handleReload(w http.ResponseWriter, r *http.Request) {
	egressErr := s.reloadEgress()
	s.resetTransports()
	s.egress.mu.RLock()
	strict := s.egress.file.Strict
	accounts := len(s.egress.file.Accounts)
	s.egress.mu.RUnlock()
	out := map[string]any{"ok": egressErr == nil, "rules": 0, "egress_accounts": accounts, "egress_strict": strict}
	if egressErr != nil {
		out["warning"] = "reload egress: " + egressErr.Error()
	}
	writeJSON(w, http.StatusOK, out)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, kind, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": message, "type": kind},
	})
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// fingerprint is a short, non-reversible tag so two tokens can be told apart in
// a log without exposing either.
func fingerprint(secret string) string {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return ""
	}
	sum := sha256Sum(secret)
	return "sha256:" + sum[:8]
}
