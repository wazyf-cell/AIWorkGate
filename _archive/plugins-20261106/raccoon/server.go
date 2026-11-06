package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
)

// server holds the plugin's dependencies.
type server struct {
	cfg    pluginConfig
	store  *store
	client *upstreamClient
	logins *loginManager

	egressPath string
	egressMu   sync.RWMutex
	egress     egressFile
}

const pluginVersion = "0.2.0"

// capabilities mirrors manifest.json and the routes actually registered in
// routes(). `checkin` stays absent: the reference has no check-in endpoint for
// Raccoon. `quota` stays absent too: /accounts has no balance to report, and
// declaring it would make the host poll an empty source. `login_oauth` is
// declared because POST /accounts/login really opens a scan-code session;
// `egress` and `identity_request` only because egress.json routing and identity
// rotation are implemented below.
var capabilities = []string{
	"upstream", "accounts", "refresh", "login_oauth", "egress", "identity_request",
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/meta", s.requireToken(s.handleMeta))
	mux.HandleFunc("/accounts", s.requireToken(s.handleAccounts))
	mux.HandleFunc("/accounts/login", s.requireToken(s.handleLoginStart))
	mux.HandleFunc("/accounts/login/", s.requireToken(s.handleLoginRoute))
	mux.HandleFunc("/accounts/", s.requireToken(s.handleAccountAction))
	mux.HandleFunc("/v1/models", s.requireToken(s.handleModels))
	mux.HandleFunc("/v1/chat/completions", s.requireToken(s.handleChat))
	mux.HandleFunc("/reload", s.requireToken(s.handleReload))
	mux.HandleFunc("/admin/credentials", s.requireToken(s.handleAdminCredentials))
	// The scan-code page must be openable by the user's browser, which cannot
	// send the plugin bearer token. It is loopback-only and serves nothing but a
	// QR for a live session the caller already created.
	mux.HandleFunc("/qr/", s.handleQRPage)
	return mux
}

// handleLoginRoute dispatches GET/DELETE /accounts/login/{sid} and
// POST /accounts/login/{sid}/submit.
func (s *server) handleLoginRoute(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/submit") {
		s.handleLoginSubmit(w, r)
		return
	}
	s.handleLoginPoll(w, r)
}

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
		"name":          "Raccoon（商汤）",
		"version":       pluginVersion,
		"api_level":     s.cfg.APILevel,
		"capabilities":  capabilities,
		"account_count": boolToInt(ok),
		"configured":    ok,
		"sealing":       s.store.sealKind,
		"base_url":      originOf(creds.BaseURL),
		"client":        userAgent,
	}
	if creds.Label != "" {
		out["label"] = creds.Label
	}
	writeJSON(w, http.StatusOK, out)
}

// handleAccounts reports one account while a token is stored. The account keeps
// the host's scheduler supplied: with an api_level 2 plugin an empty account list
// makes every request fail 503 ErrNoCandidate before it reaches this plugin.
// quota is left empty (unknown) rather than fabricated.
func (s *server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	creds, ok := s.store.get()
	accounts := []map[string]any{}
	if ok {
		account := map[string]any{
			"id":      "default",
			"label":   firstNonEmpty(creds.Label, "Raccoon"),
			"enabled": true,
			"status":  "ok",
			"credential": map[string]any{
				// refreshable mirrors isRaccoonRefreshable: true only when a
				// refresh_token was stored.
				"present":     true,
				"refreshable": strings.TrimSpace(creds.RefreshToken) != "",
			},
			"quota": map[string]any{},
		}
		if view := identityView(creds.Identity); view != nil {
			account["identity"] = view
		}
		accounts = append(accounts, account)
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": accounts})
}

// handleAdminCredentials lets the operator hand a token to the plugin.
//
// The reference's own login is a QR / SMS flow that needs a slider captcha
// (aliyunCaptcha sceneId "1pkmy0x3", prefix "hk1r5l") plus an AES-128-CFB
// phone cipher whose key lives in a secret this task forbids copying, so the
// flow is BLOCKED-ON-USER. This endpoint is the supported path.
func (s *server) handleAdminCredentials(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST or PUT")
		return
	}
	var in struct {
		AccessToken    string `json:"access_token"`
		RefreshToken   string `json:"refresh_token"`
		OfficeIdentity string `json:"office_identity"`
		DeviceID       string `json:"device_id"`
		ExpiresAt      string `json:"expires_at"`
		Label          string `json:"label"`
		BaseURL        string `json:"base_url"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if err := s.store.set(credentials{
		AccessToken: in.AccessToken, RefreshToken: in.RefreshToken,
		OfficeIdentity: in.OfficeIdentity, DeviceID: in.DeviceID,
		ExpiresAt: in.ExpiresAt, Label: in.Label, BaseURL: in.BaseURL,
		Identity: newIdentity(),
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

// handleReload re-reads egress.json and drops every cached transport, so a
// re-bound proxy takes effect without restarting the plugin.
func (s *server) handleReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	n, err := s.reloadEgress()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "reload_failed", err.Error())
		return
	}
	s.egressMu.RLock()
	strict := s.egress.Strict
	s.egressMu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "rules": 0, "egress": n, "strict": strict})
}

// handleModels prefers the live catalog, falling back to the manifest's static
// table — the reference's fetchModels returns [] on any failure so the adapter can
// fall back ("失败时返回空数组：适配器据此回退兜底表").
func (s *server) handleModels(w http.ResponseWriter, r *http.Request) {
	creds, ok := s.store.get()
	if ok {
		proxyURL, allow := s.proxyFor(w, "default")
		if !allow {
			return
		}
		body, err := s.client.catalog(r.Context(), creds.BaseURL, creds, proxyURL)
		if err == nil {
			if models, perr := parseCatalog(body); perr == nil && len(models) > 0 {
				writeJSON(w, http.StatusOK, map[string]any{
					"object": "list",
					"data":   models,
					"source": "remote",
				})
				return
			}
			s.respondFallbackModels(w, "catalog response had no usable entries")
			return
		}
		s.respondFallbackModels(w, err.Error())
		return
	}
	s.respondFallbackModels(w, "not configured")
}

func (s *server) respondFallbackModels(w http.ResponseWriter, reason string) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   fallbackModels(),
		"source": "fallback",
		"note":   "远端模型目录不可用，回退到清单静态表：" + reason,
	})
}

// handleChat lives in chat.go: it forwards one request and relays or aggregates
// the SSE answer.

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, kind, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": message, "type": kind, "code": kind},
	})
}

func openAIErrorBody(status int, message string) []byte {
	if strings.TrimSpace(message) == "" {
		message = "upstream returned a non-2xx status"
	}
	b, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "upstream_error",
			"code":    status,
		},
	})
	return b
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func fingerprint(secret string) string {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return ""
	}
	sum := sha256Sum(secret)
	return "sha256:" + sum[:8]
}
