package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

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

// statusRecorder captures status codes and forwards Flush for SSE.
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
func (p *Plugin) Handler() http.Handler { return p.middleware(http.HandlerFunc(p.route)) }

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
		// file (measured 2026-10-22: 68% of a day's lines were /health), which is
		// why a real failure becomes impossible to find. Failures still log, and
		// the trailing slash is trimmed so both spellings of the probe are
		// covered rather than just one.
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

func (p *Plugin) route(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if len(path) > 1 {
		path = strings.TrimSuffix(path, "/")
	}
	switch path {
	case "/health":
		if r.Method == http.MethodGet {
			p.handleHealth(w, r)
			return
		}
	case "/meta":
		if r.Method == http.MethodGet {
			p.handleMeta(w, r)
			return
		}
	case "/v1/models":
		if r.Method == http.MethodGet {
			p.handleModels(w, r)
			return
		}
	case "/v1/chat/completions":
		if r.Method == http.MethodPost {
			p.handleChat(w, r)
			return
		}
	case "/reload":
		if r.Method == http.MethodPost {
			p.handleReload(w, r)
			return
		}
	}
	segs := strings.Split(strings.Trim(path, "/"), "/")
	if len(segs) > 0 && segs[0] == "accounts" {
		p.routeAccounts(w, r, segs)
		return
	}
	p.handleNotFound(w, r)
}

func (p *Plugin) routeAccounts(w http.ResponseWriter, r *http.Request, segs []string) {
	switch {
	case len(segs) == 1 && r.Method == http.MethodGet:
		p.handleAccounts(w, r)
	case len(segs) == 2 && segs[1] == "login" && r.Method == http.MethodPost:
		p.handleLoginStart(w, r)
	case len(segs) == 2 && r.Method == http.MethodPatch:
		p.handleAccountPatch(w, r, segs[1])
	case len(segs) == 2 && r.Method == http.MethodDelete:
		p.handleAccountDelete(w, r, segs[1])
	case len(segs) == 3 && segs[1] == "login" && r.Method == http.MethodGet:
		p.handleLoginPoll(w, r, segs[2])
	case len(segs) == 3 && segs[1] == "login" && r.Method == http.MethodDelete:
		p.handleLoginCancel(w, r, segs[2])
	case len(segs) == 4 && segs[2] == "identity" &&
		(segs[3] == "reset" || segs[3] == "details") && r.Method == http.MethodPost:
		// The OfficeAce account is a Huawei IAM identity; this plugin never
		// mints or stores a device fingerprint of its own, so there is nothing
		// to read back or rotate. Say so with a stable code instead of letting
		// the host surface a bare 404 as a 502.
		writeError(w, http.StatusNotImplemented, "identity_not_applicable",
			"OfficeAce 账号是华为 IAM 身份，本插件不持有可重置的设备指纹；设备信任请在 OfficeAce 客户端内管理")
	case len(segs) == 3:
		id, action := segs[1], segs[2]
		switch {
		case action == "checkin" && r.Method == http.MethodPost:
			p.handleCheckin(w, r, id)
		case action == "refresh" && r.Method == http.MethodPost:
			p.handleRefresh(w, r, id)
		case action == "history" && r.Method == http.MethodGet:
			p.handleHistory(w, r, id)
		default:
			p.handleNotFound(w, r)
		}
	case len(segs) == 4 && segs[1] == "login" && segs[3] == "submit" && r.Method == http.MethodPost:
		p.handleLoginSubmit(w, r, segs[2])
	default:
		p.handleNotFound(w, r)
	}
}
