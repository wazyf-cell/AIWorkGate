package main

// The host-facing HTTP surface (contract v0.3/v0.4 subset).
//
// Endpoints implemented, and why each exists:
//
//	GET  /health                    supervisor probe, no token
//	GET  /meta                      identity + account count
//	GET  /accounts                  account list with inlined quota
//	GET  /accounts/login/{sid}      login polling (see login.go)
//	GET  /v1/models                 model catalog (cached, static fallback)
//	POST /v1/chat/completions       the proxy request
//	POST /checkin                   daily sign-in, one or all accounts
//	POST /refresh                   re-read balances now
//	POST /accounts/{id}/checkin     per-account sign-in (host calls this)
//	POST /accounts/{id}/refresh     per-account token refresh
//	PATCH /accounts/{id}            enable/disable
//	DELETE /accounts/{id}           forget the credential
//	POST /admin/credentials         operator hands us an autoclaw-<uid>.json
//	POST /reload                    re-reads the egress bindings file

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
)

var stdlogger = log.Default()

type server struct {
	cfg    pluginConfig
	st     *store
	up     *upstream
	engine *engine
	models *modelCache

	// login session state shared by start, submit and poll (login.go).
	loginMu sync.Mutex
	logins  map[string]*loginFlow
}

const pluginVersion = "0.1.0"

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/meta", s.requireToken(s.handleMeta))
	mux.HandleFunc("/accounts", s.requireToken(s.handleAccounts))
	mux.HandleFunc("/accounts/login", s.requireToken(s.handleLoginStart))
	mux.HandleFunc("/accounts/login/", s.requireToken(s.handleLoginPoll))
	mux.HandleFunc("/v1/models", s.requireToken(s.handleModels))
	mux.HandleFunc("/v1/chat/completions", s.requireToken(s.engine.handleChat))
	mux.HandleFunc("/checkin", s.requireToken(s.handleCheckin))
	mux.HandleFunc("/refresh", s.requireToken(s.handleRefreshAll))
	mux.HandleFunc("/admin/credentials", s.requireToken(s.handleAdminCredentials))
	mux.HandleFunc("/reload", s.requireToken(s.handleReload))
	mux.HandleFunc("/accounts/", s.requireToken(s.handleAccountSub))
	return mux
}

// requireToken enforces the host-injected bearer token. /health is exempt
// because the supervisor probes it.
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
	c := s.st.counts()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                  true,
		"status":              "ok",
		"version":             pluginVersion,
		"configured":          s.st.configured(),
		"sealing":             s.st.sealKindName(),
		"accounts_total":      c.Total,
		"accounts_healthy":    c.Healthy,
		"upstream_configured": s.st.configured(),
	})
}

func (s *server) handleMeta(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"id":            s.cfg.ID,
		"name":          "AutoGLM（智谱）",
		"version":       pluginVersion,
		"api_level":     s.cfg.APILevel,
		"capabilities":  []string{"upstream", "accounts", "checkin", "quota", "refresh", "egress"},
		"account_count": len(s.st.list()),
		"configured":    s.st.configured(),
		"regions":       []string{regionCN, regionGlobal},
	})
}

func (s *server) handleModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": s.models.modelsFor(s.engine)})
}

func (s *server) handleCheckin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	var body struct {
		Region string `json:"region"`
		DryRun bool   `json:"dry_run"`
		UID    string `json:"uid"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body)
	writeJSON(w, http.StatusOK, s.engine.checkinRun(sanitizeRegion(body.Region, "all"), body.DryRun, "checkin", body.UID))
}

func (s *server) handleRefreshAll(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 64<<10))
	out := []map[string]any{}
	for _, row := range s.st.statusList() {
		a := s.st.byID(row.UID)
		if a == nil {
			continue
		}
		pts, err := s.up.checkPoints(a)
		if err == nil {
			s.st.reenableIfPoints(a, pts)
			out = append(out, map[string]any{"uid": row.UID, "region": row.Region, "points": pts})
			continue
		}
		out = append(out, map[string]any{"uid": row.UID, "region": row.Region, "points": row.Points, "error": truncStr(err.Error(), 120)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

func (s *server) handleReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	// Re-read the host's egress.json and drop every cached transport, so a
	// re-bound proxy takes effect without restarting the plugin.
	ef, err := s.up.applyEgress(s.cfg.EgressFile)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "egress_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "rules": 0, "egress": len(ef.Accounts), "strict": ef.Strict,
	})
}
