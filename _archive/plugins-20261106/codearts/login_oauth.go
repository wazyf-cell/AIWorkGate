package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func (p *Plugin) handleOAuthCallback(s *loginSession, w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	q := r.URL.Query()
	if secret := strings.TrimSpace(q.Get("secret")); secret != "" {
		redirectTo := strings.TrimSpace(q.Get("redirect"))
		if redirectTo == "" {
			redirectTo = p.buildPortalLoginURL(true)
		}
		w.Header().Set("Location", redirectTo)
		w.WriteHeader(http.StatusTemporaryRedirect)
		go func() {
			ctx, cancel := context.WithTimeout(p.rootCtx, p.opts.LoginTTL)
			defer cancel()
			cred, err := p.pollTicketCredential(ctx, s, secret, 0)
			if err != nil {
				p.failSession(s, err.Error())
				return
			}
			if _, err := p.completeLogin(s, cred); err != nil {
				p.logLine("warn", "ticket login failed", map[string]any{"error": err.Error()})
			}
		}()
		return
	}
	code := strings.TrimSpace(q.Get("code"))
	if code == "" {
		http.Error(w, "Missing authorization code or secret", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	cred, err := p.exchangeAuthorizationCode(ctx, s.ID, s.proxyURL, code, s.verifier, s.port, s.key)
	if err != nil {
		p.failSession(s, err.Error())
		w.Header().Set("Location", p.buildPortalLoginURL(false))
		w.WriteHeader(http.StatusTemporaryRedirect)
		return
	}
	if _, err := p.completeLogin(s, cred); err != nil {
		w.Header().Set("Location", p.buildPortalLoginURL(false))
		w.WriteHeader(http.StatusTemporaryRedirect)
		return
	}
	w.Header().Set("Location", p.buildPortalLoginURL(true))
	w.WriteHeader(http.StatusTemporaryRedirect)
}

// pollTicketCredential implements the legacy portal ticket flow. It is only
// used when a callback carries `secret`; the modern OAuth flow returns a code.
// The budget is short for pasted secrets and the full session TTL for a live
// callback, where the browser is waiting on this very ticket.
func (p *Plugin) pollTicketCredential(ctx context.Context, s *loginSession, secret string, budget time.Duration) (Credential, error) {
	if budget <= 0 {
		budget = p.opts.LoginTTL
	}
	deadline := time.Now().Add(budget)
	const maxAttempts = 120
	for i := 0; i < maxAttempts; i++ {
		if time.Now().After(deadline) {
			break
		}
		if i > 0 {
			select {
			case <-ctx.Done():
				return Credential{}, ctx.Err()
			case <-time.After(time.Second):
			}
		}
		u := p.endpoints.Ticket + "?ticket_id=" + url.QueryEscape(s.ticket) + "&secret=" + url.QueryEscape(secret)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return Credential{}, err
		}
		req.Header.Set("Content-Type", "application/json;charset=UTF-8")
		// The poll must present the same plugin identity that minted the
		// ticket: snap_AIIDE/5.2.0 for the portal channel and
		// snap_jetbrains/26.3.3 for the IDE plugin channel.
		name, version := s.ticketPluginName, s.ticketPluginVersion
		if name == "" {
			name, version = loginPluginName, loginPluginVersion
		}
		req.Header.Set("plugin-name", name)
		req.Header.Set("plugin-version", version)
		resp, err := p.httpClientFor(s.ID, s.proxyURL).Do(req)
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal(body, &data); err != nil {
			continue
		}
		if cred, ok := parseTicketCredential(data); ok {
			return cred, nil
		}
	}
	if budget < p.opts.LoginTTL {
		return Credential{}, errors.New("CodeArts 没有为这个 secret 返回凭据：它多半来自另一次（已过期或被替换的）登录。请重新点「开始登录」，在**新打开的页面**里完成登录后，立刻把地址栏的回调地址粘贴进来。")
	}
	return Credential{}, errors.New("CodeArts 登录票据轮询超时")
}

func parseTicketCredential(data map[string]any) (Credential, bool) {
	if c, ok := data["credential"].(map[string]any); ok {
		access, _ := c["access"].(string)
		st, _ := c["securitytoken"].(string)
		if st == "" {
			st, _ = c["securityToken"].(string)
		}
		if access != "" && st != "" {
			sec, _ := c["secret"].(string)
			exp, _ := c["expires_at"].(string)
			if exp == "" {
				exp, _ = c["expiresAt"].(string)
			}
			cred := Credential{
				AccessKeyID:     access,
				SecretAccessKey: sec,
				SecurityToken:   st,
				ExpiresAt:       exp,
			}
			cred.DomainID, _ = data["domain_id"].(string)
			cred.UserID, _ = data["user_id"].(string)
			cred.UserName, _ = data["user_name"].(string)
			return cred, true
		}
	}
	if r, ok := data["result"].(map[string]any); ok {
		ak, _ := r["accessKeyId"].(string)
		st, _ := r["securityToken"].(string)
		if ak != "" && st != "" {
			sec, _ := r["secretAccessKey"].(string)
			exp, _ := r["expiration"].(string)
			if exp == "" {
				exp, _ = r["expiresAt"].(string)
			}
			return Credential{AccessKeyID: ak, SecretAccessKey: sec, SecurityToken: st, ExpiresAt: exp}, true
		}
	}
	return Credential{}, false
}
