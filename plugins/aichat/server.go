package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// serverConfig is the environment the host handed to this process.
type serverConfig struct {
	ID         string
	Token      string
	DataDir    string
	RulesFile  string
	EgressFile string
	APILevel   int
	StartedAt  time.Time
}

type server struct {
	cfg serverConfig

	store *store
	// upstream is the HTTP client used to reach the assistant. Tests replace
	// it through newTestServer's client field.
	client *http.Client

	logMu sync.Mutex
}

func newServer(cfg serverConfig) (*server, error) {
	st, err := newStore(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	if cfg.StartedAt.IsZero() {
		cfg.StartedAt = time.Now()
	}
	return &server{
		cfg:   cfg,
		store: st,
		client: &http.Client{
			// No total timeout: an SSE answer can stream for many minutes.
			// Liveness is enforced by the request context and the dial/TLS
			// timeouts below.
			Transport: &http.Transport{
				Proxy:                 nil,
				MaxIdleConnsPerHost:   8,
				IdleConnTimeout:       90 * time.Second,
				ResponseHeaderTimeout: 60 * time.Second,
			},
			// Never follow a redirect for a proxied request: the client asked
			// for exactly one upstream call (see the host's own passthrough
			// rule, TestUpstreamRedirectIsPassedThrough).
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// log, statusRecorder and logging live in logging.go.

// handler wraps every route with the bearer-token check and access logging.
func (s *server) handler() http.Handler {
	return s.logging(s.auth(http.HandlerFunc(s.route)))
}

// auth enforces Authorization: Bearer <AIWORKGATE_PLUGIN_TOKEN> on every route
// except GET /health.
//
// The host does send the token to /health (internal/plugins/supervisor.go), but
// /health must also stay answerable without it: a human debugging the plugin by
// hand has no token, and the endpoint leaks nothing but liveness. Everything
// else — including /meta and /v1/models — requires the token, so a local process
// cannot borrow the assistant's credentials.
func (s *server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if path(r) == "/health" {
			next.ServeHTTP(w, r)
			return
		}
		if s.cfg.Token == "" {
			// No token was injected: refuse rather than serve unauthenticated.
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or missing bearer token")
			return
		}
		const prefix = "Bearer "
		header := r.Header.Get("Authorization")
		if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) ||
			subtle.ConstantTimeCompare([]byte(strings.TrimSpace(header[len(prefix):])), []byte(s.cfg.Token)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or missing bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// path returns the request path without a trailing slash.
func path(r *http.Request) string {
	p := r.URL.Path
	if len(p) > 1 {
		p = strings.TrimSuffix(p, "/")
	}
	return p
}

func (s *server) route(w http.ResponseWriter, r *http.Request) {
	p := path(r)
	switch {
	case p == "/health" && r.Method == http.MethodGet:
		s.handleHealth(w, r)
	case p == "/meta" && r.Method == http.MethodGet:
		s.handleMeta(w, r)
	case p == "/v1/models" && r.Method == http.MethodGet:
		s.handleModels(w, r)
	case p == "/accounts" && r.Method == http.MethodGet:
		s.handleAccounts(w, r)
	case strings.HasPrefix(p, "/accounts/") && isIdentityAction(p) && r.Method == http.MethodPost:
		// This channel forwards to a local GUI that owns its own device state.
		// There is no per-account identity to read or rotate here, so say so
		// with a stable code instead of a bare 404 (which the host surfaces as
		// a confusing 502).
		writeError(w, http.StatusNotImplemented, "identity_not_applicable",
			"A 转发通道不持有设备身份：设备指纹由本机 AI Work 助手自己维护，换设备请在其设置里操作")
	case strings.HasPrefix(p, "/accounts/") && isAccountAction(p) && r.Method == http.MethodPost:
		// The manifest declares neither checkin nor refresh: the assistant owns
		// its own check-in and this plugin implements neither. Answer explicitly
		// with 501 instead of a bare 404 so a caller cannot mistake "not
		// supported here" for "no such endpoint".
		writeError(w, http.StatusNotImplemented, "not_implemented",
			"本骨架未实现签到/续期；A 的 trae/wb/qoder 签到需下一轮迁移")
	case p == "/reload" && r.Method == http.MethodPost:
		// Declares no rule pack and reads no egress file, but the host calls
		// this after regenerating egress.json; answering 200 keeps it happy.
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "rules": 0, "egress": 0})
	case p == "/admin/upstream" && (r.Method == http.MethodPost || r.Method == http.MethodPut):
		s.handleAdminUpstream(w, r)
	case isProxyPath(p) && r.Method == http.MethodPost:
		s.handleProxy(w, r)
	default:
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint: "+r.Method+" "+r.URL.Path)
	}
}

func (s *server) uptimeSeconds() int64 {
	return int64(time.Since(s.cfg.StartedAt).Seconds())
}

// handleHealth answers the shape the host's supervisor parses: any 2xx is
// healthy, "ok": false or a "status" other than "ok" is not.
func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                  true,
		"status":              "ok",
		"version":             pluginVersion,
		"uptime_s":            s.uptimeSeconds(),
		"upstream_configured": s.store.configured(),
	})
}

