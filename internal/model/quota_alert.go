package model

import "fmt"

// Quota alert evaluation (P9-2, D-21). This file is deliberately pure logic:
// no store, no scheduler, no notify. It turns one account's quota view into a
// list of independent alert records, and the caller decides what to do with
// them.
//
// 三条规则互不覆盖（D-21③）：同一账号可以同时命中多条（例如券快烧完且余额
// 也见底），任一条命中都不抑制另一条。
//
// 到期语义（F-14 两类）在 Q1-A 下的兼容口径：
//   - runsOutAt  券/批次烧完（F-14 "runsOutAt"）。旧单组数据没有该字段 → 回落
//                到 ExpiresAt（旧语义就是"订阅到期"）。
//   - resetsAt   周期重置（F-14 "resetsAt"）。旧单组没有 → 该规则不触发。
//   - ExpiresAt <= 0 一律是「到期未知」三态之一（D-21④）：不当作永不过期，
//                也不产生"即将失效"告警。

// QuotaAlertKind is one of the three independent alert rules.
type QuotaAlertKind int

const (
	// QuotaAlertExpiring 券/批次将失效：runsOutAt（或旧单组 expires_at）落在
	// 告警窗口内。
	QuotaAlertExpiring QuotaAlertKind = iota
	// QuotaAlertResetting 周期重置前未用完：resetsAt 临近且 used/limit 偏低。
	QuotaAlertResetting
	// QuotaAlertBalance 余额见底：used/limit 超过阈值（或无总额时低于绝对值）。
	QuotaAlertBalance
)

func (k QuotaAlertKind) String() string {
	switch k {
	case QuotaAlertExpiring:
		return "expiring"
	case QuotaAlertResetting:
		return "resetting"
	case QuotaAlertBalance:
		return "balance"
	}
	return "unknown"
}

// QuotaAlertThresholds holds the three alert windows. Zero values mean "rule
// disabled": a rule whose threshold is <= 0 never fires, so an operator can
// silence one kind without touching the others.
type QuotaAlertThresholds struct {
	// RunsOutWarningDays is how many days before runsOutAt the expiring rule
	// fires.
	RunsOutWarningDays int
	// ResetWarningDays is how many days before resetsAt the resetting rule
	// fires.
	ResetWarningDays int
	// ResetWarningUsageRatio is the used/limit ratio below which the resetting
	// rule fires (i.e. "most of the quota will reset unused").
	ResetWarningUsageRatio float64
	// BalanceLowRatio is the used/limit ratio at/above which the balance rule
	// fires.
	BalanceLowRatio float64
	// BalanceLowAbsolute is the absolute remaining threshold used when the
	// plugin reports no total (legacy single-group data often has total=0).
	BalanceLowAbsolute float64
}

// DefaultQuotaAlertThresholds returns the shipped global defaults. Per-plugin
// overrides live in internal/config (P9-2 config scope) and are merged on top
// of these; a zero field in an override keeps the global value.
func DefaultQuotaAlertThresholds() QuotaAlertThresholds {
	return QuotaAlertThresholds{
		RunsOutWarningDays:     7,
		ResetWarningDays:       7,
		ResetWarningUsageRatio: 0.30,
		BalanceLowRatio:        0.90,
		BalanceLowAbsolute:     100,
	}
}

// QuotaAlertInput is the quota view of one account fed to the evaluator. It is
// intentionally separate from model.Account so the scheduler and the market
// (windows[] data, task-26) can both build it without coupling.
type QuotaAlertInput struct {
	PluginID  string
	AccountID string
	Remaining float64
	Total     float64
	Unlimited bool  // 无额度概念（zen-free）：不产生任何告警
	RunsOutAt int64 // 券烧完（F-14）；0 = 未知
	ResetAt   int64 // 周期重置（F-14）；0 = 未知
	ExpiresAt int64 // 旧单组语义；0 = 未知
}

// QuotaAlert is one emitted alert record.
type QuotaAlert struct {
	Kind      QuotaAlertKind
	PluginID  string
	AccountID string
	Title     string
	Body      string
}

