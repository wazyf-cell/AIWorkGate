package main

// The device-login state machine: a WorkOS device authorization is polled in the
// background, and a paste_token form waits for a submit. Neither stores anything
// until the flow actually succeeds.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// loginSession is one in-flight login: a WorkOS device authorization, or a
// paste_token form that completes on submit.
type loginSession struct {
	auth      *deviceAuthorization
	createdAt time.Time
	method    string
	// status is pending until the poll loop succeeds or fails.
	status  string
	err     string
	account string
	stop    chan struct{}
	// finishedAt is when the terminal status was set, for the sweep.
	finishedAt time.Time

	// pollState/attempts are diagnostics exposed on GET /accounts/login/{sid}:
	// they prove the upstream poll is live without leaking the device code.
	pollState string
	attempts  int
}

// handleLoginStart begins a login: oauth (WorkOS device flow) or paste_code
// (paste an access token directly).
func (s *server) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	s.sweepLogins()
	var in struct {
		Method string `json:"method"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in)
	method := strings.ToLower(strings.TrimSpace(in.Method))
	if method == "" {
		method = "oauth"
	}
	switch method {
	case "oauth":
		if err := s.egressError(); err != nil {
			writeError(w, http.StatusForbidden, "egress_missing", err.Error())
			return
		}
		auth, err := s.client.deviceAuthorization()
		if err != nil {
			writeError(w, http.StatusBadGateway, "login_failed", err.Error())
			return
		}
		sessionID := newUUID()
		session := &loginSession{
			auth: auth, createdAt: time.Now(), method: "oauth", status: "pending", stop: make(chan struct{}),
		}
		s.loginMu.Lock()
		s.logins[sessionID] = session
		s.loginMu.Unlock()
		// Poll in the background so the host's /accounts/login/{sid} poll is cheap
		// and the operator's browser round trip does not hold an HTTP call open.
		go s.pollLogin(sessionID, session)
		s.log("info", "device_login_started", map[string]any{
			"session": sessionID, "url": auth.LoginURL(), "interval_s": int(auth.Interval.Seconds()),
		})
		writeJSON(w, http.StatusOK, map[string]any{
			"session_id":   sessionID,
			"status":       "pending",
			"method":       "oauth",
			"url":          auth.LoginURL(),
			"code":         auth.UserCode,
			"expires_in_s": int(auth.ExpiresIn.Seconds()),
			"interval_s":   int(auth.Interval.Seconds()),
			"instructions": "在浏览器打开 url 并输入 code 完成授权；插件会自动轮询 WorkOS 并在成功后注册 Cline 会话。",
		})
		return
	case "paste_code":
		// A paste session is only a holder for the session id. Submitting the form
		// stores the token; polling reports success afterwards, so the host sees
		// the same pending -> success transition as the device flow.
		sessionID := newUUID()
		s.loginMu.Lock()
		s.logins[sessionID] = &loginSession{
			createdAt: time.Now(), method: "paste_code", status: "pending",
		}
		s.loginMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"session_id": sessionID,
			"status":     "pending",
			"method":     "paste_code",
			"fields": []map[string]any{
				{
					"name":        "access_token",
					"label":       "Cline Access Token",
					"type":        "password",
					"required":    true,
					"placeholder": "WorkOS access token",
					"help":        "可带或不带 workos: 前缀，插件会归一化。",
				},
				{
					"name":     "refresh_token",
					"label":    "Refresh Token（可选）",
					"type":     "password",
					"required": false,
					"help":     "填了才能在过期后自动续期。",
				},
			},
			"instructions": "粘贴 access token（HAR/浏览器里 Authorization: Bearer workos:<token> 的后半段）。",
		})
		return
	}
	writeError(w, http.StatusBadRequest, "invalid_request_error",
		"method must be oauth or paste_code, got "+method)
}

// sleep is the device-flow timer seam: the test harness swaps it per server,
// so a background poll cannot read state another test is rewriting.
func (s *server) sleep(d time.Duration) <-chan time.Time {
	if s.pollSleep != nil {
		return s.pollSleep(d)
	}
	return time.After(d)
}

// step is the interval increase WorkOS asks for (reference: +1s).
func (s *server) step() time.Duration {
	if s.slowDownStep > 0 {
		return s.slowDownStep
	}
	return time.Second
}

// cancelled reports whether DELETE closed this session while a poll was in
// flight. The session is gone from the map, but the poll still has to stop
// before it registers anything.
func (session *loginSession) cancelled() bool {
	if session.stop == nil {
		return false
	}
	select {
	case <-session.stop:
		return true
	default:
		return false
	}
}

// pollLogin drives the WorkOS device flow in the background.
func (s *server) pollLogin(sessionID string, session *loginSession) {
	deadline := time.Now().Add(session.auth.ExpiresIn)
	interval := session.auth.Interval
	if interval < time.Second {
		interval = time.Second
	}
	failures, attempts := 0, 0
	for time.Now().Before(deadline) {
		select {
		case <-session.stop:
			return
		case <-s.sleep(interval):
		}
		tokens, state, err := s.client.pollDeviceTokens(session.auth.DeviceCode)
		attempts++
		s.recordPoll(sessionID, string(state), attempts)
		switch state {
		case pollPending:
			failures = 0
			continue
		case pollSlowDown:
			// WorkOS asks for a slower cadence; honour it instead of hammering.
			failures = 0
			interval += s.step()
			continue
		case pollTransport:
			// Five consecutive transport failures end the flow, the same bound the
			// reference uses (CLINE_POLL_MAX_FAILURES).
			failures++
			if failures >= 5 {
				s.finishLogin(sessionID, session, "failed", errText(err), "")
				return
			}
			continue
		case pollDenied:
			s.finishLogin(sessionID, session, "failed", "授权被拒绝："+errText(err), "")
			return
		case pollExpired:
			s.finishLogin(sessionID, session, "failed", "设备码已过期，请重新发起登录："+errText(err), "")
			return
		case pollFailed:
			s.finishLogin(sessionID, session, "failed", errText(err), "")
			return
		}
		// The operator may have cancelled while this poll was in flight; a grant
		// that arrives afterwards must not write a credential.
		if session.cancelled() {
			return
		}
		cred, err := s.client.register(*tokens)
		if err != nil {
			s.finishLogin(sessionID, session, "failed", err.Error(), "")
			return
		}
		if cred.AccessToken == "" {
			s.finishLogin(sessionID, session, "failed", "登录响应缺少访问令牌", "")
			return
		}
		if err := s.store.set(cred); err != nil {
			s.finishLogin(sessionID, session, "failed", err.Error(), "")
			return
		}
		s.log("info", "login_succeeded", map[string]any{
			"session": sessionID, "account": cred.AccountID, "fingerprint": fingerprint(cred.AccessToken),
		})
		s.finishLogin(sessionID, session, "success", "", "default")
		return
	}
	s.finishLogin(sessionID, session, "failed", "登录等待已超时，请重新发起登录", "")
}

// recordPoll stores the last upstream poll outcome for GET /accounts/login/{sid}.
func (s *server) recordPoll(sessionID, state string, attempts int) {
	s.loginMu.Lock()
	if session, ok := s.logins[sessionID]; ok {
		session.pollState, session.attempts = state, attempts
	}
	s.loginMu.Unlock()
}

// finishLogin records a terminal state for one session, including when it ended
// so a repeated poll can still be answered with the same result.
func (s *server) finishLogin(sessionID string, session *loginSession, status, errText, account string) {
	s.loginMu.Lock()
	session.status, session.err, session.account = status, errText, account
	session.finishedAt = time.Now()
	s.loginMu.Unlock()
}

// sweepLogins drops terminal sessions that have been answered long enough.
// Sessions are kept for a while so a duplicate poll after success is idempotent
// instead of looking like an unknown session.
func (s *server) sweepLogins() {
	now := time.Now()
	cutoff := now.Add(-10 * time.Minute)
	pasteCutoff := now.Add(-15 * time.Minute)
	s.loginMu.Lock()
	for id, session := range s.logins {
		switch {
		case !session.finishedAt.IsZero() && session.finishedAt.Before(cutoff):
			delete(s.logins, id)
		case session.method == "paste_code" && session.finishedAt.IsZero() &&
			!session.createdAt.IsZero() && session.createdAt.Before(pasteCutoff):
			// An abandoned paste dialog never reaches a terminal state.
			delete(s.logins, id)
		}
	}
	s.loginMu.Unlock()
}
