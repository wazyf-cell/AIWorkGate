package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	defaultIAMSTSUrl     = "https://iam.myhuaweicloud.com/v3.0/OS-CREDENTIAL/securitytokens"
	iamSTSDurationSecond = 86400
)

type credentialRefreshError struct {
	msg string
	err error
}

func (e *credentialRefreshError) Error() string {
	if e.err != nil {
		return e.msg + "：" + e.err.Error()
	}
	return e.msg
}

func (e *credentialRefreshError) Unwrap() error { return e.err }

// refreshFailure throttles automatic refresh attempts after a hard failure so
// that a chatty client cannot hammer IAM.
type refreshFailure struct {
	sessionID string
	at        time.Time
	msg       string
}

const refreshCooldown = 30 * time.Second

func (p *Plugin) recordRefreshFailure(c *Credential, err error) {
	p.credMu.Lock()
	p.refreshFail[c.UserKey()] = refreshFailure{sessionID: c.SessionID, at: p.now(), msg: err.Error()}
	p.credMu.Unlock()
}

func (p *Plugin) clearRefreshFailure(c *Credential) {
	p.credMu.Lock()
	delete(p.refreshFail, c.UserKey())
	p.credMu.Unlock()
}

func (p *Plugin) refreshCooldown(c *Credential) error {
	p.credMu.RLock()
	f, ok := p.refreshFail[c.UserKey()]
	p.credMu.RUnlock()
	if ok && f.sessionID == c.SessionID && p.now().Sub(f.at) < refreshCooldown {
		return &credentialRefreshError{msg: "自动续期刚刚失败，稍后重试；请打开 OfficeAce 并重新登录", err: errors.New(f.msg)}
	}
	return nil
}

// overlayRefreshed replaces STS material with an in-memory refresh result when
// it belongs to the same OfficeAce session.
func (p *Plugin) overlayRefreshed(c *Credential) *Credential {
	p.credMu.RLock()
	r := p.refreshed[c.UserKey()]
	p.credMu.RUnlock()
	if r == nil || r.SessionID != c.SessionID {
		return c
	}
	out := *c
	if !r.STSExpiresAt.IsZero() && r.STSExpiresAt.After(out.STSExpiresAt) {
		out.STSToken = r.STSToken
		out.STSExpiresAt = r.STSExpiresAt
		out.SessionExpiresAt = r.STSExpiresAt
	}
	return &out
}

func (p *Plugin) storeRefreshed(c *Credential) {
	cp := *c
	p.credMu.Lock()
	p.refreshed[c.UserKey()] = &cp
	p.credMu.Unlock()
}

