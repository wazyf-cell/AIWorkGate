package webui

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"aiworkgate/internal/model"
)

// actionCategory is the audit_log category for one user action. It is kept
// separate from "audit" (which records state changes) so the timeline can show
// "what the user clicked" without mixing it into the change history.
const actionCategory = "action"

// actionSkipPrefixes are paths whose calls are never recorded as user actions.
//
// Polling reads are excluded on purpose. The dashboard polls /api/status every
// few seconds, and the log views poll /api/logs/* and /api/usage/*. Capturing
// those would flood the timeline and bury the real actions -- and watching the
// logs must not itself generate log entries.
var actionSkipPrefixes = []string{
	"/api/status",
	"/api/logs/",
	"/api/usage/",
	"/api/session",
}

// recordAction logs one user action per state-changing admin API call.
//
// The admin UI has no per-button instrumentation and does not need any: every
// button that changes something issues an /api request, so recording the
// request is enough to reconstruct "what did the user click". Reads are skipped
// because they are not actions, and because almost all of them are polling.
//
// The entry is written after the handler ran, so it can carry the outcome. A
// failure to record never changes the response the client already received.
func (s *server) recordAction(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !actionWatched(r) || s.opts.RecordAction == nil {
			next.ServeHTTP(w, r)
			return
		}
		rec := &actionRecorder{ResponseWriter: w, status: http.StatusOK}
		started := time.Now()
		next.ServeHTTP(rec, r)
		s.opts.RecordAction(s.actionEntry(r, rec, time.Since(started)))
	})
}

// actionWatched reports whether this request is a user action worth recording:
// an admin API call that changes state, and is not a polling path.
func actionWatched(r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, apiPrefix) {
		return false
	}
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return false
	}
	for _, prefix := range actionSkipPrefixes {
		if strings.HasPrefix(r.URL.Path, prefix) {
			return false
		}
	}
	return true
}

// actionRecorder captures the status code and, for a failure only, the start of
// the response body. The body is what turns "4xx" into "why", so a call that
// never reached its host callback (an unknown plugin, a route the plugin does
// not implement) still leaves a readable trail.
type actionRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	reason      []byte
}

func (a *actionRecorder) WriteHeader(code int) {
	if !a.wroteHeader {
		a.status = code
		a.wroteHeader = true
	}
	a.ResponseWriter.WriteHeader(code)
}

func (a *actionRecorder) Write(p []byte) (int, error) {
	if !a.wroteHeader {
		a.wroteHeader = true
	}
	a.capture(p)
	return a.ResponseWriter.Write(p)
}

// capture keeps the first bytes of a failed response, and nothing else. A
// successful body (a plugin log page, an account list) is never buffered.
func (a *actionRecorder) capture(p []byte) {
	if a.status < 400 || len(a.reason) >= maxActionErrorBytes {
		return
	}
	if room := maxActionErrorBytes - len(a.reason); len(p) > room {
		p = p[:room]
	}
	a.reason = append(a.reason, p...)
}

// Flush keeps the SSE handlers working when they go through this wrapper.
func (a *actionRecorder) Flush() {
	if f, ok := a.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

const (
	// maxActionErrorBytes bounds the failed-response buffer.
	maxActionErrorBytes = 4 << 10
	// maxActionReasonRunes bounds what actually reaches the audit detail.
	maxActionReasonRunes = 200
)

// actionEntry builds the audit row for one recorded request. It carries the
// outcome, the wall time and, on a failure, the reason the handler answered with
// -- but never the request body, which routinely holds credentials.
func (s *server) actionEntry(r *http.Request, rec *actionRecorder, took time.Duration) model.AuditEntry {
	path := s.actionPath(r)
	detail := statusClass(rec.status) + " " + took.Round(time.Millisecond).String()
	if reason := actionReason(rec.reason); reason != "" {
		detail += " · " + reason
	}
	return model.AuditEntry{
		Category: actionCategory,
		Action:   r.Method + " " + path,
		Target:   path,
		Detail:   detail,
		Result:   statusClass(rec.status),
	}
}

// actionReason extracts one readable sentence from a failed response body: the
// error message of the standard envelope, or the first line of a plain-text
// answer. Control characters are stripped and the text is rune-capped, because
// this string is stored and later rendered in the timeline.
func actionReason(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	message := ""
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil && strings.TrimSpace(envelope.Error.Message) != "" {
		message = envelope.Error.Message
	} else {
		var plain struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &plain) == nil && strings.TrimSpace(plain.Error) != "" {
			message = plain.Error
		}
	}
	if message == "" {
		trimmed := strings.TrimSpace(string(body))
		if line, _, found := strings.Cut(trimmed, "\n"); found {
			message = line
		} else {
			message = trimmed
		}
	}

	var keep []rune
	for _, r := range message {
		if len(keep) >= maxActionReasonRunes {
			break
		}
		if r == '\n' || r == '\r' || r == '\t' || r < 0x20 {
			r = ' '
		}
		keep = append(keep, r)
	}
	return strings.TrimSpace(string(keep))
}

