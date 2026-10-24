package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ---------------------------------------------------------------- login flow

// handleLoginStart begins the loopback OAuth flow. It answers immediately with the
// consent URL; the browser's redirect settles the flow.
func (s *server) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	flow, err := s.startLoopback()
	if err != nil {
		writeError(w, http.StatusBadGateway, "login_error", err.Error())
		return
	}
	sid := generateState()
	s.loginMu.Lock()
	s.logins[sid] = flow
	s.loginMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": sid,
		"status":     "pending",
		"method":     "oauth",
		"url":        flow.URL,
		"pkce":       flow.Verifier != "",
		// The operator can see up front whether the exchange will be refused
		// for a missing client secret, instead of after pasting a code.
		"client_secret_configured": clientSecretOrEmpty() != "",
		"instructions": "在浏览器打开 url 完成 Google 授权；回调会落到本机 loopback 端口，" +
			"插件随即自动换取 token。若浏览器无法回调，可用 /accounts/login/" + sid + "/submit 粘贴 code。",
	})
}

// handleLoginPoll answers the flow state, and accepts a pasted code on the
// /submit suffix (the reference's paste-code path).
func (s *server) handleLoginPoll(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/accounts/login/")
	parts := strings.SplitN(strings.Trim(rest, "/"), "/", 2)
	sid := parts[0]
	if sid == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "session id is required")
		return
	}
	s.loginMu.Lock()
	flow := s.logins[sid]
	s.loginMu.Unlock()
	if flow == nil {
		// A session the plugin never issued (or one already cancelled) must never
		// inherit success from a credential stored by an earlier login: the dialog
		// would close without the user having authorized anything.
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "failed",
			"message": "登录会话不存在或已过期，请重新发起 /accounts/login",
		})
		return
	}
	// The host cancels an abandoned login with DELETE /accounts/login/{sid}.
	if r.Method == http.MethodDelete {
		s.cancelLogin(sid)
		writeJSON(w, http.StatusOK, map[string]any{"status": "failed", "message": "login cancelled"})
		return
	}

	if len(parts) == 2 && parts[1] == "submit" {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
			return
		}
		var in struct {
			Values map[string]string `json:"values"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		code := strings.TrimSpace(in.Values["code"])
		if code == "" {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "values.code is required")
			return
		}
		// The operator may paste the whole callback URL, as the reference's
		// paste path accepts.
		if strings.Contains(code, "code=") {
			if u, perr := url.Parse(code); perr == nil {
				if v := u.Query().Get("code"); v != "" {
					code = v
				}
			}
		}
		if err := s.finishCode(r.Context(), flow, code); err != nil {
			s.settle(flow, "failed", err.Error())
			status, kind := exchangeErrorStatus(err)
			writeError(w, status, kind, err.Error())
			return
		}
		s.settle(flow, "success", "")
		writeJSON(w, http.StatusOK, map[string]any{"status": "success", "account_id": defaultAccountID})
		return
	}

	status, url, errMsg := s.flowStatus(flow)
	switch status {
	case "success":
		creds, _ := s.store.get()
		writeJSON(w, http.StatusOK, map[string]any{"status": "success", "account_id": "default", "label": creds.label()})
	case "failed":
		writeJSON(w, http.StatusOK, map[string]any{"status": "failed", "message": firstNonEmpty(errMsg, "登录失败")})
	default:
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "pending",
			"url":     url,
			"message": "等待在浏览器完成授权",
		})
	}
}

// cancelLogin stops a flow's listener (DELETE /accounts/login/{sid}).
func (s *server) cancelLogin(sid string) {
	s.loginMu.Lock()
	flow := s.logins[sid]
	delete(s.logins, sid)
	s.loginMu.Unlock()
	if flow != nil {
		s.settle(flow, "failed", "login cancelled")
	}
}
