package scheduler

// D-24 冷却按原因分级。此前 429 一律 30s 翻倍封顶 600s、5xx 一律 60s，不分
// 原因（plans/2026-11-05 §D-24），额度耗尽的账号被反复 doomed 重试。本文件
// 按 docs/缺口分析/05-冷却档位规格.md 的 7 类原因分派冷却时长：
//
//	rate_limit      429：尊重上游 Retry-After（封顶 1 小时，即 05 号文档的
//	                longestWait）；无提示时保留现行为——30s 起翻倍封顶 600s。
//	upstream_error  5xx / 408 / 409：60s（保留现行为）。
//	quota_exhausted 上游明确额度/积分耗尽文案：冷却到窗口重置时间或 30 分钟
//	                取小（ResetAt 未知时取 30 分钟）。
//	auth_expired    401 / 登录态失效：长冷却 30 分钟。
//	network         连接失败（status 0）：15s 短冷却。
//	egress          出口策略拒绝（EGRESS_ 前缀文案）：5 分钟。
//	other           其余明确上报的失败：30s。
//
// 归因入口是 ClassifyCooldown(status, message)：状态码驱动 401/429/0/5xx，
// 错误文案驱动 quota_exhausted / egress / other。普通 Report 只有状态码可依，
// 所以 plain Report 上的归因是状态码子集；ReportCause 允许调用方附带文案与
// 上游提示（Retry-After / 窗口重置时间）。
//
// 兼容性约束（测试锁定）：429 的 30s 翻倍封顶 600s、5xx/408/409 的 60s、
// 连败熔断计数条件（status>=500 || status==0 || status==408）全部保留；
// 未归因的失败（如请求形态的 400）维持现状——不冷却，只复位连败计数。

import (
	"strings"
	"time"
)

// Cooldown reason classes (D-24, 05 号文档的 7 类)。CooldownRecord.Why 与
// /api/accounts/{plugin}/{account}/cooldown/clear 的宿主日志共用这套枚举。
const (
	CooldownRateLimit      = "rate_limit"
	CooldownUpstreamError  = "upstream_error"
	CooldownQuotaExhausted = "quota_exhausted"
	CooldownAuthExpired    = "auth_expired"
	CooldownNetwork        = "network"
	CooldownEgress         = "egress"
	CooldownOther          = "other"
)

// Tiered cooldown seconds per class. cooldownServerErr keeps its historic name
// because the 60s upstream tier is the pre-D-24 value; the rest are new.
const (
	cooldownAuthSeconds   = 30 * 60 // 401 / 登录态失效：30 分钟
	cooldownQuotaSeconds  = 30 * 60 // 额度耗尽上限：30 分钟（与窗口重置取小）
	cooldownEgressSeconds = 5 * 60  // 出口策略拒绝：5 分钟
	cooldownNetSeconds    = 15      // 连接失败：15s
	cooldownOtherSeconds  = 30      // 其他明确上报的失败：30s
	// cooldownRetryCapSeconds 是 05 号文档的 longestWait：上游自己给的
	// "何时可再试"最多被信任 1 小时。
	cooldownRetryCapSeconds = 3600
)

// FailureCause is the optional attribution input of ReportCause. The zero
// value reproduces the plain Report behavior (status-code-only attribution).
type FailureCause struct {
	// Status is the upstream HTTP status; 0 marks a transport failure.
	Status int
	// Message is the upstream error text used for quota/egress attribution.
	// It must be a transport-safe summary, never a raw body dump.
	Message string
	// RetryAfter, when positive, is the upstream retry hint (Retry-After 头
	// 或等价字段). Only the rate_limit tier consumes it, capped at 1 hour.
	RetryAfter time.Duration
	// ResetAt, when positive, is the upstream window reset unix time. Only
	// the quota_exhausted tier consumes it, capped at 30 minutes.
	ResetAt int64
	// NoPenalty (D-46①) marks a failure whose cause is not attributable to the
	// account: the same error signature (status + normalized error text) was
	// already reported for another account inside one dispatch round, which
	// means the block follows the request/IP/upstream and not the credential.
	// Such a report records no strikes, no cooldowns and no pool circuit
	// evidence — punishing every candidate with the identical error would park
	// a whole pool for one shared fault. Credits bookkeeping is unaffected.
	NoPenalty bool
}

// quotaMarkers are the error-text markers of an upstream quota/credit
// exhaustion (D-24: 文案含 quota/额度/积分 → quota_exhausted；credit 与
// 余额是同一类硬性额度耗尽).
var quotaMarkers = []string{"quota", "额度", "积分", "credit", "余额"}

// ClassifyCooldown maps one upstream failure onto the D-24 reason classes.
// An empty result means "no cooldown": plain Report keeps today's set
// {401, 429, 408, 409, 5xx, 0} and a request-shaped 4xx (400/402/403 ...)
// without an explicit error message stays cooldown-free. The egress marker
// wins over the status because an egress policy rejection can surface as any
// 4xx; auth wins over quota text per the D-24 attribution order.
func ClassifyCooldown(status int, message string) string {
	msg := strings.ToLower(strings.TrimSpace(message))
	switch {
	case strings.Contains(msg, "egress_"):
		return CooldownEgress
	case status == 401:
		return CooldownAuthExpired
	case containsAny(msg, quotaMarkers):
		return CooldownQuotaExhausted
	case status == 429:
		return CooldownRateLimit
	case status == 0:
		return CooldownNetwork
	case status == 408 || status == 409 || status >= 500:
		return CooldownUpstreamError
	case status >= 400 && msg != "":
		return CooldownOther
	}
	return ""
}

// containsAny reports whether s contains one of the markers (already
// lower-cased by the caller).
func containsAny(s string, markers []string) bool {
	if s == "" {
		return false
	}
	for _, marker := range markers {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// accountCooldownSeconds returns the account-scoped cooldown length for a
// classified reason. The rate_limit tier is model-scoped and handled inside
// ReportCause (its doubling state lives on the account+model pair).
func accountCooldownSeconds(why string, cause FailureCause, now int64) int64 {
	switch why {
	case CooldownUpstreamError:
		return cooldownServerErr
	case CooldownQuotaExhausted:
		// 冷却到窗口重置时间或 30 分钟取小。A reset time in the past means the
		// window already rolled over, so the 30-minute floor applies instead of
		// a zero-length cooldown.
		if cause.ResetAt > now {
			if until := cause.ResetAt - now; until < cooldownQuotaSeconds {
				return until
			}
		}
		return cooldownQuotaSeconds
	case CooldownAuthExpired:
		return cooldownAuthSeconds
	case CooldownEgress:
		return cooldownEgressSeconds
	case CooldownNetwork:
		return cooldownNetSeconds
	default: // CooldownOther and any future tier
		return cooldownOtherSeconds
	}
}
