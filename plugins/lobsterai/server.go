package main

import (
	"context"
	"net/http"
	"strings"
)

// server holds the plugin's dependencies.
type server struct {
	cfg    pluginConfig
	store  *store
	client *upstreamClient
	egress egressState
	login  loginState

	// exchangeOverride lets tests drive the login flow without a portal. It is
	// nil in production.
	exchangeOverride func(ctx context.Context, code string, session loginSession) (credentials, error)
}

const pluginVersion = "0.1.0"

// capabilities mirrors manifest.json. `checkin` is absent: the reference has no
// check-in endpoint for LobsterAI (it exposes credits and models only), so
// declaring it would be a promise with nothing behind it.
var capabilities = []string{
	"upstream", "accounts", "login_oauth", "identity_request", "egress", "refresh",
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/meta", s.requireToken(s.handleMeta))
	mux.HandleFunc("/accounts", s.requireToken(s.handleAccounts))
	mux.HandleFunc("/accounts/login", s.requireToken(s.handleLoginStart))
	mux.HandleFunc("/accounts/login/", s.requireToken(s.handleLoginPoll))
	mux.HandleFunc("/accounts/", s.requireToken(s.handleAccountSub))
	mux.HandleFunc("/auth/callback", s.handleAuthCallback)
	mux.HandleFunc("/v1/models", s.requireToken(s.handleModels))
	mux.HandleFunc("/v1/chat/completions", s.requireToken(s.handleChat))
	mux.HandleFunc("/reload", s.requireToken(s.handleReload))
	mux.HandleFunc("/admin/credentials", s.requireToken(s.handleAdminCredentials))
	mux.HandleFunc("/admin/exchange", s.requireToken(s.handleAdminExchange))
	return mux
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

func (s *server) handleReload(w http.ResponseWriter, r *http.Request) {
	err := s.reloadEgress()
	s.resetTransports()
	s.egress.mu.RLock()
	accounts := len(s.egress.file.Accounts)
	strict := s.egress.file.Strict
	s.egress.mu.RUnlock()
	out := map[string]any{"ok": err == nil, "rules": 0, "egress_accounts": accounts, "egress_strict": strict}
	if err != nil {
		out["warning"] = "reload egress: " + err.Error()
	}
	writeJSON(w, http.StatusOK, out)
}
