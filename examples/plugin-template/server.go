package main

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"os"
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
	StartedAt  time.Time
}

type server struct {
	cfg serverConfig

	logMu sync.Mutex
}

func newServer(cfg serverConfig) *server {
	return &server{cfg: cfg}
}

// log writes one JSON line to stdout. The host captures stdout line by line
// into %APPDATA%\AIWorkGate\logs\plugins\<id>-YYYY-MM-DD.log.
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

// handler wraps every route with the bearer-token check and request logging.
func (s *server) handler() http.Handler {
	return s.logging(s.auth(http.HandlerFunc(s.route)))
}

func (s *server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// An empty token means the plugin was started by hand for debugging.
		// The host always injects one, and then every request must carry it.
		if s.cfg.Token == "" {
			next.ServeHTTP(w, r)
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

// statusRecorder captures the status code for the access log and keeps Flush
// working for SSE.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.wrote {
		return
	}
	r.status = code
	r.wrote = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Flush() {
	if !r.wrote {
		r.WriteHeader(http.StatusOK)
	}
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		level := "info"
		switch {
		case rec.status >= 500:
			level = "error"
		case rec.status >= 400:
			level = "warn"
		}
		s.log(level, "http_request", map[string]any{
			"method": r.Method,
			"path":   r.URL.Path,
			"status": rec.status,
			"dur_ms": time.Since(start).Milliseconds(),
		})
	})
}

func (s *server) route(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if len(path) > 1 {
		path = strings.TrimSuffix(path, "/")
	}
	switch {
	case path == "/health" && r.Method == http.MethodGet:
		s.handleHealth(w, r)
	case path == "/meta" && r.Method == http.MethodGet:
		s.handleMeta(w, r)
	case path == "/v1/models" && r.Method == http.MethodGet:
		s.handleModels(w, r)
	case path == "/v1/chat/completions" && r.Method == http.MethodPost:
		s.handleChat(w, r)
	case path == "/reload" && r.Method == http.MethodPost:
		// Optional endpoint. The template declares no rule packs, so there is
		// nothing to re-read; answering 200 keeps the host's reload path happy.
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "rules": 0, "egress": 0})
	default:
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint: "+r.Method+" "+r.URL.Path)
	}
}

func (s *server) uptimeSeconds() int64 {
	return int64(time.Since(s.cfg.StartedAt).Seconds())
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"version":  pluginVersion,
		"uptime_s": s.uptimeSeconds(),
	})
}

func (s *server) handleMeta(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"id":            s.cfg.ID,
		"name":          "Hello 模板插件",
		"version":       pluginVersion,
		"api_level":     apiLevel,
		"capabilities":  []string{"upstream"},
		"account_count": 0,
		"uptime_s":      s.uptimeSeconds(),
		"started_at":    s.cfg.StartedAt.Unix(),
	})
}

func (s *server) handleModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data": []map[string]any{{
			"id":             "hello-chat",
			"object":         "model",
			"created":        s.cfg.StartedAt.Unix(),
			"owned_by":       s.cfg.ID,
			"name":           "Hello Chat",
			"context_length": 8192,
			"tools":          true,
			"vision":         false,
		}},
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
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
