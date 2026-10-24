package main

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type checkinResult struct {
	Result        string   `json:"result"`
	CreditsBefore *float64 `json:"credits_before,omitempty"`
	CreditsAfter  *float64 `json:"credits_after,omitempty"`
	Message       string   `json:"message,omitempty"`
	At            int64    `json:"at"`
	// Optional failure attribution (contract v0.4, plan 2026-10-21 §4). They
	// are only set when the local upstream made the cause unambiguous.
	Kind    string `json:"kind,omitempty"`
	ResetAt int64  `json:"reset_at,omitempty"`
}

type refreshResult struct {
	Result    string `json:"result"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
	Message   string `json:"message,omitempty"`
	At        int64  `json:"at"`
}

type historyRecord struct {
	ID            int64   `json:"id"`
	PluginID      string  `json:"plugin_id"`
	AccountID     string  `json:"account_id"`
	AccountLabel  string  `json:"account_label"`
	Date          string  `json:"date"`
	Result        string  `json:"result"`
	CreditsBefore float64 `json:"credits_before"`
	CreditsAfter  float64 `json:"credits_after"`
	Message       string  `json:"message,omitempty"`
	At            int64   `json:"at"`
}

func floatPtr(v float64) *float64 { return &v }

// localFailure classifies a local API error for user-facing messages.
func localFailure(err error) (down bool, status int, msg string) {
	var le *localHTTPError
	if errors.As(err, &le) {
		return le.isDown(), le.Status, le.Error()
	}
	return false, 0, err.Error()
}

func (p *Plugin) handleAccounts(w http.ResponseWriter, r *http.Request) {
	now := p.now()
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	cred, credErr := p.liveCredential(ctx)
	if credErr != nil {
		code, msg := statusFromDBError(credErr)
		p.mu.RLock()
		snapshot := make([]*Account, 0, len(p.state.Accounts))
		for _, a := range p.state.Accounts {
			cp := *a
			snapshot = append(snapshot, &cp)
		}
		p.mu.RUnlock()
		views := make([]accountView, 0, len(snapshot))
		for _, a := range snapshot {
			if code == pluginStatusLoginRequired {
				a.Status = statusInvalid
			} else if a.Status == "" {
				a.Status = statusUnknown
			}
			a.LastError = msg
			views = append(views, a.view(now, nil))
		}
		p.setStatus(code, msg)
		writeJSON(w, http.StatusOK, map[string]any{"accounts": views})
		return
	}

	acct := p.accountForCredential(cred)
	status, statusMsg := p.accountSessionStatus(cred)

	var quota *Quota
	var serviceErr string
	if cred.SessionID != "" {
		sub, err := p.local.getSubscription(ctx, cred.SessionID)
		if err != nil {
			down, code, msg := localFailure(err)
			p.setService(!down && code != 0)
			if code == http.StatusUnauthorized || code == http.StatusForbidden {
				status = statusExpired
				statusMsg = "OfficeAce 本机会话已失效，请打开 OfficeAce 重新登录"
				p.setStatus(pluginStatusExpired, statusMsg)
			} else if down {
				serviceErr = "OfficeAce 未运行，无法获取积分；MaaS 推理直连可能仍可用"
			} else {
				serviceErr = "OfficeAce 本机 API 不可用：" + msg
			}
		} else {
			p.setService(true)
			// A working local API is stronger evidence than the stored STS clock:
			// the session is alive even if the STS token has expired (Basic auth).
			status = statusOK
			statusMsg = ""
			quota = p.quotaFromSubscription(sub, cred, acct, now)
			if strings.EqualFold(sub.Status, "expired") {
				serviceErr = "订阅已到期，积分可能无法继续使用"
			}
		}
	}
	if serviceErr != "" {
		if statusMsg == "" {
			statusMsg = serviceErr
		} else {
			statusMsg += "；" + serviceErr
		}
	}
	p.setAccountStatus(acct.ID, status, statusMsg)
	if status == statusOK {
		p.setStatus(pluginStatusOK, "")
	} else {
		p.setStatus(pluginStatusExpired, statusMsg)
	}
	view := acct.view(now, quota)
	view.Status = status
	view.LastError = statusMsg
	view.Credential = p.credentialViewFor(cred)
	writeJSON(w, http.StatusOK, map[string]any{"accounts": []accountView{view}})
}

// credentialViewFor summarizes the live session for the contract v0.3
// credential block. Refreshable only reports capability; the auto_refresh
// policy stays off by default so the host must not rotate OfficeAce's session.
func (p *Plugin) credentialViewFor(c *Credential) *credentialView {
	if c == nil {
		return nil
	}
	v := &credentialView{Present: c.HasUpstreamCredential()}
	exp := c.SessionExpiresAt
	if exp.IsZero() {
		exp = c.STSExpiresAt
	}
	if !exp.IsZero() {
		v.ExpiresAt = exp.Unix()
	}
	v.Refreshable = c.RefreshTok != "" && (c.RefreshExpiresAt.IsZero() || c.RefreshExpiresAt.After(p.now()))
	return v
}

func (p *Plugin) quotaFromSubscription(sub *subscription, cred *Credential, acct Account, now time.Time) *Quota {
	if sub == nil {
		return nil
	}
	unlimited := sub.UnlimitedCredits || sub.TotalCredits < 0
	remaining, ok := sub.remaining()
	if !ok {
		return nil
	}
	q := &Quota{Plan: strings.TrimSpace(sub.PlanName), Unlimited: unlimited}
	// An unlimited plan has no meaningful balance: keep remaining/total absent
	// instead of writing the -1 sentinel into the host's quota snapshot.
	if !unlimited {
		q.Remaining = remaining
		q.Total = sub.TotalCredits
	}
	if t, ok := parseTime(sub.ExpiresAt); ok {
		q.ExpiresAt = t.Unix()
	} else if !cred.SessionExpiresAt.IsZero() {
		q.ExpiresAt = cred.SessionExpiresAt.Unix()
	}
	if checked, at := acct.todayCheckedIn(now); checked {
		q.TodayCheckedIn = true
		q.CheckedInAt = at
	}
	return q
}

func (p *Plugin) handleHistory(w http.ResponseWriter, r *http.Request, id string) {
	acct, ok := p.findAccount(id)
	if !ok {
		writeError(w, http.StatusNotFound, "account_not_found", "account "+id+" not found")
		return
	}
	setAccount(r, id)
	days := 30
	if q := strings.TrimSpace(r.URL.Query().Get("days")); q != "" {
		if n, err := strconv.Atoi(q); err == nil {
			days = n
		}
	}
	if days < 1 {
		days = 1
	}
	if days > 180 {
		days = 180
	}
	now := p.now()
	startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	cutoff := startOfToday.AddDate(0, 0, -(days - 1)).Unix()
	records := []historyRecord{}
	if acct.CheckedInAt >= cutoff && acct.CheckedInAt > 0 {
		at := time.Unix(acct.CheckedInAt, 0).In(now.Location())
		msg := "已领取"
		if acct.CheckinResult == "already" {
			msg = "今日无可领取积分"
		}
		records = append(records, historyRecord{
			ID:           1,
			PluginID:     p.ID,
			AccountID:    acct.ID,
			AccountLabel: acct.Label,
			Date:         at.Format("2006-01-02"),
			Result:       acct.CheckinResult,
			Message:      msg,
			At:           acct.CheckedInAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"days": days, "records": records})
}

func (p *Plugin) handleAccountPatch(w http.ResponseWriter, r *http.Request, id string) {
	setAccount(r, id)
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Enabled == nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "enabled is required")
		return
	}
	p.mu.Lock()
	a := p.accountByIDLocked(id)
	if a == nil {
		p.mu.Unlock()
		writeError(w, http.StatusNotFound, "account_not_found", "account "+id+" not found")
		return
	}
	a.Enabled = *req.Enabled
	view := a.view(p.now(), nil)
	p.mu.Unlock()
	if err := p.save(); err != nil {
		p.logLine("warn", "save state failed", map[string]any{"error": err.Error()})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": *req.Enabled, "account": view})
}

// handleAccountDelete refuses to delete. OfficeAce owns the only account, its
// session lives in OfficeAce's read-only storage, and this plugin writes no
// external user data: dropping the local record would only make the account
// reappear on the next read. Answer with a stable not_supported error instead
// of a 204 whose effect never holds.
func (p *Plugin) handleAccountDelete(w http.ResponseWriter, r *http.Request, id string) {
	setAccount(r, id)
	if _, ok := p.findAccount(id); !ok {
		writeError(w, http.StatusNotFound, "account_not_found", "account "+id+" not found")
		return
	}
	writeError(w, http.StatusNotImplemented, "not_supported",
		"OfficeAce 账号来自本机只读登录态，本插件不能删除外部账号；如需移除请先在 OfficeAce 退出登录，或 PATCH {\"enabled\":false} 停用该账号")
}
