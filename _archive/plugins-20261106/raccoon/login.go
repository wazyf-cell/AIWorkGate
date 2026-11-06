// Package main — Raccoon scan-code (QR) login.
//
// Ported from raccoon-oauth.ts / raccoon-login-page.ts in the reference:
//
//	code   := 16 random bytes, hex  (generateQrCode)
//	payload:= <apiBase>/login/mp?code=<code>&appname=商汤小浣熊官网 (buildQrImageUrl)
//	poll   := POST <apiBase>/api/web/auth/v1/login_with_qrcode_code {qrcode_code}
//
// The reference renders that payload as a QR inside its own loopback page; this
// plugin does the same at GET /qr/{sid} so the host UI's "打开登录页" button leads
// to something a phone can actually scan.  The payload URL is also returned as
// `qr_content`, and the raw code as `code`.
//
// Measured on 2026-10-22: the payload URL answers HTTP 404 in a desktop browser
// (it is a deep link for the phone app, not a web page), while the poll endpoint
// answers {"code":0,"data":{"status":"pending"}} for any code.  So `url` points
// at the local QR page, never at the 404 deep link.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// loginTimeout is RACCOON_LOGIN_TIMEOUT_MS = 5 minutes.
	loginTimeout = 5 * time.Minute
	// qrAppName is the appname query value buildQrImageUrl sends verbatim.
	qrAppName = "商汤小浣熊官网"
	// maxLoginSessions bounds the in-memory session map.
	maxLoginSessions = 16
)

// qrPayloadPath is buildQrImageUrl's path segment ("/login/mp").
const qrPayloadPath = "/login/mp"

// loginSession is one pending scan-code login.
type loginSession struct {
	ID        string
	QRCode    string
	Payload   string
	CreatedAt time.Time
	ExpiresAt time.Time
	Status    string
	ScannedAt time.Time
	AccountID string
	Err       string
}

type loginManager struct {
	mu       sync.Mutex
	sessions map[string]*loginSession
}

func newLoginManager() *loginManager {
	return &loginManager{sessions: map[string]*loginSession{}}
}

// start creates a session with a fresh QR code.
func (m *loginManager) start(now time.Time) *loginSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(now)
	s := &loginSession{
		ID:        randomHex(16),
		QRCode:    randomHex(16),
		CreatedAt: now,
		ExpiresAt: now.Add(loginTimeout),
		Status:    "pending",
	}
	s.Payload = buildQRPayload(s.QRCode, "")
	m.sessions[s.ID] = s
	return s
}

func (m *loginManager) get(id string) (*loginSession, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	return s, ok
}

// snapshot copies one session under the manager lock: the poll handler reads it
// while the QR callback (complete/fail) mutates the same struct.
func (m *loginManager) snapshot(id string) (loginSession, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return loginSession{}, false
	}
	return *s, true
}

func (m *loginManager) update(id string, fn func(*loginSession)) (*loginSession, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, false
	}
	fn(s)
	return s, true
}

func (m *loginManager) remove(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
}

// pruneLocked drops finished and stale sessions so the map cannot grow.
func (m *loginManager) pruneLocked(now time.Time) {
	for id, s := range m.sessions {
		if now.Sub(s.CreatedAt) > loginTimeout+time.Minute {
			delete(m.sessions, id)
		}
	}
	for len(m.sessions) >= maxLoginSessions {
		var oldest string
		var at time.Time
		for id, s := range m.sessions {
			if oldest == "" || s.CreatedAt.Before(at) {
				oldest, at = id, s.CreatedAt
			}
		}
		delete(m.sessions, oldest)
	}
}

