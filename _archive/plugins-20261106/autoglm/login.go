package main

// Login flows.
//
// Three real methods, all ported from the reference (internal/autoclaw):
//
//	phone_sms             cn      /userapi/v1/agent-send-code then /userapi/v1/agent-login/
//	global_import_desktop global  reads ~/.openclaw-autoclaw/{request-headers,openclaw}.json
//	global_oauth          global  POST /userapi/overseasv1/google-oauth-login with a callback URL
//
// A flow is tracked per session so the poll can tell "still waiting" from "this
// submit failed". Success is reported only after a credential is actually
// persisted, and the returned account_id is the id GET /accounts lists.
//
// Failures are HTTP-status-coded rather than a 200 with an "error" key: the host
// turns a non-2xx into the message the operator sees, and a 200 would look like
// a completed login.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// loginFlow is one in-flight login.
type loginFlow struct {
	Method    string
	Status    string // pending / success / failed
	Message   string
	Fields    []map[string]any
	AccountID string
	Err       string
	DeviceID  string
	UpdatedAt time.Time
}

// flowFor returns a copy: poll and submit both read the flow while the other
// side mutates it, and loginMu only protects the map lookup.
func (s *server) flowFor(sid string) (loginFlow, bool) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	flow, ok := s.logins[sid]
	if !ok {
		return loginFlow{}, false
	}
	return *flow, true
}

func (s *server) putFlow(sid string, flow *loginFlow) {
	flow.UpdatedAt = time.Now()
	s.loginMu.Lock()
	if s.logins == nil {
		s.logins = map[string]*loginFlow{}
	}
	s.logins[sid] = flow
	s.loginMu.Unlock()
}

