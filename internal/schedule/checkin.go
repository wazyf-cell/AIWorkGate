package schedule

import (
	"context"
	"fmt"
	"strings"
	"time"

	"aiworkgate/internal/model"
)

// Retry delays from contract section 9: after a failed attempt wait 5s, after
// the second wait 20s, then give up. The host settings may override them.
var retryDelays = []time.Duration{5 * time.Second, 20 * time.Second}

// delays returns the effective retry waits: nil keeps the contract default,
// an explicitly empty slice disables retries.
func (m *Manager) delays() []time.Duration {
	if m.RetryDelays == nil {
		return retryDelays
	}
	return m.RetryDelays
}

// checkinAccount performs one check-in for an account and persists every
// attempt. With retry it makes up to len(delays) additional attempts and
// notifies checkin_failed after the final failure; without retry it runs once
// (manual CheckinAll).
//
// An account inside a cooldown, or one whose device already signed in today, is
// never handed to the plugin: it yields a "skipped" result instead (plan
// 2026-10-21 §4). A classified failure is terminal for this run, mirroring A's
// signin_with_retry, which only retries transport errors.
func (m *Manager) checkinAccount(ctx context.Context, pluginID string, acc model.Account, date string, retry, notifyFail bool) (model.CheckinResult, bool) {
	if result, deadline, skip := m.skipCheckin(pluginID, acc, date); skip {
		m.recordCheckin(pluginID, acc, date, result)
		m.logAttempt(pluginID, acc, date, result, 0)
		m.parkUntil(pluginID, acc.ID, deadline)
		return result, false
	}

	delays := m.delays()
	var last model.CheckinResult
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			if err := m.sleep(ctx, delays[attempt-1]); err != nil {
				return last, false
			}
		}
		if err := ctx.Err(); err != nil {
			return last, false
		}

		result, err := m.Plugins.Checkin(ctx, pluginID, acc.ID)
		// A transport/plugin error says nothing about the upstream account, so
		// it is never attributed (a "plugins: http 401" means the host token to
		// the plugin is wrong, not that the account session died).
		attributable := err == nil
		if err != nil {
			result = model.CheckinResult{Result: model.CheckinFail, Message: err.Error()}
		}
		result = normalizeCheckin(result)
		last = result

		if isSuccess(result.Result) {
			m.clearHealth(pluginID, acc.ID)
			m.unpark(pluginID, acc.ID)
		} else {
			kind := ""
			if attributable {
				kind = failureKind(result)
			}
			result.Kind = kind
			if _, until := m.recordFailure(pluginID, acc, result, kind); until > 0 {
				// Silence the scheduled passes until the cooldown ends; the
				// failure itself is already on record.
				m.parkUntil(pluginID, acc.ID, until)
			}
		}

		m.recordCheckin(pluginID, acc, date, result)
		m.logAttempt(pluginID, acc, date, result, attempt+1)

		if isSuccess(result.Result) {
			return result, true
		}
		if result.Kind != "" {
			// The cause is known (expired session, quota, device dedup...);
			// another attempt within the same run cannot succeed.
			break
		}
		if !retry || attempt >= len(delays) {
			break
		}
	}
	if notifyFail {
		// A classified failure already parks the account until cooldown_until,
		// which is a better guard than the per-day "attempted" flag: a 30m rate
		// limit must be retried once it expires. Unattributed failures keep the
		// daily flag so they are not hammered every 30s.
		if last.Kind == "" {
			m.markAttempted(pluginID, acc.ID, date)
		}
		m.notifyCheckinFailed(pluginID, acc, last)
	}
	return last, false
}

// CheckinAccountNow performs one immediate check-in for a single account with
// the same rules as the scheduled pass: an active cooldown or a device that
// already signed today yields "skipped", a successful attempt clears the health
// tuple, and a classified failure parks the account. It is the entry point of
// the account page's explicit "sign in" action and of the bulk action.
func (m *Manager) CheckinAccountNow(ctx context.Context, pluginID string, acc model.Account) model.CheckinResult {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	date := m.now().Format(dateLayout)
	result, _ := m.checkinAccount(ctx, pluginID, acc, date, false, false)
	return result
}

// normalizeCheckin folds any unexpected plugin answer into fail so only
// ok/already/fail/skipped ever reaches the store.
func normalizeCheckin(result model.CheckinResult) model.CheckinResult {
	if isSuccess(result.Result) || result.Result == model.CheckinFail || result.Result == model.CheckinSkipped {
		return result
	}
	message := strings.TrimSpace(result.Message)
	if message == "" && result.Result != "" {
		message = "unexpected result: " + result.Result
	}
	result.Result = model.CheckinFail
	result.Message = message
	return result
}

// recordCheckin upserts one attempt into checkin_results. A skipped note never
// overwrites a real success recorded earlier in the day.
func (m *Manager) recordCheckin(pluginID string, acc model.Account, date string, result model.CheckinResult) {
	if m.Store == nil {
		return
	}
	if result.Result == model.CheckinSkipped {
		if ok, err := m.Store.CheckinSucceededOn(pluginID, acc.ID, date); err == nil && ok {
			return
		}
	}
	rec := model.CheckinRecord{
		PluginID:      pluginID,
		AccountID:     acc.ID,
		AccountLabel:  acc.Label,
		Date:          date,
		Result:        result.Result,
		CreditsBefore: result.CreditsBefore,
		CreditsAfter:  result.CreditsAfter,
		Message:       result.Message,
		At:            m.now().Unix(),
		DedupScope:    result.DedupScope,
		DeviceKey:     result.DeviceKey,
	}
	if _, err := m.Store.UpsertCheckinRecord(rec); err != nil {
		m.warn("record check-in failed", map[string]any{
			"plugin": pluginID, "account": acc.ID, "date": date, "error": err.Error(),
		})
	}
}

func (m *Manager) logAttempt(pluginID string, acc model.Account, date string, result model.CheckinResult, attempt int) {
	fields := map[string]any{
		"plugin":  pluginID,
		"account": acc.ID,
		"date":    date,
		"result":  result.Result,
		"attempt": attempt,
	}
	if result.Kind != "" {
		fields["kind"] = result.Kind
	}
	if result.Message != "" {
		fields["message"] = result.Message
	}
	if isSuccess(result.Result) {
		m.info("check-in succeeded", fields)
		return
	}
	if result.Result == model.CheckinSkipped {
		m.info("check-in skipped", fields)
		return
	}
	m.warn("check-in attempt failed", fields)
}

func (m *Manager) notifyCheckinFailed(pluginID string, acc model.Account, result model.CheckinResult) {
	if m.Notify == nil {
		return
	}
	body := formatAccount(acc)
	if message := strings.TrimSpace(result.Message); message != "" {
		body += ": " + message
	}
	m.Notify.Notify(KindCheckinFailed, fmt.Sprintf("签到失败：%s", pluginID), body)
}
