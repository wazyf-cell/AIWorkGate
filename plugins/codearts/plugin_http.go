package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// authorized performs a constant-time bearer token check.
func (p *Plugin) authorized(r *http.Request) bool {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return false
	}
	if p.Token == "" {
		return false
	}
	got := strings.TrimSpace(h[len(prefix):])
	return subtle.ConstantTimeCompare([]byte(got), []byte(p.Token)) == 1
}

type ctxKey int

const reqInfoKey ctxKey = iota

type reqInfo struct {
	account string
}

func setAccount(r *http.Request, id string) {
	if info, ok := r.Context().Value(reqInfoKey).(*reqInfo); ok {
		info.account = id
	}
}

// statusRecorder captures the status code for logging and supports SSE flush.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.wrote {
		return
	}
	s.status = code
	s.wrote = true
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wrote {
		s.WriteHeader(http.StatusOK)
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Flush() {
	if !s.wrote {
		s.WriteHeader(http.StatusOK)
	}
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Handler returns the authenticated plugin HTTP handler.
func (p *Plugin) Handler() http.Handler {
	return p.middleware(http.HandlerFunc(p.route))
}

func (p *Plugin) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := p.now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		info := &reqInfo{}
		r = r.WithContext(context.WithValue(r.Context(), reqInfoKey, info))
		if strings.TrimSuffix(r.URL.Path, "/") != "/health" && !p.authorized(r) {
			writeError(rec, http.StatusUnauthorized, "unauthorized", "invalid or missing bearer token")
		} else {
			func() {
				defer func() {
					if v := recover(); v != nil {
						if !rec.wrote {
							writeError(rec, http.StatusInternalServerError, "internal_error", fmt.Sprintf("panic: %v", v))
						}
					}
				}()
				next.ServeHTTP(rec, r)
			}()
		}
		level := "info"
		if rec.status >= 500 {
			level = "error"
		} else if rec.status >= 400 {
			level = "warn"
		}
		// The host probes /health every 10s. Logging the successes drowns the
		// file: measured 2026-10-22, /health was 68% of a 10930-line day file,
		// which is exactly why a real failure becomes impossible to find. A
		// failing probe is still logged (that is the whole point of a probe), and
		// non-GET hits keep their records too.
		//
		// The trailing slash is trimmed for the same reason the auth check above
		// trims it: a probe that reaches this handler as "/health/" is the same
		// probe, and logging only one spelling would leave half the noise behind.
		if r.Method == http.MethodGet && strings.TrimSuffix(r.URL.Path, "/") == "/health" && rec.status < 400 {
			return
		}
		p.logLine(level, "http_request", map[string]any{
			"path":    r.URL.Path,
			"method":  r.Method,
			"account": info.account,
			"status":  rec.status,
			"dur_ms":  p.now().Sub(start).Milliseconds(),
			"req_id":  requestID(r),
			"hedge":   isHedge(r),
		})
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": msg, "type": typ, "code": typ},
	})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func (p *Plugin) logLine(level, msg string, extra map[string]any) {
	m := map[string]any{
		"ts":    p.now().Format(time.RFC3339),
		"level": level,
		"msg":   msg,
	}
	for k, v := range extra {
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	p.logMu.Lock()
	defer p.logMu.Unlock()
	if p.logw == nil {
		return
	}
	_, _ = p.logw.Write(append(b, '\n'))
}

func flushWriter(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func setSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
}
