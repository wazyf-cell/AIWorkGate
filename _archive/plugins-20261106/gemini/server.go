package main

import (
	"encoding/json"
	"net/http"
	"time"
)

// pluginVersion is stamped through the manifest and /health.
const pluginVersion = "0.1.0"

// defaultAccountID is the only account this channel stores.
const defaultAccountID = "default"

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/meta", s.requireToken(s.handleMeta))
	mux.HandleFunc("/accounts", s.requireToken(s.handleAccounts))
	mux.HandleFunc("/accounts/login", s.requireToken(s.handleLoginStart))
	mux.HandleFunc("/accounts/login/", s.requireToken(s.handleLoginPoll))
	// Longer patterns win, so /accounts/login/... still reaches the login
	// handler; everything else under /accounts/ is the per-account surface.
	mux.HandleFunc("/accounts/", s.requireToken(s.handleAccountSub))
	mux.HandleFunc("/accounts/default/refresh", s.requireToken(s.handleRefresh))
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
			if trimBearer(r.Header.Get("Authorization")) != s.cfg.Token {
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
		"sealing":             s.store.sealKind,
		"upstream_configured": ok,
		"expired":             ok && creds.nearExpiry(time.Now()),
	})
}

func (s *server) handleMeta(w http.ResponseWriter, r *http.Request) {
	creds, ok := s.store.get()
	writeJSON(w, http.StatusOK, map[string]any{
		"id":            s.cfg.ID,
		"name":          "Gemini Code Assist",
		"version":       pluginVersion,
		"api_level":     s.cfg.APILevel,
		"capabilities":  []string{"upstream", "accounts", "quota", "refresh", "login_oauth", "egress", "identity_request"},
		"account_count": boolToInt(ok),
		"configured":    ok,
		"label":         creds.label(),
		"base_url":      firstNonEmpty(creds.EndpointBase, endpointDaily),
	})
}

func (s *server) handleModels(w http.ResponseWriter, r *http.Request) {
	entries := catalog()
	data := make([]map[string]any, 0, len(entries))
	for _, m := range entries {
		data = append(data, map[string]any{
			"id":             m.ID,
			"object":         "model",
			"owned_by":       "gemini",
			"name":           m.Name,
			"context_length": m.ContextWindow,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
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
