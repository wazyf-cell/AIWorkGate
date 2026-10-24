package main

// The device-authorization login flow (RFC 8628 device_code + PKCE, the shape
// MiniMax Code uses). Kept apart from server.go so the HTTP surface and the
// login state machine stay readable independently.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------- login flow

// sleep is the device-flow timer seam. It lives on the server so a background
// poll cannot observe a hook another test is swapping.
func (s *server) sleep(d time.Duration) <-chan time.Time {
	if s.pollSleep != nil {
		return s.pollSleep(d)
	}
	return time.After(d)
}

// step is the reference's +5 s interval increase on slow_down.
func (s *server) step() time.Duration {
	if s.slowDownStep > 0 {
		return s.slowDownStep
	}
	return 5 * time.Second
}

// handleLoginStart begins a login: oauth (device authorization) or paste_code.
//
// POST /accounts/login -> {"session_id":..., "url":..., "code":..., "status":"pending"}
// The operator opens url in a browser, approves, and the plugin's own polling loop
// then completes the flow. Nothing here stores a credential until the poll
// succeeds.
func (s *server) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	var in struct {
		Method string `json:"method"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in)
	method := strings.ToLower(strings.TrimSpace(in.Method))
	if method == "" {
		method = "oauth"
	}
	s.sweepLogins()
	switch method {
	case "paste_code":
		flowID := newUUID()
		s.loginMu.Lock()
		if s.logins == nil {
			s.logins = map[string]*loginFlow{}
		}
		s.logins[flowID] = &loginFlow{method: "paste_code", status: "pending", createdAt: time.Now()}
		s.loginMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"session_id": flowID,
			"status":     "pending",
			"method":     "paste_code",
			"fields": []map[string]any{
				{
					"name":     "access_token",
					"label":    "MiniMax access_token",
					"type":     "password",
					"required": true,
					"help":     "从 MiniMax Code 登录态里取出的 bearer token。",
				},
				{
					"name":     "refresh_token",
					"label":    "refresh_token（可选）",
					"type":     "password",
					"required": false,
					"help":     "填了才能在过期后自动续期。",
				},
			},
			"instructions": "粘贴 access_token；插件会保存并作为 Bearer 使用。",
		})
		return
	case "oauth":
		// continue below
	default:
		writeError(w, http.StatusBadRequest, "unsupported_method",
			"method must be oauth or paste_code, got "+method)
		return
	}
	if err := s.egressError(); err != nil {
		writeError(w, http.StatusForbidden, "egress_missing", err.Error())
		return
	}
	creds, _ := s.store.get()
	auth, err := s.client.startDeviceAuthorization(r.Context(), creds.AccountBaseURL)
	if err != nil {
		writeError(w, http.StatusBadGateway, "login_error", "device authorization failed: "+err.Error())
		return
	}
	flowID := newUUID()
	ctx, cancel := context.WithCancel(context.Background())
	flow := &loginFlow{auth: auth, method: "oauth", status: "pending", done: make(chan struct{}), cancel: cancel}
	s.loginMu.Lock()
	if s.logins == nil {
		s.logins = map[string]*loginFlow{}
	}
	s.logins[flowID] = flow
	s.loginMu.Unlock()

	go s.pollDevice(ctx, flowID, flow)

	writeJSON(w, http.StatusOK, map[string]any{
		"session_id":   flowID,
		"status":       "pending",
		"method":       "oauth",
		"url":          auth.VerificationURIComplete,
		"code":         auth.UserCode,
		"account_id":   "",
		"instructions": "在浏览器打开 url 并输入 code 完成授权，插件会自动轮询到成功",
		"interval_s":   auth.Interval,
		"expires_in_s": auth.ExpiresIn,
	})
}

// pollDevice drives the device-token loop in the background, honouring the
// pending/slow_down state machine the reference implements.
func (s *server) pollDevice(ctx context.Context, flowID string, flow *loginFlow) {
	defer close(flow.done)
	deadline := time.Now().Add(time.Duration(flow.auth.ExpiresIn) * time.Second)
	interval := time.Duration(flow.auth.Interval) * time.Second
	if interval <= 0 {
		interval = 3 * time.Second
	}
	attempts := 0
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			s.setFlow(flowID, "failed", nil, "login cancelled")
			return
		case <-s.sleep(interval):
		}
		grant, status, err := s.client.pollDeviceToken(ctx, s.accountBase(), flow.auth)
		attempts++
		if err != nil {
			s.recordPoll(flowID, "failed", attempts)
			s.setFlow(flowID, "failed", nil, err.Error())
			return
		}
		s.recordPoll(flowID, pollDiagnostic(status), attempts)
		switch status {
		case "pending":
			continue
		case "slow_down":
			interval += s.step()
			continue
		case "denied", "access_denied":
			s.setFlow(flowID, "failed", nil, "authorization denied")
			return
		case "expired", "expired_token":
			s.setFlow(flowID, "failed", nil, "device code expired")
			return
		}
		creds, _ := s.store.get()
		next := grantToCredentials(grant, creds)
		if err := s.store.set(next); err != nil {
			s.setFlow(flowID, "failed", nil, "cannot store credential: "+err.Error())
			return
		}
		g := grant
		s.setFlow(flowID, "success", &g, "")
		return
	}
	s.setFlow(flowID, "failed", nil, "device code expired")
}

// handleLoginSubmit stores a pasted access token for a paste_code flow.
func (s *server) handleLoginSubmit(w http.ResponseWriter, r *http.Request, sid string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	s.loginMu.Lock()
	flow, ok := s.logins[sid]
	method := ""
	if ok {
		method = flow.method
	}
	s.loginMu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "unknown_login_session", "登录会话不存在或已过期")
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
	token := strings.TrimSpace(firstNonEmpty(in.Values["access_token"], in.Values["token"]))
	if token == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "values.access_token is required")
		return
	}
	prev, _ := s.store.get()
	next := credentials{
		AccessToken:    token,
		RefreshToken:   strings.TrimSpace(in.Values["refresh_token"]),
		TokenType:      "Bearer",
		AccountID:      jwtSub(token),
		Nickname:       prev.Nickname,
		BaseURL:        prev.BaseURL,
		AccountBaseURL: prev.AccountBaseURL,
	}
	// The expiry is only recorded when the token actually carries one; inventing
	// a TTL would make a live token look expired (or vice versa).
	if exp, known := jwtExpMs(token); known {
		next.ExpiresAt = strconv.FormatInt(exp, 10)
	}
	// Validate before storing: a pasted token the API rejects must not leave a
	// pool entry that only fails on the first chat.
	if _, err := s.client.models(r.Context(), token, prev.BaseURL); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_credential",
			"上游拒绝了这把 token："+err.Error())
		return
	}
	if err := s.store.set(next); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	s.setFlow(sid, "success", nil, "")
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "success",
		"account_id": firstNonEmpty(next.AccountID, "default"),
	})
}
