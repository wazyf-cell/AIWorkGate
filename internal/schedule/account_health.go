package schedule

import (
	"regexp"
	"strings"
	"time"

	"aiworkgate/internal/model"
	"aiworkgate/internal/store"
)

// This file holds the check-in health rules of plan 2026-10-21 §4: how a
// failure is attributed to a status kind, how long that kind parks the account,
// and how a device-level dedup turns into a "skipped" result.
//
// Ported from A (MIT, 朱天伟):
//   - api_server/pool.rs state_str(): the account state machine.
//   - tasks/trae_checkin.rs classify_error()/save_cooldown(): status-code based
//     classification, permanent SessionDead, 12h PlanLimit, Server/Client only
//     cooling after the third failure.
//   - tasks/trae_checkin.rs secs_until_next_checkin_reset(): DeviceSigned is
//     parked until 00:05 of the next local day.
// The exponential rate-limit backoff (30m doubling, 6h cap) is this host's own
// rule; A uses a fixed 60s SoftRate cooldown.

const (
	// PermanentCooldownUntil is the cooldown_until value of session_dead and
	// banned. Only a new login clears those states (A uses 9_999_999_999).
	PermanentCooldownUntil int64 = 9999999999

	planLimitCooldown = 12 * time.Hour
	rateLimitBase     = 30 * time.Minute
	rateLimitMax      = 6 * time.Hour

	deviceResetHour   = 0
	deviceResetMinute = 5

	// cooldownReasonMaxRunes caps the stored reason so a verbose upstream
	// message cannot bloat the account listing.
	cooldownReasonMaxRunes = 200
)

// validKinds is the closed set a plugin may report. "normal" is accepted as
// "no failure kind" so an explicit healthy answer never enters a cooldown.
var validKinds = map[string]bool{
	model.StatusKindRateLimited:  true,
	model.StatusKindSessionDead:  true,
	model.StatusKindPlanLimit:    true,
	model.StatusKindDeviceSigned: true,
	model.StatusKindQuota:        true,
	model.StatusKindBanned:       true,
}

// httpStatusHint recognises "HTTP 401", "plugins: http 429: ..." and
// "status_code=403" style text. It is the fallback for plugins that do not send
// kind yet; arbitrary numbers in a message are not treated as status codes.
var httpStatusHint = regexp.MustCompile(`(?i)(?:http|status(?:_?code)?)\s*[:=]?\s*(401|403|429)\b`)

// quotaHints are the upstream wordings that unambiguously mean "out of balance
// or quota" rather than a transient error.
var quotaHints = []string{
	"余额不足", "额度不足", "积分不足", "配额不足",
	"insufficient quota", "insufficient balance", "insufficient credit",
	"quota exceeded", "quota exhausted", "credits exhausted",
}

// normalizeKind validates a plugin-reported kind. Unknown or empty values mean
// "the plugin did not attribute this failure".
func normalizeKind(kind string) string {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind == model.StatusKindNormal || !validKinds[kind] {
		return ""
	}
	return kind
}

// failureKind resolves the attribution of one failed attempt: the plugin's own
// kind wins; otherwise 401/403 -> session_dead, 429 -> rate_limited and an
// explicit insufficient-balance wording -> quota. Everything else stays
// unattributed and only bumps failure_count.
func failureKind(result model.CheckinResult) string {
	if kind := normalizeKind(result.Kind); kind != "" {
		return kind
	}
	if match := httpStatusHint.FindStringSubmatch(result.Message); match != nil {
		switch match[1] {
		case "401", "403":
			return model.StatusKindSessionDead
		case "429":
			return model.StatusKindRateLimited
		}
	}
	lower := strings.ToLower(result.Message)
	for _, hint := range quotaHints {
		if strings.Contains(lower, hint) {
			return model.StatusKindQuota
		}
	}
	return ""
}

