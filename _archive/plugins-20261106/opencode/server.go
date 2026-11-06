package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// pluginVersion must match manifest.json "version".
const pluginVersion = "0.1.0"

// capabilities mirrors manifest.json "capabilities". quota is absent on purpose:
// there is no quota source, so /accounts reports an empty quota and declaring the
// capability would only put a permanently blank panel in the UI.
var capabilities = []string{"upstream", "accounts", "refresh", "login_api_key", "identity_request", "egress"}

// serverConfig is the environment the host handed to this process.
type serverConfig struct {
	ID        string
	Token     string
	DataDir   string
	APILevel  int
	StartedAt time.Time
	// EgressFile is AIWORKGATE_EGRESS_FILE: runtime\opencode\egress.json. Empty
	// means the host did not hand one over, which is the direct-connection case.
	EgressFile string
}

// server holds the plugin's dependencies.
type server struct {
	cfg    serverConfig
	store  *store
	caps   *capabilityCache
	client *upstreamClient
	logMu  sync.Mutex

	// egress is the last binding doc read from EgressFile. It is kept next to
	// the client's own copy so strict-mode decisions can be answered without
	// touching the transport.
	egressMu sync.RWMutex
	egress   egressDoc

	// logins remembers the fate of each api_key session: pending until the user
	// submits, cancelled once the dialog is dismissed. A session that is not in
	// here is treated as settled (the anonymous lane is always usable).
	loginMu sync.Mutex
	logins  map[string]loginSession
}

func newServer(cfg serverConfig) (*server, error) {
	st, err := newStore(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	if cfg.StartedAt.IsZero() {
		cfg.StartedAt = time.Now()
	}
	s := &server{
		cfg:    cfg,
		store:  st,
		caps:   newCapabilityCache(cfg.DataDir),
		client: newUpstreamClient(),
		logins: map[string]loginSession{},
	}
	// Apply the binding at startup so a proxy set before a host restart keeps
	// working; the host also calls POST /reload after every rewrite.
	if err := s.reloadEgress(); err != nil {
		s.log("warn", "cannot read egress file", map[string]any{"error": err.Error()})
	}
	return s, nil
}

// log writes one JSON line to stdout; the host captures it into
// logs\plugins\opencode-YYYY-MM-DD.log.
func (s *server) log(level, msg string, fields map[string]any) {
	line := map[string]any{
		"ts":    time.Now().Format(time.RFC3339),
		"level": level,
		"msg":   msg,
	}
	for k, v := range fields {
		line[k] = v
	}
	data, err := json.Marshal(line)
	if err != nil {
		return
	}
	s.logMu.Lock()
	defer s.logMu.Unlock()
	_, _ = os.Stdout.Write(append(data, '\n'))
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
// stays outside it: the supervisor probes it, and it leaks nothing but liveness.
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
		"ok":       true,
		"status":   "ok",
		"version":  pluginVersion,
		"uptime_s": int64(time.Since(s.cfg.StartedAt).Seconds()),
		"sealing":  s.store.sealKind,
		// The anonymous free tier needs no login, so there is always a usable
		// upstream; "configured" additionally means a real key is stored.
		"upstream_configured": true,
		"anonymous":           s.store.current().anonymous(),
	})
}

func (s *server) handleMeta(w http.ResponseWriter, r *http.Request) {
	creds := s.store.current()
	writeJSON(w, http.StatusOK, map[string]any{
		"id":            s.cfg.ID,
		"name":          "OpenCode",
		"version":       pluginVersion,
		"api_level":     s.cfg.APILevel,
		"capabilities":  capabilities,
		"account_count": 1,
		"uptime_s":      int64(time.Since(s.cfg.StartedAt).Seconds()),
		"started_at":    s.cfg.StartedAt.Unix(),
		"sealing":       s.store.sealKind,
		"base_url":      inferenceBase,
		"anonymous":     creds.anonymous(),
		"user_agent":    creds.effectiveUserAgent(),
	})
}