// buildQRPayload is buildQrImageUrl: <apiBase>/login/mp?code=..&appname=..
// The parameter order is kept as the reference builds it (code first), and the
// values are escaped the way URLSearchParams does.
func buildQRPayload(code, base string) string {
	return originOf(base) + qrPayloadPath +
		"?code=" + url.QueryEscape(code) +
		"&appname=" + url.QueryEscape(qrAppName)
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failure is fatal for identity material; fall back to a
		// timestamp so the flow still yields a unique, non-repeating value.
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

// decodeJWTExpMs ports decodeJwtExpMs: the token's exp claim in milliseconds,
// or "" when the token is not a decodable JWT.
// ---------------------------------------------------------------- handlers

// loginMethods are the methods this plugin really implements.
var loginMethods = []string{"oauth"}

func (s *server) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	var in struct {
		Method string `json:"method"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in)
	method := strings.ToLower(strings.TrimSpace(in.Method))
	if method == "" {
		method = "oauth"
	}
	if method != "oauth" {
		writeError(w, http.StatusBadRequest, "unsupported_method",
			fmt.Sprintf("Raccoon 只支持 oauth（二维码扫码）登录，收到 %q。"+
				"短信登录需要阿里云滑块验证码 + 手机号 AES 密钥（本插件不持有），"+
				"若已有 access_token，请 POST /admin/credentials 写入。", method))
		return
	}

	sess := s.logins.start(time.Now())
	entry := "http://127.0.0.1:" + s.cfg.Port + "/qr/" + sess.ID
	if !s.localPortKnown() {
		entry = sess.Payload
	}
	instructions := "用 Raccoon（商汤小浣熊）手机 App 的扫一扫扫描二维码并在手机上确认；" +
		"本页每 2 秒自动检查，确认后账号会自动出现。二维码 5 分钟内有效，过期请点「重新开始」。"
	if !s.localPortKnown() {
		instructions = "打开 " + sess.Payload + " 用手机 App 扫码确认（插件未从宿主拿到端口，无法提供本地二维码页）。"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id":   sess.ID,
		"status":       "pending",
		"method":       "oauth",
		"url":          entry,
		"qr_content":   sess.Payload,
		"code":         sess.QRCode,
		"instructions": instructions,
	})
}

func (s *server) localPortKnown() bool {
	port, err := strconv.Atoi(strings.TrimSpace(s.cfg.Port))
	return err == nil && port > 0
}

// handleLoginPoll asks the upstream what the phone did with the code.
//
// The reference treats every unexpected answer as "pending" (pollRaccoonQrLogin
// returns pending on transport error, non-zero code and unknown status), so a
// flaky network never kills a login the user already completed.
func (s *server) handleLoginPoll(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		s.logins.remove(strings.TrimPrefix(r.URL.Path, "/accounts/login/"))
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET or DELETE")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/accounts/login/")
	sess, ok := s.logins.snapshot(id)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown_session", "登录会话不存在或已结束，请重新发起登录")
		return
	}
	switch sess.Status {
	case "success":
		writeJSON(w, http.StatusOK, map[string]any{"status": "success", "account_id": sess.AccountID})
		return
	case "failed":
		writeJSON(w, http.StatusGone, loginErrorBody("qr_expired", sess.Err))
		s.logins.remove(id)
		return
	}
	if time.Now().After(sess.ExpiresAt) {
		s.fail(w, id, "二维码已过期（5 分钟未完成），请重新发起登录")
		return
	}

	creds, _ := s.store.get()
	status, err := s.client.pollQRCode(r.Context(), creds.BaseURL, sess.QRCode)
	if err != nil {
		// Transport/parse failure: stay pending exactly like the reference.
		writeJSON(w, http.StatusOK, map[string]any{"status": "pending", "note": "上游暂时无响应，继续等待"})
		return
	}
	// NOTE: the upstream's `expired_at` is deliberately NOT used to decide
	// expiry. The reference (raccoon-oauth.ts pollRaccoonQrLogin) only ever
	// carries it through for display, and the wire format has never been
	// observed; treating a relative value as an absolute instant faked a
	// "expired" verdict on the first poll after a scan. The only expiry this
	// plugin enforces is its own 5-minute session window above.
	//
	// `canceled` means the phone-side code went stale, NOT that the user ran out
	// of time: the reference answers it by minting a fresh code and re-rendering
	// the QR so the page stays usable (raccoon-login-page.ts:212-220 "否则用户卡
	// 死"). Ending the session here would strand the user, which is exactly the
	// "扫码后立刻显示过期" symptom.
	switch status.Status {
	case "canceled":
		fresh := randomHex(16)
		if _, ok := s.logins.update(id, func(x *loginSession) {
			x.QRCode = fresh
			x.Payload = buildQRPayload(fresh, creds.BaseURL)
			x.ExpiresAt = time.Now().Add(loginTimeout)
			x.Status = "pending"
		}); !ok {
			writeError(w, http.StatusNotFound, "unknown_session", "登录会话不存在")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "pending",
			"note":   "二维码已刷新，请重新扫码",
			"code":   fresh,
			"url":    "http://127.0.0.1:" + s.cfg.Port + "/qr/" + id,
		})
	case "success":
		if strings.TrimSpace(status.AccessToken) == "" {
			// The reference keeps polling until a token shows up.
			writeJSON(w, http.StatusOK, map[string]any{"status": "pending"})
			return
		}
		s.complete(w, id, status)
	case "logging":
		if _, ok := s.logins.update(id, func(x *loginSession) {
			x.ScannedAt = time.Now()
		}); ok {
			writeJSON(w, http.StatusOK, map[string]any{
				"status": "pending", "note": "已扫码，请在手机上点击确认",
			})
			return
		}
		writeError(w, http.StatusNotFound, "unknown_session", "登录会话不存在")
	default:
		writeJSON(w, http.StatusOK, map[string]any{"status": "pending"})
	}
}

// complete persists the credentials and the account the host will schedule.
