package main

import (
	"context"
	"strings"
	"time"
)

// fetchAccountQuota refreshes the inline quota from the statistics endpoint.
func (p *Plugin) fetchAccountQuota(ctx context.Context, accountID string) (Quota, bool) {
	acct, ok := p.findAccount(accountID)
	if !ok || !acct.Enabled || !acct.Credential.usable() {
		return Quota{}, false
	}
	exp := acct.Credential.expiry()
	if !exp.IsZero() && !p.now().Before(exp) {
		return Quota{}, false
	}
	proxyURL, err := p.egressProxyFor(accountID)
	if err != nil {
		return Quota{}, false
	}
	info, err := p.fetchStatistics(ctx, acct, proxyURL)
	if err != nil || !info.HasCredit {
		return Quota{}, false
	}
	now := p.now()
	q := acct.Quota
	q.Remaining = info.Remaining
	q.Total = info.Total
	if strings.TrimSpace(info.PackageName) != "" {
		// v0.3 optional quota.plan: only reported when the statistics response
		// actually carries a package name.
		q.Plan = info.PackageName
	}
	if (acct.Checkin.Result == "ok" || acct.Checkin.Result == "already") && acct.Checkin.At > 0 && sameDay(acct.Checkin.At, now) {
		q.TodayCheckedIn = true
		q.CheckedInAt = acct.Checkin.At
	} else {
		q.TodayCheckedIn = false
		q.CheckedInAt = 0
	}
	p.mu.Lock()
	if a := p.accountByIDLocked(accountID); a != nil {
		a.Quota = q
		a.QuotaAt = now.Unix()
	}
	p.mu.Unlock()
	p.quotaMu.Lock()
	p.quotaCache[accountID] = quotaCacheEntry{at: now.Unix(), quota: q}
	p.quotaMu.Unlock()
	return q, true
}

func (p *Plugin) quotaIsFresh(accountID string, now time.Time) bool {
	p.quotaMu.Lock()
	defer p.quotaMu.Unlock()
	e, ok := p.quotaCache[accountID]
	return ok && now.Unix()-e.at < int64(p.opts.QuotaCacheTTL/time.Second)
}

func nextUTC8DayStart(now time.Time) int64 {
	utc8 := now.In(time.FixedZone("UTC+8", 8*3600))
	next := time.Date(utc8.Year(), utc8.Month(), utc8.Day()+1, 0, 0, 0, 0, utc8.Location())
	return next.Unix()
}

func (p *Plugin) markRateLimited(accountID string) {
	now := p.now()
	p.mu.Lock()
	if a := p.accountByIDLocked(accountID); a != nil {
		a.Quota.RateLimited = true
		a.Quota.ResetAt = nextUTC8DayStart(now)
	}
	p.mu.Unlock()
	_ = p.saveAccounts()
	p.quotaMu.Lock()
	if e, ok := p.quotaCache[accountID]; ok {
		e.quota.RateLimited = true
		e.quota.ResetAt = nextUTC8DayStart(now)
		p.quotaCache[accountID] = e
	}
	p.quotaMu.Unlock()
}

func (p *Plugin) clearRateLimited(accountID string) {
	p.mu.Lock()
	changed := false
	if a := p.accountByIDLocked(accountID); a != nil && a.Quota.RateLimited {
		a.Quota.RateLimited = false
		a.Quota.ResetAt = 0
		changed = true
	}
	p.mu.Unlock()
	if changed {
		_ = p.saveAccounts()
	}
	p.quotaMu.Lock()
	if e, ok := p.quotaCache[accountID]; ok && e.quota.RateLimited {
		e.quota.RateLimited = false
		e.quota.ResetAt = 0
		p.quotaCache[accountID] = e
	}
	p.quotaMu.Unlock()
}
