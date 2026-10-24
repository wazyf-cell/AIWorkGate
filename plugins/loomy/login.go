// Package main — Loomy WeChat scan login: QR acquisition + status long-poll.
//
// Ported from loomy-wechat.ts in the reference (buildLoomyWechatAuthUrl /
// extractLoomyWechatUuid / fetchLoomyWechatUuid / pollLoomyWechatOnce). Nothing
// in THIS file needs a secret: the QR uuid is scraped from the public auth page
// and the state machine is driven by the public long-poll endpoint. The signed
// exchange that turns a scanned code into a 讯飞 session lives in
// login_account.go and needs the operator-supplied
// LOOMY_XFYUN_ACCESS_KEY_ID / LOOMY_XFYUN_ACCESS_KEY_SECRET pair (see sign.go).
//
// ⚠️ The long-poll status semantics are the WeChat auth page's own JS switch
// (switch (window.wx_errcode)), and the reference documents reading them
// backwards as a real shipped defect ("扫码后显示已扫码但没有后续跳转"):
//
//	408 waiting   → keep polling
//	404 scanned   → scanned, user has NOT confirmed yet, keep polling
//	405 confirmed → wx_code is in THIS frame
//	403 cancelled → user cancelled
//	402 expired   → QR dead, start over
//
// The redirect_uri stays the official Loomy callback even though it answers 404:
// WeChat validates the redirect domain against a whitelist, and a local address
// produces "redirect_uri 参数错误". The code comes from the long poll, so the
// callback page is never actually loaded.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sync"
	"time"
)

const (
	// accountBase is LOOMY_ACCOUNT_BASE: the signed 讯飞 account API.
	accountBase = "https://account.xfinfr.com"
	// wechatAppID / wechatRedirectURI are buildLoomyWechatAuthUrl's constants.
	// The appid is public (it appears in every QR URL); the redirect is Loomy's.
	wechatAppID       = "wx18d60be432287cf8"
	wechatRedirectURI = "https://loomy.xunfei.cn/oauth/wechat/callback"
	// wechatUA is the browser UA the reference sends to open.weixin.qq.com.
	wechatUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	// longPollURL is the QR status long-poll endpoint.
	longPollURL = "https://long.open.weixin.qq.com/connect/l/qrconnect"
	// sessionTTLSeconds is LOOMY_SESSION_TTL_SECONDS (14 days): the value the
	// login request declares; the response carries no expiry of its own, so the
	// stored expires_at is computed locally from it.
	sessionTTLSeconds = 1209600
	// wechatPollTimeout stays below the host's 60s poll budget so one long poll
	// plus the signed exchange still fit inside a single host call.
	wechatPollTimeout = 25 * time.Second
	// loginWindow bounds how long a scan session stays usable.
	loginWindow = 5 * time.Minute
	// envAccessKeyID / envAccessKeySecret are the only accepted sources of the
	// 讯飞 signing pair. Nothing in this repo carries a value for either.
	envAccessKeyID     = "LOOMY_XFYUN_ACCESS_KEY_ID"
	envAccessKeySecret = "LOOMY_XFYUN_ACCESS_KEY_SECRET"
)

// wechatAuthPageURL builds the authorization page URL that carries the QR uuid.
// It is a variable only so a test can point it at a local handler; production
// always uses buildLoomyWechatAuthURL.
var wechatAuthPageURL = buildLoomyWechatAuthURL

// wechatPollEndpoint is the long-poll endpoint. A variable for the same reason:
// an end-to-end test needs a local long poll, and nothing else may change it.
var wechatPollEndpoint = longPollURL

var uuidPattern = regexp.MustCompile(`^[A-Za-z0-9_\-=+/]{6,64}$`)

// loginSession is one pending WeChat scan. Status uses the host's vocabulary:
// "pending" | "need_input" | "success" | "failed" (model.Login*).
type loginSession struct {
	ID        string
	UUID      string
	CreatedAt time.Time
	ExpiresAt time.Time
	// LastCode is the last wx_errcode, sent back as the long poll's `last`.
	LastCode string
	// WeChat is the one-time wx_code, kept so an exchange is never repeated.
	WeChat string
	// RCode is bind/auth's context; the phone-binding steps use it.
	RCode    string
	MsgID    string
	Nickname string
	Status   string
	Err      string
}

type loginManager struct {
	mu       sync.Mutex
	sessions map[string]*loginSession
	// busy marks the sessions whose signed exchange is already in flight, so two
	// overlapping host polls cannot exchange the same one-time code twice.
	busy map[string]bool
}

func newLoginManager() *loginManager {
	return &loginManager{sessions: map[string]*loginSession{}, busy: map[string]bool{}}
}

// claim marks a session as exchanging and reports whether this caller got it. It
// is what keeps a one-time wx_code from being spent twice by two overlapping
// polls: the second caller sees busy=true and answers "pending" instead.
func (m *loginManager) claim(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[id]; ok && s.Status == "pending" && !m.busy[id] {
		m.busy[id] = true
		return true
	}
	return false
}

