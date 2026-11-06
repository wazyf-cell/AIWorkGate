package main

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
)

// handleIDECallback serves the JetBrains/doer loopback callback. The auth page
// returns a `secret` that is exchanged for an AK/SK credential at the ticket
// endpoint; the raw `token`/`authCode` branch the reference also accepts cannot
// be signed with, so it is rejected instead of being reported as success.
func (p *Plugin) handleIDECallback(s *loginSession, w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	params := r.URL.Query()
	if fingerprint := strings.TrimSpace(params.Get("fingerprint")); fingerprint != "" {
		if decoded, err := base64.StdEncoding.DecodeString(fingerprint); err == nil {
			if inner, err := url.Parse(string(decoded)); err == nil {
				for key, values := range inner.Query() {
					params[key] = values
				}
			}
		}
	}
	if secret := strings.TrimSpace(params.Get("secret")); secret != "" {
		writeIDEPage(w, "登录已完成，可以回到 AIWorkGate。")
		go func() {
			ctx, cancel := context.WithTimeout(p.rootCtx, p.opts.LoginTTL)
			defer cancel()
			cred, err := p.pollTicketCredential(ctx, s, secret, 0)
			if err != nil {
				p.failSession(s, err.Error())
				return
			}
			if _, err := p.completeLogin(s, cred); err != nil {
				p.logLine("warn", "IDE plugin login failed", map[string]any{"error": err.Error()})
			}
		}()
		return
	}
	if token := firstNonEmpty(params.Get("token"), params.Get("access_token"),
		params.Get("accessToken"), params.Get("authCode")); token != "" {
		msg := "IDE 插件回调只返回了 token，没有可换取 AK/SK 的 secret；请改用「浏览器登录」完成授权。"
		p.failSession(s, msg)
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	http.Error(w, "登录回调缺少 secret", http.StatusBadRequest)
}

func writeIDEPage(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("<!doctype html><html><head><meta charset=\"utf-8\"><title>AIWorkGate</title></head>" +
		"<body style=\"font-family:sans-serif;padding:32px\"><h2>" + message + "</h2></body></html>"))
}
