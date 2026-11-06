package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// loginTTL mirrors LOBSTERAI_LOGIN_TIMEOUT_MS (10 minutes).
const loginTTL = 10 * time.Minute

// pendingLogin is one browser flow waiting for its callback.
type pendingLogin struct {
	Session   loginSession
	Created   time.Time
	Done      bool
	Cancelled bool
}

// loginState keeps the flows this process started. The map is keyed by the
// OAuth `state` value, which is what the portal echoes back; the exchange
// itself uses Session.UUID (a different value), matching the reference.
type loginState struct {
	mu      sync.Mutex
	pending map[string]*pendingLogin
}

func (l *loginState) put(state string, session loginSession) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pending == nil {
		l.pending = map[string]*pendingLogin{}
	}
	now := time.Now()
	for k, v := range l.pending {
		if now.Sub(v.Created) > loginTTL {
			delete(l.pending, k)
		}
	}
	l.pending[state] = &pendingLogin{Session: session, Created: now}
}

func (l *loginState) get(state string) (pendingLogin, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	v, ok := l.pending[state]
	if !ok || time.Since(v.Created) > loginTTL {
		return pendingLogin{}, false
	}
	return *v, true
}

// markCancelled remembers that the operator dismissed this flow. A later poll
// must not fall through to "a credential exists, so this is success".
func (l *loginState) markCancelled(state string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if v, ok := l.pending[state]; ok {
		v.Cancelled = true
	}
}

func (l *loginState) markDone(state string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if v, ok := l.pending[state]; ok {
		v.Done = true
	}
}

// handleLoginStart returns the portal URL plus the manual code field.
//
// Two things were wrong before: the response carried only `login_url` (the host
// reads `url`, so the link was dropped — M1), and the redirect_uri pointed at a
// /auth/callback route the plugin never served, so the browser landed on a 404
// (M4). Both are fixed: `url` is the canonical field, and the callback is real.
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
	case "", "oauth", "paste_code":
	default:
		writeError(w, http.StatusBadRequest, "unsupported_method",
			"LobsterAI 只支持门户登录（oauth）或粘贴回调 code（paste_code）")
		return
	}

	redirectURI := "http://127.0.0.1:" + firstNonEmpty(s.cfg.Port, "0") + callbackPath
	state := newUUIDHex()
	session := loginSession{UUID: newUUIDHex(), FirstKeyfrom: strconvNow(), LatestKeyfrom: strconvNow()}
	s.login.put(state, session)

	q := url.Values{}
	q.Set("source", "electron")
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	loginURL := portalBase + "/portal#/login?" + q.Encode()

	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": state,
		"status":     "pending",
		"method":     "oauth",
		// url is what the host and UI read; login_url stays for compatibility.
		"url":           loginURL,
		"login_url":     loginURL,
		"redirect_uri":  redirectURI,
		"first_keyfrom": session.FirstKeyfrom,
		"instructions": "在浏览器打开该链接完成登录，成功后会自动跳回本插件的回调并换取凭据；" +
			"若浏览器未跳转，可把回调地址里的 code 粘贴到下面提交。",
		"fields": []map[string]any{{
			"name":        "code",
			"label":       "回调 code（可选）",
			"type":        "text",
			"placeholder": "https://…?code=…",
			"help":        "只有回调没有自动完成时才需要手动粘贴",
		}},
		"note": "state 与 uuid 是两个独立值（与参考实现一致）：state 只做 CSRF 校验，uuid 进入换取请求。",
	})
}

// doExchange performs the portal code exchange, honouring the test override and
// the account's egress binding.
func (s *server) doExchange(ctx context.Context, base, code string, session loginSession) (credentials, error) {
	if s.exchangeOverride != nil {
		return s.exchangeOverride(ctx, code, session)
	}
	proxy, err := s.proxyFor("default")
	if err != nil {
		return credentials{}, err
	}
	version := s.client.clientVersion(ctx, proxy)
	return s.client.exchangeAuthCode(ctx, base, code, session, version, proxy)
}

// handleAuthCallback is the loopback endpoint the portal redirects to. It
// performs the exchange server-side, so the operator does not have to copy a
// code out of the address bar.
func (s *server) handleAuthCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "use GET", http.StatusMethodNotAllowed)
		return
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	pend, ok := s.login.get(state)
	if code == "" || state == "" || !ok || pend.Cancelled {
		writeCallbackHTML(w, http.StatusBadRequest, "登录回调参数无效：state 与本次登录不匹配、已过期或已被取消，请重新发起登录。")
		return
	}
	creds, err := s.doExchange(r.Context(), "", code, pend.Session)
	if err != nil {
		status, _ := exchangeErrorStatus(err)
		writeCallbackHTML(w, status, "登录换取凭据失败："+err.Error())
		return
	}
	if err := s.store.set(creds); err != nil {
		writeCallbackHTML(w, http.StatusInternalServerError, "凭据落库失败："+err.Error())
		return
	}
	s.login.markDone(state)
	writeCallbackHTML(w, http.StatusOK, "登录成功，可以关闭此窗口了。")
}

// writeCallbackHTML renders a human-facing page. The message is escaped: an
// upstream error is untrusted text.
func writeCallbackHTML(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<html><body><h2>%s</h2></body></html>", html.EscapeString(message))
}

// handleLoginSubmit accepts values.code from the host's paste form and performs
// the same exchange the browser callback does.
func (s *server) handleLoginSubmit(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST to submit")
		return
	}
	var body struct {
		Values map[string]string `json:"values"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "cannot read values")
		return
	}
	code := strings.TrimSpace(body.Values["code"])
	if code == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "code is empty")
		return
	}
	if i := strings.Index(code, "code="); i >= 0 {
		code = code[i+len("code="):]
		if j := strings.IndexAny(code, "&#"); j >= 0 {
			code = code[:j]
		}
	}
	session := loginSession{UUID: newUUIDHex(), FirstKeyfrom: strconvNow(), LatestKeyfrom: strconvNow()}
	if pend, ok := s.login.get(sessionID); ok {
		if pend.Cancelled {
			// The operator dismissed this dialog; a late paste must not sign in.
			writeError(w, http.StatusBadRequest, "login_cancelled", "登录已被取消，请重新发起登录")
			return
		}
		session = pend.Session
	}
	creds, err := s.doExchange(r.Context(), "", code, session)
	if err != nil {
		status, kind := exchangeErrorStatus(err)
		writeError(w, status, kind, "exchange failed: "+err.Error())
		return
	}
	if err := s.store.set(creds); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	s.login.markDone(sessionID)
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "success", "account_id": "default",
		"key_fingerprint": fingerprint(creds.AccessToken),
	})
}

// exchangeErrorStatus keeps a rejected auth code a 4xx: the operator pasted
// something the portal does not accept, the plugin itself is fine.
func exchangeErrorStatus(err error) (int, string) {
	var rejected *exchangeRejectedError
	if errors.As(err, &rejected) {
		return http.StatusBadRequest, "invalid_credential"
	}
	return http.StatusBadGateway, "upstream_error"
}