const daySeconds = 86400

// EvaluateQuotaAlerts applies the three independent rules to every input and
// returns the alerts that fired. An input with Unlimited (zen-free / no quota
// concept) produces nothing at all — the evaluator must never invent an empty
// alert for a channel that has no quota to speak of.
func EvaluateQuotaAlerts(inputs []QuotaAlertInput, now int64, th QuotaAlertThresholds) []QuotaAlert {
	var out []QuotaAlert
	for _, in := range inputs {
		if in.Unlimited {
			continue
		}
		// used/limit: 1 when there is no usable total (conservative: treat as
		// fully used so the balance rule still has a chance to fire), clamped
		// to [0,1] otherwise.
		usedRatio := 1.0
		if in.Total > 0 {
			usedRatio = 1 - in.Remaining/in.Total
			if usedRatio < 0 {
				usedRatio = 0
			}
			if usedRatio > 1 {
				usedRatio = 1
			}
		}

		// ① 券/批次将失效：runsOutAt 优先，旧数据回落 expires_at。
		// 「到期未知」（两者都 <= 0）不触发、不当作永不过期。
		effective := in.RunsOutAt
		if effective <= 0 {
			effective = in.ExpiresAt
		}
		if effective > 0 && now < effective && effective-now <= int64(th.RunsOutWarningDays)*daySeconds {
			out = append(out, QuotaAlert{
				Kind: QuotaAlertExpiring, PluginID: in.PluginID, AccountID: in.AccountID,
				Title: "额度即将到期",
				Body:  fmtAmountExpiring(in, effective, now),
			})
		}

		// ② 周期重置前未用完：resetsAt 临近 且 使用率低于阈值。
		if in.ResetAt > 0 && now < in.ResetAt &&
			in.ResetAt-now <= int64(th.ResetWarningDays)*daySeconds &&
			usedRatio < th.ResetWarningUsageRatio {
			out = append(out, QuotaAlert{
				Kind: QuotaAlertResetting, PluginID: in.PluginID, AccountID: in.AccountID,
				Title: "额度即将重置",
				Body:  fmtAmountResetting(in, in.ResetAt, now, usedRatio),
			})
		}

		// ③ 余额见底：used/limit 超阈值，或无总额时低于绝对值。
		if th.BalanceLowRatio > 0 && in.Total > 0 && usedRatio >= th.BalanceLowRatio {
			out = append(out, QuotaAlert{
				Kind: QuotaAlertBalance, PluginID: in.PluginID, AccountID: in.AccountID,
				Title: "额度余额不足",
				Body:  fmtAmountBalance(in, usedRatio),
			})
		} else if th.BalanceLowAbsolute > 0 && in.Total <= 0 && in.Remaining < th.BalanceLowAbsolute {
			out = append(out, QuotaAlert{
				Kind: QuotaAlertBalance, PluginID: in.PluginID, AccountID: in.AccountID,
				Title: "额度余额不足",
				Body:  fmtAmountBalance(in, usedRatio),
			})
		}
	}
	return out
}

func fmtAmountExpiring(in QuotaAlertInput, effective, now int64) string {
	days := (effective - now) / daySeconds
	if days <= 0 {
		days = 1
	}
	return fmt.Sprintf("账户 %s 的额度将在 %d 天后到期（剩余 %g），请尽快使用",
		in.AccountID, days, in.Remaining)
}

func fmtAmountResetting(in QuotaAlertInput, resetAt, now int64, usedRatio float64) string {
	days := (resetAt - now) / daySeconds
	if days <= 0 {
		days = 1
	}
	return fmt.Sprintf("账户 %s 的额度将在 %d 天后重置，当前仅使用 %.1f%%，重置前请尽量用完",
		in.AccountID, days, usedRatio*100)
}

func fmtAmountBalance(in QuotaAlertInput, usedRatio float64) string {
	return fmt.Sprintf("账户 %s 的额度余额不足（剩余 %g，已用 %.1f%%）",
		in.AccountID, in.Remaining, usedRatio*100)
}