// isAccountAction reports whether p is /accounts/<id>/<checkin|refresh>.
func isAccountAction(p string) bool {
	segs := strings.Split(strings.Trim(p, "/"), "/")
	if len(segs) != 3 {
		return false
	}
	return segs[2] == "checkin" || segs[2] == "refresh"
}

// isIdentityAction matches /accounts/<id>/identity/{details,reset}.
func isIdentityAction(p string) bool {
	segs := strings.Split(strings.Trim(p, "/"), "/")
	if len(segs) != 4 || segs[0] != "accounts" || segs[2] != "identity" {
		return false
	}
	return segs[3] == "reset" || segs[3] == "details"
}

// upstreamAccountID is the single synthetic account this plugin reports. It is
// not a real pool member: the assistant holds one credential — the plugin's own
// — so the account exists only to satisfy the host scheduler.
const upstreamAccountID = "default"

// handleAccounts reports the one upstream this plugin forwards to, or an empty
// list before the upstream has been configured.
//
// Why an account at all: the host builds its scheduling pool from GET /accounts
// (internal/app/scheduler_snapshot.go schedulerAccountsFor) and, for every
// plugin request, requires at least one selectable account
// (internal/scheduler/scheduler.go evaluateAccountsLocked). An empty list makes
// every aichat/<model> call fail with 503 ErrNoCandidate before this plugin is
// ever reached — verified in internal/gateway/dispatch_test.go
// TestScheduledChatReportsNoCandidate ("no accounts" case). So the plugin
// exposes exactly one account while it has an upstream.
//
// The account is deliberately reported with no quota numbers: the scheduler's
// credit filter only excludes an account when Remaining <= 0 while Total > 0 or
// ExpiresAt > 0 is set (scheduler.go creditIneligibleReason). Leaving both at
// zero keeps the account selectable instead of parked as "credits depleted".
// The assistant does not expose per-account credits to this plugin, so claiming
// a number would be fabricated.
func (s *server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"accounts": s.accounts()})
}

// accounts returns the account list shared by GET /accounts and /meta's
// account_count, so the two can never disagree.
func (s *server) accounts() []map[string]any {
	base, key, ok := s.store.get()
	if !ok {
		return []map[string]any{}
	}
	return []map[string]any{{
		"id":      upstreamAccountID,
		"label":   "AI Work 助手（" + base + "）",
		"enabled": true,
		"status":  "ok",
		"credential": map[string]any{
			// The host's scheduler reads this as CredentialPresent. It must only
			// be true for a non-empty stored api_key: a base_url alone is not a
			// usable credential, and reporting true would make the scheduler
			// route traffic believed to be authenticated.
			"present":     key != "",
			"refreshable": false,
		},
		// Empty quota object: unknown, not zero-remaining. See above.
		"quota": map[string]any{},
	}}
}

func (s *server) handleMeta(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{
		"id":            s.cfg.ID,
		"name":          "AI Work 助手（转发）",
		"version":       pluginVersion,
		"api_level":     s.cfg.APILevel,
		"capabilities":  capabilities,
		"account_count": len(s.accounts()),
		"uptime_s":      s.uptimeSeconds(),
		"started_at":    s.cfg.StartedAt.Unix(),
		"sealing":       s.store.sealing,
	}
	if base, _, ok := s.store.get(); ok {
		// The base URL is not a credential; the key never leaves the process.
		out["upstream_base_url"] = base
	}
	writeJSON(w, http.StatusOK, out)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// decodeJSON reads one JSON object from the request body, bounded to 64 KiB
// because every body it is used for is a small control payload.
func decodeJSON(r *http.Request, dst any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		return errors.New("read body: " + err.Error())
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return errors.New("request body must be a JSON object")
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return errors.New("invalid JSON body: " + err.Error())
	}
	return nil
}

// writeError emits the OpenAI-style error envelope required by the contract.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    code,
			"code":    code,
		},
	})
}
