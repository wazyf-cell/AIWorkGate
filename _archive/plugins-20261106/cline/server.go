package main

// Process wiring: configuration, routes, token guard and the small JSON/log
// helpers. The account, login, refresh and catalog surfaces live in their own
// files so each stays readable.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// pluginVersion must match manifest.json "version".
const pluginVersion = "0.1.0"

// capabilities mirrors manifest.json "capabilities". quota is absent: Cline
// reports credit only inside a 402, so /accounts has no balance to poll and an
// empty quota object must not be advertised as a source.
var capabilities = []string{"upstream", "accounts", "refresh", "login_oauth", "login_paste_code", "egress"}

// serverConfig is the environment the host handed to this process.
type serverConfig struct {
	ID         string
	Token      string
	DataDir    string
	APILevel   int
	StartedAt  time.Time
	EgressFile string
}

// server holds the plugin's dependencies.
type server struct {
	cfg    serverConfig
	store  *store
	client *upstreamClient
	logMu  sync.Mutex

	// loginMu guards the in-flight device login sessions.
	loginMu sync.Mutex
	logins  map[string]*loginSession

	// refreshMu/refreshing collapse concurrent renewals of one refresh token into
	// a single upstream call.
	refreshMu  sync.Mutex
	refreshing map[string]*refreshCall

	// egress is the host-written account binding, re-read on /reload.
	egressMu   sync.RWMutex
	egressPath string
	egress     egressDoc

	// pollSleep/slowDownStep are seams the device-flow tests drive per server;
	// production leaves them nil/zero and gets the defaults below.
	pollSleep    func(time.Duration) <-chan time.Time
	slowDownStep time.Duration
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
		cfg:        cfg,
		store:      st,
		client:     newUpstreamClient(),
		logins:     map[string]*loginSession{},
		refreshing: map[string]*refreshCall{},
		egressPath: strings.TrimSpace(cfg.EgressFile),
	}
	if err := s.reloadEgress(); err != nil {
		// A malformed binding file must not take the plugin down; direct mode is
		// what an absent file means anyway.
		s.log("warn", "egress_load_failed", map[string]any{"error": err.Error()})
	}
	return s, nil
}

// log writes one JSON line to stdout; the host captures it into
// logs\plugins\cline-YYYY-MM-DD.log.
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
		"ok":                  true,
		"status":              "ok",
		"version":             pluginVersion,
		"uptime_s":            int64(time.Since(s.cfg.StartedAt).Seconds()),
		"sealing":             s.store.sealKind,
		"upstream_configured": s.store.configured(),
	})
}

func (s *server) handleMeta(w http.ResponseWriter, r *http.Request) {
	creds, ok := s.store.get()
	writeJSON(w, http.StatusOK, map[string]any{
		"id":            s.cfg.ID,
		"name":          "Cline",
		"version":       pluginVersion,
		"api_level":     s.cfg.APILevel,
		"capabilities":  capabilities,
		"account_count": boolToInt(ok),
		"uptime_s":      int64(time.Since(s.cfg.StartedAt).Seconds()),
		"started_at":    s.cfg.StartedAt.Unix(),
		"sealing":       s.store.sealKind,
		"base_url":      apiBase,
		"login_url":     appBase,
		"label":         creds.label(),
	})
}

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

// fingerprint is a short, non-reversible tag so two tokens can be told apart in a
// log without exposing either.
func fingerprint(token string) string {
	token = bareToken(token)
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return "sha256:" + hex.EncodeToString(sum[:4])
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// errText renders an error, substituting a message when there is none.
func errText(err error) string {
	if err == nil {
		return "unknown error"
	}
	return err.Error()
}

// newUUID returns a random RFC-4122 v4 UUID, used as a login session id.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	dst := make([]byte, 36)
	hex.Encode(dst[0:8], b[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], b[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], b[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], b[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], b[10:16])
	return string(dst)
}

// ctxOrBackground returns ctx, or Background when nil.
func ctxOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