// handleLoginStart begins a login flow and reports the fields to render.
func (s *server) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	var in struct {
		Method string `json:"method"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in)
	method := firstNonEmpty(in.Method, "phone_sms")
	// The host passes the bare capability name ("oauth"); this plugin's real
	// branches are finer-grained. "oauth" means "show me the phone/import/oauth
	// choices", which is the phone form plus the note below.
	if method == "oauth" || method == "" {
		method = "phone_sms"
	}
	switch method {
	case "phone_sms", "global_import_desktop", "global_oauth":
	default:
		writeError(w, http.StatusBadRequest, "unsupported_method",
			"method "+method+" is not supported; use phone_sms, global_import_desktop or global_oauth")
		return
	}
	sid := newDeviceID()
	flow := &loginFlow{Method: method, Status: "pending", Fields: loginFields(method), DeviceID: newDeviceID()}
	s.putFlow(sid, flow)
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": sid,
		"status":     "pending",
		"method":     method,
		"methods":    []string{"phone_sms", "global_import_desktop", "global_oauth"},
		"fields":     flow.Fields,
		"instructions": "选一种方式填写下面的字段后提交：" +
			"phone_sms 先填手机号（发送验证码），再填手机号+验证码完成登录；" +
			"global_import_desktop 填 import；global_oauth 粘贴授权后的完整回调地址。",
		"note": "phone_sms: POST /accounts/login/{sid}/submit {\"values\":{\"phone\":\"...\"}} 获取短信，" +
			"再提交 {\"values\":{\"phone\":\"...\",\"code\":\"...\"}}；" +
			"global_import_desktop: {\"values\":{\"action\":\"import\"}}；" +
			"global_oauth: {\"values\":{\"callback_url\":\"http://.../?code=..&state=..\"}}",
	})
}

// loginFields is the form the dialog renders for one method.
func loginFields(method string) []map[string]any {
	switch method {
	case "global_import_desktop":
		return []map[string]any{
			{"name": "action", "label": "从本机导入", "type": "text", "placeholder": "import",
				"help": "读取 ~/.openclaw-autoclaw 下已有的登录态；填 import 即可"},
		}
	case "global_oauth":
		return []map[string]any{
			{"name": "callback_url", "label": "回调地址", "type": "text",
				"placeholder": "http://…/?code=…&state=…",
				"help":        "浏览器完成 Google 授权后，把跳转到的完整地址粘贴到这里"},
		}
	}
	return []map[string]any{
		{"name": "phone", "label": "手机号", "type": "text", "placeholder": "138…",
			"help": "cn 区域：先填手机号发送验证码，收到后连同验证码一起提交"},
		{"name": "code", "label": "短信验证码", "type": "text", "placeholder": "6 位数字"},
	}
}

// handleLoginPoll answers /accounts/login/{sid}[/submit].
func (s *server) handleLoginPoll(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/accounts/login/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	sid := parts[0]
	if sid == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "session id is required")
		return
	}
	if len(parts) >= 2 && parts[1] == "submit" {
		s.handleLoginSubmit(w, r, sid)
		return
	}
	if r.Method == http.MethodDelete {
		s.loginMu.Lock()
		if flow := s.logins[sid]; flow != nil {
			flow.Status, flow.Err = "failed", "login cancelled"
		}
		s.loginMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"status": "failed", "error": "login cancelled"})
		return
	}
	flow, ok := s.flowFor(sid)
	if !ok {
		// A session started before a restart, already cancelled, or simply
		// unknown must never inherit success from a stored credential.
		if s.st.configured() {
			writeJSON(w, http.StatusOK, map[string]any{
				"status":  "failed",
				"message": "登录会话不存在或已过期，请重新发起 /accounts/login",
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "pending",
			"message": "尚无已存账号：请通过 /accounts/login 开始登录",
		})
		return
	}
	switch flow.Status {
	case "success":
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "success", "account_id": flow.AccountID, "label": flow.Message,
		})
	case "failed":
		writeJSON(w, http.StatusOK, map[string]any{"status": "failed", "error": flow.Err})
	default:
		out := map[string]any{"status": "pending", "method": flow.Method, "fields": flow.Fields}
		if flow.Message != "" {
			out["message"] = flow.Message
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// handleLoginSubmit runs one step of the flow's method.
func (s *server) handleLoginSubmit(w http.ResponseWriter, r *http.Request, sid string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	flow, ok := s.flowFor(sid)
	if !ok {
		writeError(w, http.StatusBadRequest, "login_not_found", "登录会话不存在或已过期，请重新发起 /accounts/login")
		return
	}
	if flow.Status == "failed" {
		writeError(w, http.StatusBadRequest, "login_cancelled", "登录已被取消，请重新发起 /accounts/login")
		return
	}
	var in struct {
		Values map[string]string `json:"values"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in)
	v := in.Values
	if v == nil {
		v = map[string]string{}
	}
	method := methodForValues(flow.Method, v)
	if method == "" {
		flow.Status, flow.Err = "failed", "unsupported values: need callback_url, action=import, or phone(+code)"
		s.putFlow(sid, &flow)
		writeError(w, http.StatusBadRequest, "bad_request", flow.Err)
		return
	}
	flow.Method = method

	accountID, message, err := s.runLoginStep(&flow, method, v)
	if err != nil {
		status, kind := loginErrorStatus(err)
		flow.Status, flow.Err, flow.Message = "failed", err.Error(), ""
		s.putFlow(sid, &flow)
		writeError(w, status, kind, err.Error())
		return
	}
	if accountID == "" {
		// A non-terminal step (sent code, import handshake, ...).
		flow.Message = message
		if method == "phone_sms" {
			flow.Fields = []map[string]any{
				{"name": "phone", "label": "手机号", "type": "text", "placeholder": "138…"},
				{"name": "code", "label": "短信验证码", "type": "text", "placeholder": "6 位数字",
					"help": "收到短信后连同手机号一起提交"},
			}
		}
		s.putFlow(sid, &flow)
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "pending", "method": method, "message": message, "fields": flow.Fields,
		})
		return
	}
	// Success is only claimed when the credential is really in the store.
	if s.st.byID(uidOf(accountID)) == nil {
		msg := "登录返回了账号 " + accountID + "，但凭据未落库；请重试或检查插件数据目录权限"
		flow.Status, flow.Err = "failed", msg
		s.putFlow(sid, &flow)
		writeError(w, http.StatusInternalServerError, "not_persisted", msg)
		return
	}
	flow.Status, flow.AccountID, flow.Message, flow.Err = "success", accountID, message, ""
	s.putFlow(sid, &flow)
	writeJSON(w, http.StatusOK, map[string]any{"status": "success", "account_id": accountID, "label": message})
}
