package main

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// credentialFromPastedCode accepts a full callback URL, a "code=..." fragment
// or the bare authorization code. Callback URLs that carry a legacy `secret`
// use the ticket credential endpoint.
func (p *Plugin) credentialFromPastedCode(ctx context.Context, s *loginSession, raw string) (Credential, error) {
	if msg := pastedCallbackProblem(raw, s.port); msg != "" {
		return Credential{}, errors.New(msg)
	}
	code, secret := parsePastedCredential(raw)
	if secret != "" {
		// A pasted secret is polled with a short budget: the ticket belongs to
		// whichever session generated the link, so a stale or foreign callback
		// must produce a clear error instead of blocking until the host gives up.
		return p.pollTicketCredential(ctx, s, secret, pasteTicketBudget)
	}
	if code == "" {
		return Credential{}, errors.New("没有在粘贴内容中找到 code 或 secret 参数")
	}
	return p.exchangeAuthorizationCode(ctx, s.ID, s.proxyURL, code, s.verifier, s.port, s.key)
}

// pasteTicketBudget is how long a pasted `secret` is polled before giving up.
const pasteTicketBudget = 12 * time.Second

// pastedCallbackProblem rejects the two paste mistakes that otherwise cost a
// full timeout: the portal's own address bar, and a callback for an account
// CodeArts refuses to authorize.
func pastedCallbackProblem(raw string, port int) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || (!strings.Contains(raw, "secret=") && !strings.Contains(raw, "code=")) {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	params := parsed.Query()
	if nested := params.Get("redirect"); nested != "" {
		if inner, ierr := url.Parse(nested); ierr == nil {
			for key, values := range inner.Query() {
				params[key] = values
			}
		}
	}
	if strings.EqualFold(params.Get("authorizationAvailable"), "false") {
		return "CodeArts 门户返回 authorizationAvailable=false：这个华为账号当前没有可用的智能编码授权（未开通、无席位或未实名）。换链接或重登都不会通过，请先在 codearts.huaweicloud.com 确认账号已开通该服务。"
	}
	if !strings.HasPrefix(parsed.Hostname(), "127.0.0.") && !strings.EqualFold(parsed.Hostname(), "localhost") {
		return ""
	}
	if port > 0 && parsed.Port() != strconv.Itoa(port) {
		shown := parsed.Port()
		if shown == "" {
			shown = "无"
		}
		return "这不是本次登录会话的回调地址（端口 " + shown + " ≠ " + strconv.Itoa(port) + "）。请点「开始登录」后用新弹出的链接完成登录，再立刻粘贴地址栏里的地址（华为门户自己的网址不算回调地址）。"
	}
	return ""
}

func parsePastedCredential(raw string) (code, secret string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	if strings.Contains(raw, "?") || strings.Contains(raw, "code=") || strings.Contains(raw, "secret=") {
		if u, err := url.Parse(raw); err == nil {
			q := u.Query()
			if code, secret := strings.TrimSpace(q.Get("code")), strings.TrimSpace(q.Get("secret")); code != "" || secret != "" {
				return code, secret
			}
		}
		if q, err := url.ParseQuery(strings.TrimPrefix(raw, "?")); err == nil {
			if code, secret := strings.TrimSpace(q.Get("code")), strings.TrimSpace(q.Get("secret")); code != "" || secret != "" {
				return code, secret
			}
		}
	}
	if strings.HasPrefix(raw, "secret=") {
		return "", strings.TrimSpace(strings.TrimPrefix(raw, "secret="))
	}
	if strings.HasPrefix(raw, "code=") {
		return strings.TrimSpace(strings.TrimPrefix(raw, "code=")), ""
	}
	return raw, ""
}