// idBearingResources are the admin resources whose first path segment after
// /api/<resource>/ is a user-supplied id (key id, plugin id, account id...).
// Those ids must never reach the audit log verbatim: a client key id in an URL
// is exactly the kind of secret that leaks through "harmless" logs.
var idBearingResources = map[string]bool{
	"keys":          true,
	"pools":         true,
	"groups":        true,
	"plugins":       true,
	"custom-models": true,
	"proxies":       true,
	"accounts":      false, // account routes carry the id in the body, not the path
}

// actionPathTemplate turns a concrete admin path into a stable template so a
// key id or plugin id in the URL never reaches the audit log.
//
//	/api/keys/agk_abc123                 -> /api/keys/{id}
//	/api/pools/officeace/models          -> /api/pools/{id}/models
//	/api/plugins/codearts/install        -> /api/plugins/{id}/install
//	/api/scheduler/pools/officeace       -> /api/scheduler/pools/{id}
func actionPathTemplate(path string) string {
	// Collapse duplicate slashes first: "/api//keys/x" would otherwise leave
	// segments[1] == "" and the id at index 3 would stay un-templated, leaking
	// the key id verbatim into the audit log.
	path = collapseSlashes(path)
	segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
	// segments[0] == "api"; the id-bearing slot is always index 2 for
	// /api/<resource>/<id>/..., and index 3 for /api/scheduler/<kind>/<id>.
	if len(segments) > 2 && idBearingResources[segments[1]] {
		segments[2] = "{id}"
	}
	if len(segments) > 3 && segments[1] == "scheduler" &&
		(segments[2] == "pools" || segments[2] == "models") {
		segments[3] = "{id}"
	}
	return "/" + strings.Join(segments, "/")
}

// actionPath chooses how much of the concrete URL reaches the audit row. Debug
// mode records the real plugin / account / pool id so a failure can be traced
// back to the thing it touched. Two cases always stay templated: a client key
// id, which looks exactly like a key, and any path that carries the session
// token.
func (s *server) actionPath(r *http.Request) string {
	templated := actionPathTemplate(r.URL.Path)
	if s == nil || s.opts.DebugActionIDs == nil || !s.opts.DebugActionIDs() {
		return templated
	}
	path := collapseSlashes(r.URL.Path)
	if s.opts.SessionToken != "" && strings.Contains(path, s.opts.SessionToken) {
		return templated
	}
	segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(segments) > 2 && segments[1] == "keys" {
		return templated
	}
	return path
}

// collapseSlashes folds duplicate separators so a logged path matches the route
// that answered it.
func collapseSlashes(path string) string {
	for strings.Contains(path, "//") {
		path = strings.ReplaceAll(path, "//", "/")
	}
	return path
}

// statusClass collapses a status code to its class so the audit row stays
// stable across e.g. 200 and 201.
func statusClass(status int) string {
	switch {
	case status >= 200 && status < 300:
		return "2xx"
	case status >= 400 && status < 500:
		return "4xx"
	case status >= 500 && status < 600:
		return "5xx"
	default:
		return "other"
	}
}
