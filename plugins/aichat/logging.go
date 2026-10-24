// Package main — access logging for the plugin's own HTTP surface.
package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"
)

// log writes one JSON line to stdout; the host captures it into
// %APPDATA%\AIWorkGate\logs\plugins\aichat-YYYY-MM-DD.log.
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
		// The host probes /health every 10s. Logging the successes drowns the
		// file (measured 2026-10-22: 68% of a day's lines were /health), which is
		// why a real failure becomes impossible to find. Failures still log, and
		// the trailing slash is trimmed so both spellings of the probe are
		// covered rather than just one.
		if r.Method == http.MethodGet && strings.TrimSuffix(r.URL.Path, "/") == "/health" && rec.status < 400 {
			return
		}
		s.log(level, "http_request", map[string]any{
			"method": r.Method,
			"path":   r.URL.Path,
			"status": rec.status,
			"dur_ms": time.Since(start).Milliseconds(),
		})
	})
}
