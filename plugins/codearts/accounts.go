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
	"sync"
	"time"
)

func (p *Plugin) accountViews(now time.Time) []accountView {
	p.mu.RLock()
	defer p.mu.RUnlock()
	views := make([]accountView, 0, len(p.accounts))
	for _, a := range p.accounts {
		views = append(views, a.view(now))
	}
	return views
}

// handleAccounts returns every account with inline quota. Statistics are
// fetched at most once per quota cache TTL (60s) and never for disabled or
// expired accounts; failures keep the last known values.
func (p *Plugin) handleAccounts(w http.ResponseWriter, r *http.Request) {
	now := p.now()
	snaps := p.accountSnapshots()
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	sem := make(chan struct{}, 3)
	var mu sync.Mutex
	fetched := false
	for _, a := range snaps {
		if !a.Enabled || !a.Credential.usable() {
			continue
		}
		if p.quotaIsFresh(a.ID, now) {
			continue
		}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if _, ok := p.fetchAccountQuota(ctx, id); ok {
				mu.Lock()
				fetched = true
				mu.Unlock()
			}
		}(a.ID)
	}
	wg.Wait()
	if fetched {
		if err := p.saveAccounts(); err != nil {
			p.logLine("warn", "save accounts failed", map[string]any{"error": err.Error()})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": p.accountViews(now)})
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
	now := p.now()
	p.mu.Lock()
	a := p.accountByIDLocked(id)
	if a == nil {
		p.mu.Unlock()
		writeError(w, http.StatusNotFound, "account_not_found", "account "+id+" not found")
		return
	}
	a.Enabled = *req.Enabled
	view := a.view(now)
	p.mu.Unlock()
	if err := p.saveAccounts(); err != nil {
		p.logLine("warn", "save accounts failed", map[string]any{"error": err.Error()})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": *req.Enabled, "account": view})
}

func (p *Plugin) handleAccountDelete(w http.ResponseWriter, r *http.Request, id string) {
	setAccount(r, id)
	p.mu.Lock()
	idx := -1
	for i, a := range p.accounts {
		if a.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		p.mu.Unlock()
		writeError(w, http.StatusNotFound, "account_not_found", "account "+id+" not found")
		return
	}
	p.accounts = append(p.accounts[:idx], p.accounts[idx+1:]...)
	p.mu.Unlock()
	p.dropTransport(id)
	p.quotaMu.Lock()
	delete(p.quotaCache, id)
	p.quotaMu.Unlock()
	if err := p.saveAccounts(); err != nil {
		p.logLine("warn", "save accounts failed", map[string]any{"error": err.Error()})
	}
	w.WriteHeader(http.StatusNoContent)
}

func (p *Plugin) handleCheckin(w http.ResponseWriter, r *http.Request, id string) {
	setAccount(r, id)
	acct, ok := p.findAccount(id)
	if !ok {
		writeError(w, http.StatusNotFound, "account_not_found", "account "+id+" not found")
		return
	}
	if !p.checkEgress(w, id) {
		return
	}
	now := p.now()
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
	defer cancel()
	acct, err := p.ensureCredential(ctx, id, false)
	if err != nil {
		p.recordCheckinFailure(id, err.Error(), now)
		writeJSON(w, http.StatusOK, credentialCheckinFailure(err, now))
		return
	}
	proxyURL, err := p.egressProxyFor(id)
	if err != nil {
		var em *egressMissingError
		if errors.As(err, &em) {
			writeError(w, http.StatusForbidden, "egress_missing", em.Error())
			return
		}
		writeJSON(w, http.StatusOK, checkinResult{Result: "fail", Message: err.Error(), At: now.Unix()})
		return
	}
	res, checkErr := p.claimDailyCheckin(ctx, acct, proxyURL)
	if checkErr != nil {
		res.Result = "fail"
		res.Message = checkErr.Error()
		res.At = now.Unix()
		res.Kind, res.ResetAt = checkinErrorAttribution(checkErr)
		p.recordCheckinFailure(id, res.Message, now)
		writeJSON(w, http.StatusOK, res)
		return
	}
	p.recordCheckinResult(id, res, now)
	writeJSON(w, http.StatusOK, res)
}

func (p *Plugin) recordCheckinFailure(accountID, msg string, now time.Time) {
	p.mu.Lock()
	if a := p.accountByIDLocked(accountID); a != nil {
		a.LastError = msg
		// Keep a successful check-in from earlier today; a later failure must
		// not flip today_checked_in back to false.
		if !((a.Checkin.Result == "ok" || a.Checkin.Result == "already") && sameDay(a.Checkin.At, now)) {
			a.Checkin = checkinState{At: now.Unix(), Result: "fail", Message: msg}
		}
	}
	p.mu.Unlock()
	if err := p.saveAccounts(); err != nil {
		p.logLine("warn", "save accounts failed", map[string]any{"error": err.Error()})
	}
}

func (p *Plugin) recordCheckinResult(accountID string, res checkinResult, now time.Time) {
	var updated Quota
	p.mu.Lock()
	if a := p.accountByIDLocked(accountID); a != nil {
		a.LastError = ""
		a.Invalid = false
		a.Checkin = checkinState{
			At:            res.At,
			Result:        res.Result,
			CreditsBefore: res.CreditsBefore,
			CreditsAfter:  res.CreditsAfter,
			Message:       res.Message,
		}
		a.Quota.TodayCheckedIn = true
		a.Quota.CheckedInAt = res.At
		a.Quota.RateLimited = false
		a.Quota.ResetAt = 0
		if res.CreditsAfter != nil {
			a.Quota.Remaining = *res.CreditsAfter
		} else if res.CreditsBefore != nil {
			a.Quota.Remaining = *res.CreditsBefore
		}
		a.QuotaAt = now.Unix()
		updated = a.Quota
	}
	p.mu.Unlock()
	if err := p.saveAccounts(); err != nil {
		p.logLine("warn", "save accounts failed", map[string]any{"error": err.Error()})
	}
	// Keep the just-claimed value for one quota TTL instead of immediately
	// re-reading a possibly stale statistics snapshot.
	p.quotaMu.Lock()
	p.quotaCache[accountID] = quotaCacheEntry{at: now.Unix(), quota: updated}
	p.quotaMu.Unlock()
}

func (p *Plugin) handleRefresh(w http.ResponseWriter, r *http.Request, id string) {
	setAccount(r, id)
	acct, ok := p.findAccount(id)
	if !ok {
		writeError(w, http.StatusNotFound, "account_not_found", "account "+id+" not found")
		return
	}
	if !p.checkEgress(w, id) {
		return
	}
	if !acct.Credential.refreshable() {
		writeError(w, http.StatusBadRequest, "refresh_not_supported", "该账号没有 refresh_token，请重新登录")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	updated, err := p.ensureCredential(ctx, id, true)
	if err != nil {
		var em *egressMissingError
		if errors.As(err, &em) {
			writeError(w, http.StatusForbidden, "egress_missing", em.Error())
			return
		}
		writeError(w, http.StatusBadGateway, "refresh_failed", err.Error())
		return
	}
	expires := updated.Credential.expiry()
	var expUnix int64
	if !expires.IsZero() {
		expUnix = expires.Unix()
	}
	writeJSON(w, http.StatusOK, refreshResult{
		Result:    "ok",
		ExpiresAt: expUnix,
		Message:   "续期成功",
		At:        p.now().Unix(),
	})
}

func (p *Plugin) handleIdentityReset(w http.ResponseWriter, r *http.Request, id string) {
	setAccount(r, id)
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	req := struct {
		Mode string `json:"mode"`
	}{}
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
			return
		}
	}
	mode := strings.TrimSpace(strings.ToLower(req.Mode))
	if mode == "" {
		mode = "request"
	}
	if mode != "request" {
		writeError(w, http.StatusBadRequest, "unsupported_identity_mode", fmt.Sprintf("mode %s is not supported by this plugin", mode))
		return
	}
	now := p.now()
	p.mu.Lock()
	a := p.accountByIDLocked(id)
	if a == nil {
		p.mu.Unlock()
		writeError(w, http.StatusNotFound, "account_not_found", "account "+id+" not found")
		return
	}
	a.Identity = newIdentity(now.Unix())
	ident := Identity{SessionID: a.Identity.SessionID, Source: "derived", UpdatedAt: a.Identity.UpdatedAt}
	p.mu.Unlock()
	if err := p.saveAccounts(); err != nil {
		p.logLine("warn", "save accounts failed", map[string]any{"error": err.Error()})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mode": mode, "identity": ident})
}
