package schedule

import (
	"context"
	"fmt"
	"strings"
	"time"

	"aiworkgate/internal/model"
)

const (
	// quotaLowRatio is the default "remaining / total" alert threshold (D60).
	quotaLowRatio = 0.10
	// quotaLowAbsolute is used when the plugin reports no total (total == 0).
	quotaLowAbsolute = 100
	// defaultQuotaPoll is the contract default when poll_minutes is omitted.
	defaultQuotaPoll = 30 * time.Minute
)

// quotaPollInterval resolves the manifest poll interval. ok=false means the
// plugin asked for dashboard-only reads (poll_minutes = 0).
func quotaPollInterval(q model.QuotaConfig) (time.Duration, bool) {
	if q.PollMinutes == nil {
		return defaultQuotaPoll, true
	}
	if *q.PollMinutes <= 0 {
		return 0, false
	}
	return time.Duration(*q.PollMinutes) * time.Minute, true
}

// runQuotaPoll refreshes quota for every enabled quota plugin that is due.
// Polling is per plugin, at most once per its poll interval.
func (m *Manager) runQuotaPoll(ctx context.Context) {
	for _, st := range m.Plugins.List() {
		if ctx.Err() != nil {
			return
		}
		pluginID := st.Manifest.ID
		if pluginID == "" || !st.Enabled || !hasCapability(st.Manifest, model.CapQuota) {
			continue
		}
		interval, scheduled := quotaPollInterval(st.Manifest.Quota)
		if !scheduled {
			continue
		}
		now := m.now()
		if !m.quotaDue(pluginID, now, interval) {
			continue
		}
		// Mark before the call so a failing plugin is not retried every tick.
		m.markQuotaPoll(pluginID, now)

		accounts, err := m.Plugins.Accounts(ctx, pluginID)
		if err != nil {
			m.warn("quota poll: list accounts failed", map[string]any{
				"plugin": pluginID, "error": err.Error(),
			})
			if ctx.Err() == nil {
				m.notifyPluginOffline(pluginID, err)
			}
			continue
		}
		for _, acc := range accounts {
			if m.Store != nil {
				snapshot := model.QuotaSnapshot{
					PluginID:  pluginID,
					AccountID: acc.ID,
					Remaining: acc.Quota.Remaining,
					Total:     acc.Quota.Total,
					ExpiresAt: acc.Quota.ExpiresAt,
					Plan:      acc.Quota.Plan,
					At:        now.Unix(),
				}
				if err := m.Store.SaveQuotaSnapshot(snapshot); err != nil {
					m.warn("save quota snapshot failed", map[string]any{
						"plugin": pluginID, "account": acc.ID, "error": err.Error(),
					})
				}
			}
			if quotaLow(acc.Quota) {
				m.notifyQuotaLow(pluginID, acc, now)
			}
		}
	}
}

// quotaDue reports whether interval has elapsed since the last poll.
func (m *Manager) quotaDue(pluginID string, now time.Time, interval time.Duration) bool {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	last, ok := m.quotaLast[pluginID]
	if !ok || last.IsZero() {
		return true
	}
	return !now.Before(last.Add(interval))
}

func (m *Manager) markQuotaPoll(pluginID string, now time.Time) {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	if m.quotaLast == nil {
		m.quotaLast = map[string]time.Time{}
	}
	m.quotaLast[pluginID] = now
}

// quotaLow applies D60: ratio < 10% when a total is known, otherwise an
// absolute remaining < 100. The user-configurable per-account threshold is a
// later settings concern; this is the default policy.
func quotaLow(q model.Quota) bool {
	if q.Unlimited {
		return false
	}
	if q.Total > 0 {
		return q.Remaining/q.Total < quotaLowRatio
	}
	return q.Remaining < quotaLowAbsolute
}

// notifyQuotaLow emits at most one quota_low notification per account per
// local day.
func (m *Manager) notifyQuotaLow(pluginID string, acc model.Account, now time.Time) {
	if m.Notify == nil {
		return
	}
	date := now.Format(dateLayout)
	key := pluginID + "\x00" + acc.ID

	m.stateMu.Lock()
	if m.quotaNotified == nil {
		m.quotaNotified = map[string]string{}
	}
	if m.quotaNotified[key] == date {
		m.stateMu.Unlock()
		return
	}
	m.quotaNotified[key] = date
	m.stateMu.Unlock()

	var detail string
	if acc.Quota.Total > 0 {
		percent := acc.Quota.Remaining / acc.Quota.Total * 100
		detail = fmt.Sprintf("剩余 %g / %g (%.1f%%)", acc.Quota.Remaining, acc.Quota.Total, percent)
	} else {
		detail = fmt.Sprintf("剩余 %g（未报告总额）", acc.Quota.Remaining)
	}
	body := formatAccount(acc)
	if strings.TrimSpace(detail) != "" {
		body += ": " + detail
	}
	m.Notify.Notify(KindQuotaLow, fmt.Sprintf("额度不足：%s", pluginID), body)
}
