package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// localStateTimeout bounds the read + upstream validation of a local_state
// import; the official store is tiny, so 20s only covers a slow network.
const localStateTimeout = 20 * time.Second

// rememberLogin marks a paste_code session as waiting for the operator's submit.
func (s *server) rememberLogin(sid string) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	if s.logins == nil {
		s.logins = map[string]time.Time{}
	}
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	for id, at := range s.logins {
		if now.Sub(at) > 15*time.Minute {
			delete(s.logins, id)
		}
	}
	s.logins[sid] = now
}

func (s *server) forgetLogin(sid string) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	delete(s.logins, sid)
}

func (s *server) loginPending(sid string) bool {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	_, ok := s.logins[sid]
	return ok
}

// cancelLogin drops the pending session and remembers that it was cancelled.
// Without the tombstone a later poll would fall through to "an account exists,
// so this is success" and claim a login the operator abandoned.
func (s *server) cancelLogin(sid string) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	if s.cancelled == nil {
		s.cancelled = map[string]time.Time{}
	}
	for id, at := range s.cancelled {
		if now.Sub(at) > 15*time.Minute {
			delete(s.cancelled, id)
		}
	}
	s.cancelled[sid] = now
	delete(s.logins, sid)
}

func (s *server) loginCancelled(sid string) bool {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	_, ok := s.cancelled[sid]
	return ok
}

// handleLoginStart reports how this plugin can obtain a credential.
//
// local_state performs the import inline (like OfficeAce): the host renders the
// success immediately, so a missing/corrupt client state is answered with a
// readable 4xx instead of a pending session that never resolves.
// paste_code stays available for an operator who has only a token at hand.
func (s *server) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	var in struct {
		Method string `json:"method"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in)

	switch strings.ToLower(strings.TrimSpace(in.Method)) {
	case "", "local_state":
		s.startLocalState(w, r)
	case "paste_code":
		session := newUUID()
		s.rememberLogin(session)
		writeJSON(w, http.StatusOK, map[string]any{
			"session_id":   session,
			"status":       "pending",
			"method":       "paste_code",
			"instructions": "把完成授权后的完整回调地址（含 code 参数）粘贴提交。提交后本插件会用上游校验再落库。",
			"fields": []map[string]any{{
				"name":        "code",
				"label":       "回调地址或 code",
				"type":        "text",
				"placeholder": "https://…?code=…",
			}},
		})
	default:
		// Anything else would be a promise this plugin cannot keep.
		writeError(w, http.StatusBadRequest, "unsupported_method",
			"ZCode 插件支持 local_state（读取本机客户端凭据）与 paste_code，不提供浏览器 OAuth 自动回调")
	}
}

// startLocalState imports the local client login and validates it upstream.
func (s *server) startLocalState(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), localStateTimeout)
	defer cancel()

	imp, err := s.importLocalState(ctx)
	if err != nil {
		writeLocalStateError(w, err)
		return
	}
	creds := credentials{
		JWT:       imp.Secret,
		Kind:      imp.Kind,
		Label:     imp.Label,
		UpdatedAt: time.Now().Unix(),
	}
	if imp.Kind == credentialKindAPIKey {
		creds.APIKey = imp.Secret
		creds.JWT = ""
		creds.BaseURL = apiKeyOrigin
	}
	if err := s.store.set(creds); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id":      newUUID(),
		"status":          "success",
		"method":          "local_state",
		"account_id":      "default",
		"source":          imp.Source,
		"key_fingerprint": fingerprint(imp.Secret),
		"note":            "已从本机 ZCode 客户端读取凭据并用上游校验通过；值为指纹，不落日志、不回传明文。",
	})
}

// writeLocalStateError maps the typed import error onto a readable 4xx.
func writeLocalStateError(w http.ResponseWriter, err error) {
	if lse, ok := err.(*localStateError); ok {
		writeError(w, lse.Status, lse.Code, lse.Message)
		return
	}
	writeError(w, http.StatusBadRequest, "local_state_failed", err.Error())
}

// handleLoginPoll answers GET /accounts/login/{sid} and accepts the pasted
// credential on POST /accounts/login/{sid}/submit.
func (s *server) handleLoginPoll(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/accounts/login/")
	parts := strings.SplitN(strings.Trim(rest, "/"), "/", 2)
	sid := parts[0]
	if sid == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "session id is required")
		return
	}
	if len(parts) == 2 && parts[1] == "submit" {
		s.handleLoginSubmit(w, r, sid)
		return
	}
	if r.Method == http.MethodDelete {
		s.cancelLogin(sid)
		writeJSON(w, http.StatusOK, map[string]any{"status": "failed", "error": "login cancelled"})
		return
	}
	if s.loginCancelled(sid) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "failed", "error": "login cancelled"})
		return
	}
	// A fresh paste_code session stays pending even when an older credential is
	// configured: otherwise the UI closes the dialog before the operator pastes.
	if s.loginPending(sid) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "pending",
			"message": "等待粘贴回调地址或 code；提交成功后会转为 success",
		})
		return
	}
	// Poll: report what is actually true rather than pretending to be pending.
	if s.store.configured() {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":     "success",
			"account_id": "default",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "pending",
		"message": "等待通过 /accounts/login/{sid}/submit 提交 token，或使用 local_state 读取本机登录态",
	})
}

// handleLoginSubmit validates a pasted token upstream, then stores it. The old
// implementation stored anything it was given and answered success, which made
// a typo indistinguishable from a working login.
func (s *server) handleLoginSubmit(w http.ResponseWriter, r *http.Request, sid string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST to submit")
		return
	}
	// A valid code pasted after the dialog was dismissed must not sign in; only
	// the tombstone can stop it, because the upstream would accept the token.
	if s.loginCancelled(sid) {
		writeError(w, http.StatusBadRequest, "login_cancelled", "登录已被取消，请重新发起 /accounts/login")
		return
	}
	var body struct {
		Values map[string]string `json:"values"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "cannot read values")
		return
	}
	token := firstNonEmpty(strings.TrimSpace(body.Values["jwt"]), strings.TrimSpace(body.Values["code"]))
	if i := strings.Index(token, "code="); i >= 0 {
		token = token[i+len("code="):]
		if j := strings.IndexAny(token, "&#"); j >= 0 {
			token = token[:j]
		}
	}
	if token == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "token is empty")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), localStateTimeout)
	defer cancel()
	if err := s.validateToken(ctx, token); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_credential", "上游拒绝了这个 token："+err.Error())
		return
	}
	if err := s.store.set(credentials{JWT: token, Kind: credentialKindJWT, Label: "ZCode",
		UpdatedAt: time.Now().Unix()}); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	s.forgetLogin(sid)
	creds, _ := s.store.get()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":          "success",
		"account_id":      "default",
		"key_fingerprint": fingerprint(creds.JWT),
	})
}

// validateToken accepts either a coding-plan api key (id.secret) or a ZCode
// desktop JWT and asks the matching origin whether it is real.
func (s *server) validateToken(ctx context.Context, token string) error {
	if s.validateOverride != nil {
		return s.validateOverride(token)
	}
	if strings.Contains(token, ".") && !strings.HasPrefix(token, "eyJ") {
		return s.client.validateAPIKey(ctx, apiKeyOrigin, token, "")
	}
	return s.client.validateJWT(ctx, originBase, token, "")
}