// cooldownUntilFor returns the Unix second an account of kind may be tried
// again. priorFailures is the failure count before the current failure and only
// feeds the exponential rate-limit backoff.
func cooldownUntilFor(kind string, now time.Time, resetAt int64, priorFailures int) int64 {
	switch kind {
	case model.StatusKindSessionDead, model.StatusKindBanned:
		// Only a fresh login can revive the account.
		return PermanentCooldownUntil
	case model.StatusKindPlanLimit:
		return now.Add(planLimitCooldown).Unix()
	case model.StatusKindDeviceSigned:
		return nextDeviceReset(now)
	case model.StatusKindRateLimited, model.StatusKindQuota:
		if resetAt > now.Unix() {
			return resetAt
		}
		return now.Add(rateLimitBackoff(priorFailures)).Unix()
	}
	return 0
}

// nextDeviceReset is 00:05 of the next local day. The upstream dedups by
// natural day; the five minutes absorb clock skew (A:
// secs_until_next_checkin_reset).
func nextDeviceReset(now time.Time) int64 {
	next := time.Date(now.Year(), now.Month(), now.Day()+1,
		deviceResetHour, deviceResetMinute, 0, 0, now.Location())
	return next.Unix()
}

// rateLimitBackoff is 30m * 2^priorFailures capped at 6h.
func rateLimitBackoff(priorFailures int) time.Duration {
	if priorFailures < 0 {
		priorFailures = 0
	}
	if priorFailures > 8 {
		priorFailures = 8
	}
	wait := rateLimitBase << priorFailures
	if wait <= 0 || wait > rateLimitMax {
		wait = rateLimitMax
	}
	return wait
}

// kindLabel is the human next-step hint stored in cooldown_reason.
func kindLabel(kind string) string {
	switch kind {
	case model.StatusKindSessionDead:
		return "登录态已失效，请重新登录"
	case model.StatusKindBanned:
		return "账号被封禁，请更换账号"
	case model.StatusKindPlanLimit:
		return "套餐额度限制，12 小时后重试"
	case model.StatusKindDeviceSigned:
		return "设备当日已签，次日 00:05 后重试"
	case model.StatusKindRateLimited:
		return "上游限流，稍后自动重试"
	case model.StatusKindQuota:
		return "余额或额度不足，请充值后重试"
	}
	return "签到失败"
}

// cooldownReason renders the stored reason: the kind label plus a bounded copy
// of the upstream message.
func cooldownReason(kind, message string) string {
	label := kindLabel(kind)
	message = strings.TrimSpace(message)
	if message == "" {
		return label
	}
	out := label + "（" + message + "）"
	runes := []rune(out)
	if len(runes) > cooldownReasonMaxRunes {
		out = string(runes[:cooldownReasonMaxRunes])
	}
	return out
}

// healthFor reads one account's health row; a missing store or row is healthy.
func (m *Manager) healthFor(pluginID, accountID string) (store.AccountHealth, bool) {
	if m.Store == nil {
		return store.AccountHealth{}, false
	}
	health, ok, err := m.Store.GetAccountHealth(pluginID, accountID)
	if err != nil {
		m.warn("read account health failed", map[string]any{
			"plugin": pluginID, "account": accountID, "error": err.Error(),
		})
		return store.AccountHealth{}, false
	}
	return health, ok
}

// skipCheckin reports a skipped result when the account must not be called:
// it is inside a cooldown, or its device already signed in today. The deadline
// is the moment the scheduled pass may revisit the account.
func (m *Manager) skipCheckin(pluginID string, acc model.Account, date string) (model.CheckinResult, int64, bool) {
	now := m.now()
	if health, ok := m.healthFor(pluginID, acc.ID); ok && health.CooldownUntil > now.Unix() {
		reason := strings.TrimSpace(health.CooldownReason)
		if reason == "" {
			reason = kindLabel(health.StatusKind)
		}
		return model.CheckinResult{
			Result:  model.CheckinSkipped,
			Kind:    health.StatusKind,
			Message: "冷却中：" + reason,
		}, health.CooldownUntil, true
	}
	if key, signed := m.deviceSignedToday(pluginID, acc.ID, date); signed {
		return model.CheckinResult{
			Result:     model.CheckinSkipped,
			Kind:       model.StatusKindDeviceSigned,
			Message:    "设备当日已签",
			DedupScope: model.DedupScopeDevice,
			DeviceKey:  key,
		}, nextDeviceReset(now), true
	}
	return model.CheckinResult{}, 0, false
}

