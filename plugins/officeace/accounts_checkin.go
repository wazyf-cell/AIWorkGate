package main

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

func (p *Plugin) handleCheckin(w http.ResponseWriter, r *http.Request, id string) {
	setAccount(r, id)
	now := p.now()
	res := checkinResult{At: now.Unix()}

	acct, ok := p.findAccount(id)
	if !ok {
		writeError(w, http.StatusNotFound, "account_not_found", "account "+id+" not found")
		return
	}
	if !acct.Enabled {
		res.Result = "fail"
		res.Message = "账号已停用"
		writeJSON(w, http.StatusOK, res)
		return
	}
	if !p.checkEgress(w, id) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	cred, err := p.liveCredential(ctx)
	if err != nil {
		res.Result = "fail"
		res.Message = "OfficeAce 登录态不可用：" + err.Error()
		writeJSON(w, http.StatusOK, res)
		return
	}
	p.accountForCredential(cred)

	before, beforeErr := p.local.getSubscription(ctx, cred.SessionID)
	if beforeErr != nil {
		down, code, msg := localFailure(beforeErr)
		p.setService(!down && code != 0)
		res.Result = "fail"
		res.Kind, res.ResetAt = p.classifyLocalFailure(beforeErr)
		switch {
		case down:
			res.Message = "OfficeAce 未运行，无法领取"
		case code == http.StatusUnauthorized || code == http.StatusForbidden:
			res.Message = "OfficeAce 登录态已失效，请打开 OfficeAce 重新登录"
			p.setAccountStatus(id, statusExpired, res.Message)
		default:
			res.Message = "查询积分失败：" + msg
		}
		writeJSON(w, http.StatusOK, res)
		return
	}
	creditsBefore, _ := before.remaining()

	claim, claimErr := p.local.claimBonus(ctx, cred.SessionID)
	if claimErr != nil {
		down, code, msg := localFailure(claimErr)
		p.setService(!down && code != 0)
		res.Result = "fail"
		res.Kind, res.ResetAt = p.classifyLocalFailure(claimErr)
		switch {
		case down:
			res.Message = "OfficeAce 未运行，无法领取"
		case code == http.StatusUnauthorized || code == http.StatusForbidden:
			res.Message = "OfficeAce 登录态已失效，请打开 OfficeAce 重新登录"
			p.setAccountStatus(id, statusExpired, res.Message)
		default:
			res.Message = "领取失败：" + msg
		}
		writeJSON(w, http.StatusOK, res)
		return
	}
	p.setService(true)

	after, afterErr := p.local.getSubscription(ctx, cred.SessionID)
	var creditsAfter float64
	haveAfter := afterErr == nil
	if haveAfter {
		creditsAfter, _ = after.remaining()
	}
	res.CreditsBefore = floatPtr(creditsBefore)
	if haveAfter {
		res.CreditsAfter = floatPtr(creditsAfter)
	}

	delta := 0.0
	if haveAfter && !before.UnlimitedCredits && !after.UnlimitedCredits {
		delta = creditsAfter - creditsBefore
		if delta < 0 {
			delta = 0
		}
	}
	switch {
	case delta > 0:
		res.Result = "ok"
		res.Message = fmt.Sprintf("+%.0f", delta)
	case haveAfter:
		res.Result = "already"
		res.Message = "今日无可领取积分（可能已领取）"
	case len(claim.SKUs) > 0:
		res.Result = "ok"
		if claim.Claim > 0 {
			res.Message = fmt.Sprintf("+%.0f", claim.Claim)
		} else {
			res.Message = "已领取"
		}
	default:
		res.Result = "already"
		res.Message = "今日无可领取积分（可能已领取）"
	}
	p.recordCheckin(id, res.Result, now.Unix())
	if res.Result == "ok" || res.Result == "already" {
		p.setAccountStatus(id, statusOK, "")
		p.setStatus(pluginStatusOK, "")
	}
	p.logLine("info", "checkin completed", map[string]any{
		"account": id,
		"result":  res.Result,
		"delta":   delta,
		"skus":    len(claim.SKUs),
	})
	writeJSON(w, http.StatusOK, res)
}

func (p *Plugin) handleRefresh(w http.ResponseWriter, r *http.Request, id string) {
	setAccount(r, id)
	now := p.now()
	res := refreshResult{At: now.Unix()}
	if _, ok := p.findAccount(id); !ok {
		writeError(w, http.StatusNotFound, "account_not_found", "account "+id+" not found")
		return
	}
	if !p.checkEgress(w, id) {
		return
	}
	entry, _ := p.resolveEgress(id)
	proxyURL := ""
	if entry.Mode == egressProxy {
		proxyURL = entry.URL
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	cred, err := p.liveCredential(ctx)
	if err != nil {
		res.Result = "fail"
		res.Message = "OfficeAce 登录态不可用：" + err.Error()
		p.setAccountStatus(id, statusExpired, res.Message)
		writeJSON(w, http.StatusOK, res)
		return
	}
	p.accountForCredential(cred)
	refreshed, err := p.refreshCredential(ctx, cred, id, proxyURL, true)
	if err != nil {
		res.Result = "fail"
		res.Message = err.Error()
		p.setAccountStatus(id, statusExpired, res.Message)
		p.setStatus(pluginStatusExpired, res.Message)
		p.logLine("warn", "credential refresh failed", map[string]any{"account": id, "error": err.Error()})
		writeJSON(w, http.StatusOK, res)
		return
	}
	res.Result = "ok"
	res.ExpiresAt = refreshed.STSExpiresAt.Unix()
	res.Message = "已续期"
	p.mu.Lock()
	if a := p.accountByIDLocked(id); a != nil {
		a.ExpiresAt = refreshed.SessionExpiresAt.Unix()
		a.Status = statusOK
		a.LastError = ""
	}
	p.mu.Unlock()
	if err := p.save(); err != nil {
		p.logLine("warn", "save state failed", map[string]any{"error": err.Error()})
	}
	p.setStatus(pluginStatusOK, "")
	writeJSON(w, http.StatusOK, res)
}