// refreshCredential renews the STS token:
//  1. re-read OfficeAce's SQLite (the app may have renewed it already),
//  2. otherwise exchange the long-term IAM AK/SK for a new token at IAM.
//
// The renewed token lives in memory only; OfficeAce's files stay untouched.
func (p *Plugin) refreshCredential(ctx context.Context, c *Credential, accountID, proxyURL string, force bool) (out *Credential, err error) {
	now := p.now()
	if !force {
		if cerr := p.refreshCooldown(c); cerr != nil {
			return nil, cerr
		}
		// Nothing to renew: chat already re-read the live session before this call.
		if !c.Expired(now) {
			return c, nil
		}
	}
	defer func() {
		if err != nil {
			p.recordRefreshFailure(c, err)
		}
	}()
	// 1) Follow OfficeAce if it already refreshed the database.
	if fresh, rerr := readLiveCredential(ctx, p.discovery(), p.DataDir); rerr == nil && fresh != nil &&
		fresh.UserKey() == c.UserKey() && fresh.SessionID == c.SessionID &&
		!fresh.Expired(now) && fresh.STSToken != "" {
		p.storeRefreshed(fresh)
		p.clearRefreshFailure(c)
		p.logLine("info", "credential renewed from live OfficeAce session", map[string]any{
			"account":    accountID,
			"expires_at": fresh.STSExpiresAt.Format(time.RFC3339),
		})
		return fresh, nil
	}

	// 2) IAM STS exchange.
	if strings.TrimSpace(c.IAMAccess) == "" || strings.TrimSpace(c.IAMSecret) == "" {
		return nil, &credentialRefreshError{msg: "OfficeAce 会话缺少 IAM AK/SK，无法自动续期；请打开 OfficeAce 并重新登录"}
	}
	body := fmt.Sprintf(`{"auth":{"identity":{"methods":["token"],"token":{"duration_seconds":%d}}}}`, iamSTSDurationSecond)
	ct := "application/json; charset=utf8"

	token, expiresAt, iamErr := p.iamSecurityToken(ctx, c, c.STSToken, body, ct, accountID, proxyURL)
	if iamErr != nil && shouldRetryIAMWithoutToken(iamErr) {
		// Permanent AK/SK signing does not need an STS token; try once without it.
		token, expiresAt, iamErr = p.iamSecurityToken(ctx, c, "", body, ct, accountID, proxyURL)
	}
	if iamErr != nil {
		return nil, &credentialRefreshError{msg: "自动续期失败（IAM STS）；请打开 OfficeAce 并重新登录", err: iamErr}
	}
	res := *c
	res.STSToken = token
	if expiresAt.IsZero() {
		expiresAt = now.Add(time.Duration(iamSTSDurationSecond) * time.Second)
	}
	res.STSExpiresAt = expiresAt
	res.SessionExpiresAt = expiresAt
	p.storeRefreshed(&res)
	p.clearRefreshFailure(c)
	p.logLine("info", "credential renewed via IAM STS", map[string]any{
		"account":    accountID,
		"expires_at": expiresAt.Format(time.RFC3339),
	})
	return &res, nil
}

func shouldRetryIAMWithoutToken(err error) bool {
	var statusErr *iamStatusError
	if errors.As(err, &statusErr) {
		return statusErr.Status == http.StatusUnauthorized || statusErr.Status == http.StatusForbidden
	}
	return false
}

type iamStatusError struct {
	Status int
	Body   string
}

func (e *iamStatusError) Error() string {
	return fmt.Sprintf("IAM 返回 %d：%s", e.Status, e.Body)
}

func (p *Plugin) iamSecurityToken(ctx context.Context, c *Credential, securityToken, body, contentType, accountID, proxyURL string) (string, time.Time, error) {
	signed, err := signSDKHMAC(signInput{
		Method:      http.MethodPost,
		URL:         p.iamURL,
		Body:        body,
		ContentType: contentType,
		Credential: signCredential{
			AccessKey:     c.IAMAccess,
			SecretKey:     c.IAMSecret,
			SecurityToken: securityToken,
			ProjectID:     c.ProjectID,
		},
		Now: p.now(),
	})
	if err != nil {
		return "", time.Time{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.iamURL, bytes.NewReader([]byte(body)))
	if err != nil {
		return "", time.Time{}, err
	}
	for k, v := range signed.Header {
		if strings.EqualFold(k, "Host") {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	client := &http.Client{Transport: p.transportFor(accountID, proxyURL), Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", time.Time{}, &iamStatusError{Status: resp.StatusCode, Body: truncate(string(respBody), 400)}
	}
	var payload struct {
		Credential struct {
			SecurityToken string `json:"securitytoken"`
			ExpiresAt     string `json:"expires_at"`
		} `json:"credential"`
	}
	if err := json.Unmarshal(respBody, &payload); err != nil {
		return "", time.Time{}, fmt.Errorf("解析 IAM 响应失败：%w", err)
	}
	if strings.TrimSpace(payload.Credential.SecurityToken) == "" {
		return "", time.Time{}, errors.New("IAM 响应缺少 credential.securitytoken")
	}
	var expires time.Time
	if t, ok := parseTime(payload.Credential.ExpiresAt); ok {
		expires = t
	}
	return payload.Credential.SecurityToken, expires, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
