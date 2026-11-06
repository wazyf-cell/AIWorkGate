package main

// The login polling/submit surface: GET or DELETE /accounts/login/{sid}, and the
// paste-token submit. Split from login.go so the state machine and the HTTP
// projection stay separately readable.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// handleLoginPoll answers one session, and accepts the paste-token submit.
func (s *server) handleLoginPoll(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/accounts/login/"), "/"), "/")
	sid := ""
	if len(parts) > 0 {
		sid = parts[0]
	}
	if sid == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "session id is required")
		return
	}
	if len(parts) > 1 && parts[1] == "submit" {
		s.handleLoginSubmit(w, r, sid)
		return
	}
	s.sweepLogins()
	if r.Method == http.MethodDelete {
		s.loginMu.Lock()
		if session, ok := s.logins[sid]; ok {
			if session.stop != nil {
				close(session.stop)
			}
			delete(s.logins, sid)
		}
		s.loginMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"status": "failed", "error": "login cancelled"})
		return
	}

	s.loginMu.Lock()
	session, ok := s.logins[sid]
	status, errText, account, pollState, attempts := "", "", "", "", 0
	if ok {
		status, errText, account = session.status, session.err, session.account
		pollState, attempts = session.pollState, session.attempts
	}
	s.loginMu.Unlock()

	switch {
	case !ok:
		// Never claim success for an id we do not know: an unknown session means
		// the login did not happen in this process.
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "failed", "error": "unknown login session; start the login again",
		})
	case status == "success":
		writeJSON(w, http.StatusOK, map[string]any{"status": "success", "account_id": account})
	case status == "failed":
		writeJSON(w, http.StatusOK, map[string]any{"status": "failed", "error": errText})
	default:
		body := map[string]any{"status": "pending"}
		if pollState != "" {
			// Diagnostics only: no device code, user code or token is included.
			body["poll"] = map[string]any{"state": pollState, "attempts": attempts}
		}
		if session.method == "paste_code" {
			body["message"] = "等待提交 access_token"
		}
		writeJSON(w, http.StatusOK, body)
	}
}

// handleLoginSubmit stores a pasted access token for a paste_code session.
func (s *server) handleLoginSubmit(w http.ResponseWriter, r *http.Request, sid string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	s.loginMu.Lock()
	session, ok := s.logins[sid]
	method := ""
	if ok {
		method = session.method
	}
	s.loginMu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "unknown_login_session",
			"登录会话不存在或已过期，请重新发起登录")
		return
	}
	if method != "paste_code" {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			"this session uses the oauth flow; submit is only for paste_code")
		return
	}
	var in struct {
		Values map[string]string `json:"values"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	token := bareToken(firstNonEmpty(in.Values["access_token"], in.Values["api_key"]))
	if token == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			"values.access_token is required")
		return
	}
	// A pasted token must answer one authenticated call before it is stored:
	// reporting success for a token the API rejects would leave the operator
	// with a pool entry that fails on the first chat. The catalog is anonymous,
	// so the probe goes through inference.
	ctx, cancel := context.WithTimeout(r.Context(), authTimeout)
	err := s.client.probeToken(ctx, token)
	cancel()
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_credential",
			"上游拒绝了这把 token："+err.Error())
		return
	}
	cred := &clineCredential{AccessToken: token, Nickname: "Cline（粘贴 token）"}
	if refresh := strings.TrimSpace(in.Values["refresh_token"]); refresh != "" {
		cred.RefreshToken = refresh
	}
	if err := s.store.set(cred); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	s.finishLogin(sid, session, "success", "", egressAccountID)
	s.log("info", "credential_stored", map[string]any{"fingerprint": fingerprint(token)})
	writeJSON(w, http.StatusOK, map[string]any{"status": "success", "account_id": egressAccountID})
}
