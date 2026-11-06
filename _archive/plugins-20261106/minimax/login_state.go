package main

import (
	"net/http"
	"strings"
	"time"
)

// pollDiagnostic names the upstream answer, not the internal state: the device
// grant reports authorization_pending, and saying so on the poll endpoint is
// what proves the upstream poll is live.
func pollDiagnostic(status string) string {
	if status == "pending" {
		return "authorization_pending"
	}
	return status
}

// recordPoll stores the last upstream poll outcome for GET /accounts/login/{sid}.
func (s *server) recordPoll(id, state string, attempts int) {
	s.loginMu.Lock()
	if flow := s.logins[id]; flow != nil {
		flow.pollState, flow.attempts = state, attempts
	}
	s.loginMu.Unlock()
}

// sweepLogins drops terminal flows after they have been answerable for a while,
// and abandoned paste flows after their own deadline (they never reach a
// terminal state, so without this they would stay pending for the process
// lifetime and grow the map).
func (s *server) sweepLogins() {
	now := time.Now()
	cutoff := now.Add(-10 * time.Minute)
	pasteCutoff := now.Add(-15 * time.Minute)
	s.loginMu.Lock()
	for id, flow := range s.logins {
		switch {
		case !flow.finishedAt.IsZero() && flow.finishedAt.Before(cutoff):
			delete(s.logins, id)
		case flow.finishedAt.IsZero() && !flow.createdAt.IsZero() && flow.createdAt.Before(pasteCutoff):
			delete(s.logins, id)
		}
	}
	s.loginMu.Unlock()
}

func (s *server) accountBase() string {
	creds, _ := s.store.get()
	return creds.AccountBaseURL
}

func (s *server) setFlow(id, status string, grant *tokenGrant, errMsg string) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	flow := s.logins[id]
	if flow == nil {
		return
	}
	flow.status = status
	flow.token = grant
	flow.errMsg = errMsg
	if status != "pending" {
		flow.finishedAt = time.Now()
	}
}

// handleLoginPoll answers the login state for one flow, and accepts the
// paste-token submit.
func (s *server) handleLoginPoll(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/accounts/login/"), "/")
	parts := strings.Split(rest, "/")
	sid := strings.TrimSpace(parts[0])
	if sid == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "session id is required")
		return
	}
	if len(parts) > 1 && parts[1] == "submit" {
		s.handleLoginSubmit(w, r, sid)
		return
	}
	if r.Method == http.MethodDelete {
		s.loginMu.Lock()
		if flow := s.logins[sid]; flow != nil {
			if flow.cancel != nil {
				flow.cancel()
			}
			delete(s.logins, sid)
		}
		s.loginMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"status": "cancelled"})
		return
	}
	s.sweepLogins()
	s.loginMu.Lock()
	flow := s.logins[sid]
	var status, errMsg, pollState string
	var attempts int
	if flow != nil {
		status, errMsg, pollState, attempts = flow.status, flow.errMsg, flow.pollState, flow.attempts
	}
	s.loginMu.Unlock()

	if flow == nil {
		// Never claim success for an unknown id: success is only reported by a
		// flow this process actually completed.
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "failed", "message": "登录会话不存在或已过期，请重新发起 /accounts/login",
		})
		return
	}
	switch status {
	case "success":
		// The account id comes from what was persisted, so it matches /accounts.
		creds, _ := s.store.get()
		writeJSON(w, http.StatusOK, map[string]any{
			"status":     "success",
			"account_id": firstNonEmpty(creds.AccountID, "default"),
		})
	case "failed":
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "failed",
			"message": firstNonEmpty(errMsg, "登录失败"),
		})
	default:
		body := map[string]any{
			"status":  "pending",
			"message": "等待在浏览器完成授权",
		}
		if flow.auth.VerificationURIComplete != "" {
			body["url"] = flow.auth.VerificationURIComplete
			body["code"] = flow.auth.UserCode
		}
		if pollState != "" {
			// Diagnostics only: no device code, user code or token is included.
			body["poll"] = map[string]any{"state": pollState, "attempts": attempts}
		}
		writeJSON(w, http.StatusOK, body)
	}
}
