package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strconv"
	"time"
)

func pkcePair() (verifier, challenge string) {
	b := make([]byte, 48)
	if _, err := rand.Read(b); err != nil {
		copy(b, []byte(randomHex(48)))
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// listenLoopback binds an ephemeral loopback port. The reference client
// requires the callback port to be >= 10000; we retry when the OS hands out a
// lower privileged port.
func listenLoopback() (net.Listener, int, error) {
	var lastErr error
	for attempt := 0; attempt < 20; attempt++ {
		addr := "127.0.0.1:0"
		if attempt > 0 {
			n, err := cryptoInt(10000, 65000)
			if err == nil {
				addr = "127.0.0.1:" + strconv.Itoa(n)
			}
		}
		l, err := net.Listen("tcp4", addr)
		if err != nil {
			lastErr = err
			continue
		}
		port := l.Addr().(*net.TCPAddr).Port
		if port >= 10000 {
			return l, port, nil
		}
		_ = l.Close()
	}
	if lastErr == nil {
		lastErr = errors.New("could not allocate a callback port >= 10000")
	}
	return nil, 0, lastErr
}

func cryptoInt(min, max int) (int, error) {
	if max <= min {
		return 0, errors.New("invalid range")
	}
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return 0, err
	}
	n := int(uint32(buf[0])<<24|uint32(buf[1])<<16|uint32(buf[2])<<8|uint32(buf[3])) % (max - min)
	if n < 0 {
		n = -n
	}
	return min + n, nil
}

func buildAuthorizeURL(authorizeBase string, port int, challenge, ticket string) string {
	return fmt.Sprintf("%s?theme=%s&locale=%s&uri_scheme=%s&client_id=%s&port=%d&code_challenge=%s&code_challenge_method=SHA-256&ticket_id=%s&plugin-name=%s&plugin-version=%s",
		authorizeBase, oauthTheme, oauthLocale, oauthClientID, oauthClientID, port, challenge, ticket, loginPluginName, loginPluginVersion)
}

// buildIDEAuthorizeURL builds the JetBrains/doer flow. Unlike the portal flow,
// the browser first lands on Huawei's login page with the doer redirect as its
// service, and the doer redirect points back to this plugin's loopback
// callback. The reference client requires a UUID-shaped ticket here.
func buildIDEAuthorizeURL(port int, ticket string) string {
	callback := "http://127.0.0.1:" + strconv.Itoa(port) + ideCallbackPath
	redirect := fmt.Sprintf("%s?IdeaType=jetbrains&auth_callback_url=%s&plugin-name=%s&plugin-version=%s&ticket_id=%s",
		authorizeDoer, url.QueryEscape(callback), idePluginName, idePluginVersion, url.QueryEscape(ticket))
	return huaweiAuthBase + "?service=" + url.QueryEscape(redirect)
}

// randomUUID returns a v4 UUID. The doer redirect rejects a ticket that is not
// UUID-shaped, so the portal's 32-hex ticket cannot be reused here.
func randomUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return randomHex(32)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (p *Plugin) buildPortalLoginURL(succeeded bool) string {
	return fmt.Sprintf("%s?login_succeed=%t&uri_scheme=%s&locale=%s", p.endpoints.PortalLogin, succeeded, oauthClientID, oauthLocale)
}

func openBrowserDefault(rawURL string) error {
	switch runtime.GOOS {
	case "windows":
		cmd := exec.Command("cmd", "/c", "start", "", rawURL)
		return cmd.Start()
	case "darwin":
		return exec.Command("open", rawURL).Start()
	default:
		return exec.Command("xdg-open", rawURL).Start()
	}
}

func (p *Plugin) startLoginSession(method string) (*loginSession, error) {
	// Reuse a live session: every new session means a new callback port and a
	// new PKCE verifier/ticket, which silently invalidates the browser tab the
	// user is already standing in. Repeated "开始登录" clicks must not do that.
	if live := p.liveLoginSession(method); live != nil {
		return live, nil
	}
	entry, err := p.resolveEgress("login")
	if err != nil {
		return nil, err
	}
	proxyURL := ""
	if entry.Mode == egressProxy {
		proxyURL = entry.URL
	}
	listener, port, err := listenLoopback()
	if err != nil {
		return nil, fmt.Errorf("start loopback listener: %w", err)
	}
	key, err := generateDPoPKey()
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("generate DPoP key: %w", err)
	}
	verifier, challenge := pkcePair()
	ticket := randomHex(32)
	if method == "ide_plugin" {
		ticket = randomUUID()
	}
	now := p.now()
	s := &loginSession{
		ID:                  "ls_" + randomHex(8),
		Method:              method,
		Status:              "pending",
		CreatedAt:           now.Unix(),
		ExpiresAt:           now.Add(p.opts.LoginTTL).Unix(),
		port:                port,
		verifier:            verifier,
		challenge:           challenge,
		ticket:              ticket,
		key:                 key,
		proxyURL:            proxyURL,
		ticketPluginName:    loginPluginName,
		ticketPluginVersion: loginPluginVersion,
		listener:            listener,
	}
	if method == "ide_plugin" {
		s.ticketPluginName = idePluginName
		s.ticketPluginVersion = idePluginVersion
		s.URL = buildIDEAuthorizeURL(port, ticket)
	} else {
		s.URL = buildAuthorizeURL(p.endpoints.Authorize, port, challenge, ticket)
	}
	switch method {
	case "paste_code":
		s.Status = "need_input"
		s.Instructions = "在浏览器完成 CodeArts 登录后，把浏览器地址栏的完整回调地址粘贴到这里"
		s.Fields = []loginField{{
			Name:        "code",
			Label:       "回调地址或授权码",
			Type:        "text",
			Required:    true,
			Placeholder: "http://127.0.0.1:" + strconv.Itoa(port) + oauthRedirectPath + "?code=...",
			Help:        "也可以只粘贴 code 参数的值",
		}}
	case "ide_plugin":
		s.Instructions = "在浏览器完成华为账号登录；页面会自动回到本机，无需粘贴"
	default:
		s.Instructions = "在浏览器完成 CodeArts 登录"
	}

	mux := http.NewServeMux()
	if method == "ide_plugin" {
		mux.HandleFunc(ideCallbackPath, func(w http.ResponseWriter, r *http.Request) {
			p.handleIDECallback(s, w, r)
		})
	} else {
		mux.HandleFunc(oauthRedirectPath, func(w http.ResponseWriter, r *http.Request) {
			p.handleOAuthCallback(s, w, r)
		})
	}
	s.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	p.sessionsMu.Lock()
	p.sessions[s.ID] = s
	p.sessionsMu.Unlock()

	go func() {
		_ = s.server.Serve(listener)
	}()
	time.AfterFunc(p.opts.LoginTTL, func() {
		p.failSession(s, "CodeArts 登录超时（15 分钟）")
	})
	p.logLine("info", "login session started", map[string]any{
		"session": s.ID, "method": method, "port": port,
		"expires_in_seconds": int(p.opts.LoginTTL / time.Second),
	})
	return s, nil
}
