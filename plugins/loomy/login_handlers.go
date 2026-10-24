// Package main — the login surface the host actually calls.
//
// The host drives login through POST /accounts/login then GET
// /accounts/login/{sid}, and expects model.LoginStart / model.LoginStatus shapes
// with the status vocabulary pending | need_input | success | failed. DELETE on
// the session is the host cancelling an abandoned flow.
//
// Two behaviours are deliberate here:
//
//  1. A missing signing pair answers 400 with both variable names, never 501.
//     501 tells the host "this plugin has no login", which hides the entry and
//     leaves the user with no way to start the flow at all. The pair is only
//     needed for the exchange AFTER the scan, and the message says so.
//  2. Every other failure is reported as {"status":"failed","error":...} with
//     HTTP 200, because the poll contract carries the reason in the body. A
//     non-2xx here would be read by the host as a plugin fault and shown as
//     "登录等待超时" instead of the real cause.
package main

import (
	"net/http"
	"strings"
	"time"
)

// loginMethods are the methods this plugin really implements. oauth covers the
// WeChat scan; there is no sms/api_key/paste_code path (the reference has an SMS
// flow, but its code needs the same signing pair plus a phone form this plugin
// has no dialog for).
var loginMethods = []string{"oauth"}

// handleLoginStart begins one WeChat scan and hands the host a QR the user can
// actually open.
func (s *server) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	var in struct {
		Method string `json:"method"`
	}
	// A body is optional: the host sends {"method":"oauth"} but an empty POST is
	// also a valid "pick your default".
	_ = decodeOptionalJSON(r, &in)
	method := strings.ToLower(strings.TrimSpace(in.Method))
	if method == "" {
		method = "oauth"
	}
	if method != "oauth" {
		writeError(w, http.StatusBadRequest, "unsupported_method",
			"Loomy 只支持 oauth（微信扫码）登录，收到 "+method+"。"+
				"短信/手机号登录需要同样的讯飞签名密钥与一个手机号表单，本插件不提供；"+
				"若已有 session token，可 POST /admin/credentials 直接写入。")
		return
	}
	if !s.signing.complete() {
		// 400, not 501: the entry must stay visible so the message can be read.
		writeError(w, http.StatusBadRequest, "login_needs_signing_key",
			"微信扫码登录需要 Loomy 官方客户端的请求签名密钥：请先在本机设置环境变量 "+envAccessKeyID+" 与 "+envAccessKeySecret+
				"（不是讯飞开放平台账号里的 API Key；本插件不内置任何凭据），然后重启宿主。"+
				"扫码本身不需要密钥，但扫码后的换票必须签名，所以先配置再发起登录。")
		return
	}
	uuid, err := s.fetchWechatUUID(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "qr_unavailable", err.Error())
		return
	}
	sess := s.logins.start(uuid, time.Now())
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": sess.ID,
		"status":     "pending",
		"method":     "oauth",
		// The QR page itself: opening it shows the scannable code. It is a real
		// WeChat URL, not a local render, so nothing has to be proxied.
		"url":  qrImageURL(uuid),
		"code": uuid,
		"instructions": "用微信「扫一扫」扫描二维码并在手机上确认。" +
			"本页会自动检测，确认后账号自动出现。二维码 5 分钟内有效，过期请点「重新开始」。",
	})
}

// handleLoginPoll advances one pending scan. At most one long poll runs per call
// (bounded by wechatPollTimeout) so the host's own poll budget stays the outer
// limit, and the signed exchange only runs on the frame that carries wx_code.
func (s *server) handleLoginPoll(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/accounts/login/")
	// The host cancels an abandoned flow with DELETE /accounts/login/{sid}.
	if r.Method == http.MethodDelete {
		id = strings.Trim(id, "/")
		if id != "" {
			s.logins.remove(id)
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "failed", "error": "登录已取消"})
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET or DELETE")
		return
	}
	id = strings.Trim(id, "/")
	if id == "" || strings.Contains(id, "/") {
		writeError(w, http.StatusNotFound, "session_not_found", "登录会话不存在或已结束，请重新发起登录")
		return
	}
	sess, ok := s.logins.get(id)
	if !ok {
		// A session this plugin never issued must never inherit the success of an
		// earlier login: the dialog would close with nothing authorized.
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "failed", "error": "登录会话不存在或已过期，请重新发起登录",
		})
		return
	}
	switch sess.Status {
	case "success":
		writeJSON(w, http.StatusOK, map[string]any{"status": "success", "account_id": defaultAccountID})
		return
	case "failed":
		writeJSON(w, http.StatusOK, map[string]any{"status": "failed", "error": sess.Err})
		s.logins.remove(id)
		return
	}
	if time.Now().After(sess.ExpiresAt) {
		s.failSession(w, id, "二维码已过期（5 分钟未完成），请重新发起登录")
		return
	}
	if !s.signing.complete() {
		s.failSession(w, id, "缺少讯飞签名密钥，无法完成换票："+signingEnvHint())
		return
	}

	poll := s.pollWechat(r.Context(), sess.UUID, sess.LastCode)
	if poll.Errcode != "" {
		s.logins.update(id, func(ls *loginSession) { ls.LastCode = poll.Errcode })
	}
	switch poll.Status {
	case "confirmed":
		// claim keeps the one-time code from being spent by a second poll.
		if !s.logins.claim(id) {
			writeJSON(w, http.StatusOK, map[string]any{"status": "pending", "note": "正在完成登录…"})
			return
		}
		defer s.logins.release(id)
		res, err := s.exchangeCode(r.Context(), poll.Code)
		if err != nil {
			s.failSession(w, id, err.Error())
			return
		}
		if err := s.persistSession(res, res.Phone); err != nil {
			s.failSession(w, id, "登录成功但凭据落盘失败："+err.Error())
			return
		}
		s.logins.update(id, func(ls *loginSession) {
			ls.Status = "success"
			ls.WeChat = poll.Code
		})
		writeJSON(w, http.StatusOK, map[string]any{"status": "success", "account_id": defaultAccountID})
	case "scanned":
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "pending", "note": "已扫码，请在手机上点击确认",
		})
	case "cancelled":
		s.failSession(w, id, "你已取消授权，请重新发起登录")
	case "expired":
		s.failSession(w, id, "二维码已失效，请重新发起登录")
	default:
		// waiting, and a transport error too: both mean "keep polling". The
		// reference does the same so a flaky network never kills a login the user
		// already completed.
		writeJSON(w, http.StatusOK, map[string]any{"status": "pending"})
	}
}

// failSession records a terminal reason, answers it, and drops the session so it
// cannot be polled again.
func (s *server) failSession(w http.ResponseWriter, id, message string) {
	s.logins.update(id, func(ls *loginSession) {
		ls.Status = "failed"
		ls.Err = message
	})
	writeJSON(w, http.StatusOK, map[string]any{"status": "failed", "error": message})
	s.logins.remove(id)
}