// deviceSignedToday reports whether the account's known device already produced
// a successful device-scoped check-in today. The first attempt on a device is
// what teaches the host its key; accounts without a recorded key are still
// called once.
func (m *Manager) deviceSignedToday(pluginID, accountID, date string) (string, bool) {
	if m.Store == nil {
		return "", false
	}
	key, ok, err := m.Store.LastDeviceKey(pluginID, accountID)
	if err != nil {
		m.warn("read device key failed", map[string]any{
			"plugin": pluginID, "account": accountID, "error": err.Error(),
		})
		return "", false
	}
	if !ok || strings.TrimSpace(key) == "" {
		return "", false
	}
	signed, err := m.Store.DeviceCheckedIn(pluginID, date, key)
	if err != nil {
		m.warn("read device check-in failed", map[string]any{
			"plugin": pluginID, "account": accountID, "device_key": key, "error": err.Error(),
		})
		return "", false
	}
	return key, signed
}

// recordFailure persists one failed attempt. A classified failure gets a kind
// and a cooldown; an unattributed one only bumps failure_count. It returns the
// new failure_count and the cooldown deadline (0 when the failure is not
// classified or the store is unavailable).
func (m *Manager) recordFailure(pluginID string, acc model.Account, result model.CheckinResult, kind string) (int, int64) {
	if m.Store == nil {
		return 0, 0
	}
	prior := 0
	if health, ok := m.healthFor(pluginID, acc.ID); ok {
		prior = health.FailureCount
	}
	count := prior + 1
	if bumped, err := m.Store.BumpAccountFailure(pluginID, acc.ID); err != nil {
		m.warn("record account failure failed", map[string]any{
			"plugin": pluginID, "account": acc.ID, "error": err.Error(),
		})
	} else {
		count = bumped
	}
	if kind == "" {
		return count, 0
	}
	until := cooldownUntilFor(kind, m.now(), result.ResetAt, count-1)
	health := store.AccountHealth{
		PluginID:       pluginID,
		AccountID:      acc.ID,
		StatusKind:     kind,
		CooldownUntil:  until,
		CooldownReason: cooldownReason(kind, result.Message),
		FailureCount:   count,
	}
	if err := m.Store.SetAccountHealth(health); err != nil {
		m.warn("save account health failed", map[string]any{
			"plugin": pluginID, "account": acc.ID, "kind": kind, "error": err.Error(),
		})
		return count, 0
	}
	m.info("account parked after check-in failure", map[string]any{
		"plugin": pluginID, "account": acc.ID, "kind": kind,
		"cooldown_until": until, "failure_count": count,
	})
	return count, until
}

// clearHealth returns an account to normal after a successful check-in.
func (m *Manager) clearHealth(pluginID, accountID string) {
	if m.Store == nil {
		return
	}
	if err := m.Store.ClearAccountHealth(pluginID, accountID); err != nil {
		m.warn("clear account health failed", map[string]any{
			"plugin": pluginID, "account": accountID, "error": err.Error(),
		})
	}
}

// ClearAccountHealth forgets an account's cooldown, both in the database and in
// the in-memory "already reported" guard. The app calls it after a successful
// login, which is the only way out of a permanent session_dead/banned cooldown.
func (m *Manager) ClearAccountHealth(pluginID, accountID string) {
	m.unpark(pluginID, accountID)
	m.clearHealth(pluginID, accountID)
}