func (m *loginManager) release(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.busy, id)
}

func (m *loginManager) start(uuid string, now time.Time) *loginSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.sessions {
		if now.Sub(s.CreatedAt) > loginWindow+time.Minute {
			delete(m.sessions, id)
		}
	}
	s := &loginSession{
		ID:        randomHex(16),
		UUID:      uuid,
		CreatedAt: now,
		ExpiresAt: now.Add(loginWindow),
		Status:    "pending",
	}
	m.sessions[s.ID] = s
	return s
}

// get returns a copy of the session, so a handler never mutates a struct another
// handler may be polling at the same time.
func (m *loginManager) get(id string) (loginSession, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return loginSession{}, false
	}
	return *s, true
}

func (m *loginManager) update(id string, fn func(*loginSession)) (loginSession, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return loginSession{}, false
	}
	fn(s)
	return *s, true
}

func (m *loginManager) remove(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
}

// buildLoomyWechatAuthURL is buildLoomyWechatAuthUrl verbatim (same parameter
// order, same fragment).
func buildLoomyWechatAuthURL(state string) string {
	return "https://open.weixin.qq.com/connect/qrconnect?appid=" + url.QueryEscape(wechatAppID) +
		"&redirect_uri=" + url.QueryEscape(wechatRedirectURI) +
		"&response_type=code&scope=snsapi_login&state=" + url.QueryEscape(state) + "#wechat_redirect"
}

// qrImageURL is buildLoomyWechatQrImageUrl: the QR picture WeChat serves for a
// uuid. Measured: a JPEG, so opening it in a browser shows a scannable QR.
func qrImageURL(uuid string) string {
	return "https://open.weixin.qq.com/connect/qrcode/" + url.QueryEscape(uuid)
}

// extractWechatUUID is extractLoomyWechatUuid: the QR uuid from the auth page.
// The page embeds it, so no JavaScript is executed.
func extractWechatUUID(html string) string {
	if html == "" {
		return ""
	}
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`/connect/qrcode/([A-Za-z0-9_\-=+/]+)`),
		regexp.MustCompile(`l/qrconnect\?uuid=([A-Za-z0-9_\-=+/]+)`),
	} {
		if m := re.FindStringSubmatch(html); len(m) == 2 && uuidPattern.MatchString(m[1]) {
			return m[1]
		}
	}
	return ""
}

func (s *server) fetchWechatUUID(ctx context.Context) (string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, wechatAuthPageURL(randomHex(16)), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", wechatUA)
	req.Header.Set("Referer", "https://open.weixin.qq.com/")
	resp, err := s.client.httpFor("").Do(req)
	if err != nil {
		return "", fmt.Errorf("微信授权页拉取失败：%w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("微信授权页返回 HTTP %d", resp.StatusCode)
	}
	uuid := extractWechatUUID(string(body))
	if uuid == "" {
		return "", fmt.Errorf("微信授权页未包含二维码 uuid（页面结构可能已变化）")
	}
	return uuid, nil
}

// wechatPoll is one long-poll answer.
type wechatPoll struct {
	Status  string
	Code    string
	Errcode string
}

func (s *server) pollWechat(ctx context.Context, uuid, last string) wechatPoll {
	q := url.Values{}
	q.Set("uuid", uuid)
	if last != "" {
		q.Set("last", last)
	}
	q.Set("_", fmt.Sprintf("%d", time.Now().UnixMilli()))
	reqCtx, cancel := context.WithTimeout(ctx, wechatPollTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, wechatPollEndpoint+"?"+q.Encode(), nil)
	if err != nil {
		return wechatPoll{Status: "error", Errcode: err.Error()}
	}
	req.Header.Set("User-Agent", wechatUA)
	req.Header.Set("Referer", buildLoomyWechatAuthURL(""))
	resp, err := s.client.httpFor("").Do(req)
	if err != nil {
		// A flaky long poll must not kill a scan the user already confirmed:
		// the caller treats "error" as "keep waiting".
		return wechatPoll{Status: "error", Errcode: err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	text := string(body)
	errcode := firstSubmatch(text, `wx_errcode\s*=\s*(\d+)`)
	code := firstSubmatch(text, `wx_code\s*=\s*'([^']*)'`)
	switch errcode {
	case "405":
		if code != "" {
			return wechatPoll{Status: "confirmed", Code: code, Errcode: errcode}
		}
		// 405 without a code is malformed; never exchange an empty code.
		return wechatPoll{Status: "scanned", Errcode: errcode}
	case "404":
		return wechatPoll{Status: "scanned", Errcode: errcode}
	case "403":
		return wechatPoll{Status: "cancelled", Errcode: errcode}
	case "402":
		return wechatPoll{Status: "expired", Errcode: errcode}
	}
	// 408 and every unknown value stay waiting: never report success by guess.
	return wechatPoll{Status: "waiting", Errcode: errcode}
}

func firstSubmatch(s, pattern string) string {
	m := regexp.MustCompile(pattern).FindStringSubmatch(s)
	if len(m) == 2 {
		return m[1]
	}
	return ""
}
