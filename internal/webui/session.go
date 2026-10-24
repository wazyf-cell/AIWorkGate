package webui

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// handleSession exchanges the one-shot launch token from the tray-opened URL for
// the admin session cookie.
//
// It is the only /api route a client without a cookie may reach, which is why
// the token is compared in constant time, never logged and never echoed. A wrong
// token, an absent token and a missing body all answer the same 403, so a caller
// cannot distinguish "close" from "absent" and cannot turn this into an oracle.
//
// The token may arrive as ?t=<token> (what the host puts in the browser URL) or
// as a JSON body {"token":"..."} (what the SPA posts). One exchange per host
// start is enough: the cookie that comes back authorises every /api request
// until the process exits.
func (s *server) handleSession(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) {
		return
	}
	token := strings.TrimSpace(r.URL.Query().Get(launchTokenParam))
	if token == "" && requestCarriesJSON(r) {
		var body struct {
			Token string `json:"token"`
		}
		// A malformed body is not reported as a body error: it must be
		// indistinguishable from a wrong token.
		if !decodeJSON(w, r, &body) {
			writeError(w, http.StatusForbidden, "invalid launch token", "admin_session_required")
			return
		}
		token = strings.TrimSpace(body.Token)
	}
	if !s.validLaunchToken(token) {
		writeError(w, http.StatusForbidden, "invalid launch token", "admin_session_required")
		return
	}
	s.issueSessionCookie(w)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// requestCarriesJSON reports whether the request declares a JSON body. Checking
// first keeps decodeJSON from writing its own 400 for a body-less exchange.
func requestCarriesJSON(r *http.Request) bool {
	if r.Body == nil || r.ContentLength == 0 {
		return false
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type"))), "application/json")
}

// validLaunchToken reports whether token is the host's launch token, comparing
// in constant time. An empty token is always rejected, and the comparison is
// skipped entirely rather than comparing two empty strings.
func (s *server) validLaunchToken(token string) bool {
	if token == "" || s.session == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(s.session)) == 1
}
