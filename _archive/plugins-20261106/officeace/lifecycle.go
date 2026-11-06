package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"
)

// initializeSession performs the first read-only session lookup. It never
// refreshes credentials or contacts the network.
func (p *Plugin) initializeSession() {
	disc := p.discovery()
	if !disc.Found {
		msg := disc.statusMessage()
		p.setStatus(pluginStatusNotFound, msg)
		p.logLine("warn", "OfficeAce not found", map[string]any{"message": msg, "tried": disc.Tried})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cred, err := p.liveCredential(ctx)
	if err != nil {
		code, msg := statusFromDBError(err)
		p.setStatus(code, msg)
		p.logLine("warn", "OfficeAce session unavailable", map[string]any{"error": err.Error()})
		return
	}
	acct, changed := p.ensureAccount(cred, p.now())
	if changed {
		if err := p.save(); err != nil {
			p.logLine("warn", "save state failed", map[string]any{"error": err.Error()})
		}
	}
	status, statusMsg := p.accountSessionStatus(cred)
	p.setAccountStatus(acct.ID, status, statusMsg)
	if status == statusExpired || status == statusInvalid {
		p.setStatus(pluginStatusExpired, statusMsg)
	} else {
		p.setStatus(pluginStatusOK, "")
	}
	p.logLine("info", "OfficeAce session loaded", map[string]any{
		"account":       acct.ID,
		"label":         acct.Label,
		"db":            disc.DBPath,
		"expires_at":    cred.SessionExpiresAt.Format(time.RFC3339),
		"has_basic":     cred.HasBasic(),
		"has_hmac":      cred.HasHMAC(),
		"refresh_until": cred.RefreshExpiresAt.Format(time.RFC3339),
	})
}

// accountSessionStatus maps credential material to a host status.
func (p *Plugin) accountSessionStatus(c *Credential) (string, string) {
	if c == nil {
		return statusInvalid, "OfficeAce 会话不可用"
	}
	if !c.HasUpstreamCredential() {
		if c.StateError != "" {
			return statusInvalid, c.StateError
		}
		return statusInvalid, "OfficeAce 会话缺少 MaaS 模型凭据（model_app_key / model_app_secret / maas_api_key / model_api_url_base），请打开 OfficeAce 完成模型配置"
	}
	// A bare maas_api_key is a static bearer credential: the STS clock does not
	// apply to it, so only app-key sessions can be reported as expired here.
	if !c.HasMaasKey() && c.Expired(p.now()) {
		return statusExpired, "OfficeAce 登录态已过期；可调用 refresh 自动续期，或打开 OfficeAce 重新登录"
	}
	return statusOK, ""
}

func statusFromDBError(err error) (string, string) {
	if err == nil {
		return pluginStatusOK, ""
	}
	if errors.Is(err, errNoSession) {
		return pluginStatusLoginRequired, "请打开 OfficeAce 并登录后重试"
	}
	var su *sqliteUnavailableError
	if errors.As(err, &su) {
		return pluginStatusDBUnavailable, su.Error()
	}
	return pluginStatusError, err.Error()
}

// liveCredential reads the newest session and overlays in-memory refreshes.
// A <=2s, file-signature-keyed cache prevents every chat request from copying
// OfficeAce's WAL; a change to storage.sqlite or -wal invalidates it instantly.
func (p *Plugin) liveCredential(ctx context.Context) (*Credential, error) {
	disc := p.discovery()
	if !disc.Found {
		return nil, &sqliteUnavailableError{Path: disc.DBPath, Err: errors.New("未找到 OfficeAce 安装目录")}
	}
	sig := fileSignature(disc.DBPath)
	p.sessMu.Lock()
	defer p.sessMu.Unlock()
	if p.sessCred != nil && p.sessSig == sig && p.now().Sub(p.sessAt) < 2*time.Second {
		return p.overlayRefreshed(p.sessCred), nil
	}
	c, err := readLiveCredential(ctx, disc, p.DataDir)
	if err != nil {
		return nil, err
	}
	p.sessSig = sig
	p.sessAt = p.now()
	p.sessCred = c
	return p.overlayRefreshed(c), nil
}

// accountForCredential registers the session-derived account and persists only
// when something changed.
func (p *Plugin) accountForCredential(c *Credential) Account {
	acct, changed := p.ensureAccount(c, p.now())
	if changed {
		if err := p.save(); err != nil {
			p.logLine("warn", "save state failed", map[string]any{"error": err.Error()})
		}
	}
	return acct
}

func (p *Plugin) uptimeSeconds() int64 {
	return int64(p.now().Sub(p.startedAt).Seconds())
}

// autoRefreshEnabled reports whether the plugin may rotate the OfficeAce STS
// session on its own. Off by default so a refresh never invalidates the token
// the OfficeAce app itself keeps.
func (p *Plugin) autoRefreshEnabled() bool {
	p.cfgMu.RLock()
	defer p.cfgMu.RUnlock()
	return p.cfg.AutoRefresh
}

// authorized performs a constant-time bearer check.
func (p *Plugin) authorized(r *http.Request) bool {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return false
	}
	if p.Token == "" {
		return false
	}
	got := strings.TrimSpace(h[len(prefix):])
	return subtle.ConstantTimeCompare([]byte(got), []byte(p.Token)) == 1
}
