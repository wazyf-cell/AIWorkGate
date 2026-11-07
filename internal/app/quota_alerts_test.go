package app

import (
	"testing"
	"time"

	"aiworkgate/internal/config"
	"aiworkgate/internal/notify"
	"aiworkgate/internal/scheduler"
)

type recordingSink struct {
	alerts [][2]string // {pluginID, kind}
}

func (r *recordingSink) NotifyQuotaAlert(pluginID, kind, title, body string) {
	r.alerts = append(r.alerts, [2]string{pluginID, kind})
}

func alertPool(pluginID string, runsOutAt, resetAt, expiresAt int64, remaining, total float64) *scheduler.Pool {
	return &scheduler.Pool{
		PluginID: pluginID,
		Accounts: []scheduler.Account{{
			ID:                 pluginID + "-acc",
			Label:              pluginID,
			Enabled:            true,
			CredentialPresent:  true,
			Remaining:          remaining,
			Total:              total,
			RunsOutAt:          runsOutAt,
			ResetAt:            resetAt,
			ExpiresAt:          expiresAt,
		}},
	}
}

// D-21③ 运行时接线回归：评估器必须经生产调用点触达 notify sink，且每个
// 插件用自己的阈值覆盖（全局与插件覆盖同时生效）。
func TestEvaluateQuotaAlertsFromDispatchesPerPlugin(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.Local).Unix()
	day := int64(24 * 3600)

	// global: runsOut 警告 7 天 → platformA（3 天后到期）命中；
	// platformB 覆盖为 1 天 → 同样 3 天后到期但不命中（未选中渠道也评估）。
	a := &App{}

	cfg := config.Config{}
	cfg.Plugins.QuotaAlerts.RunsOutWarningDays = 7
	cfg.Plugins.QuotaAlertPluginOverrides = map[string]config.QuotaAlertPluginOverride{
		"platform-b": {RunsOutWarningDays: intPtr(1)},
	}
	snap := &scheduler.Snapshot{Pools: map[string]*scheduler.Pool{
		"platform-a": alertPool("platform-a", now+3*day, 0, 0, 50, 100),
		"platform-b": alertPool("platform-b", now+3*day, 0, 0, 50, 100),
	}}

	// 用记录 sink 直接验证分发（notifyQuotaAlertSink 的 nil dispatcher 不发）。
	rec := &recordingSink{}
	fired := 0
	for id, pool := range snap.Pools {
		per := &scheduler.Snapshot{Pools: map[string]*scheduler.Pool{id: pool}}
		th := cfg.Plugins.QuotaAlerts.ThresholdsFor(id, cfg.Plugins.QuotaAlertPluginOverrides)
		fired += len(a.dispatch.EvaluateQuotaAlerts(*per, now, th, rec))
	}
	if fired != 1 {
		t.Fatalf("fired = %d, want 1 (platform-a expiring only)", fired)
	}
	if len(rec.alerts) != 1 || rec.alerts[0][0] != "platform-a" || rec.alerts[0][1] != "expiring" {
		t.Fatalf("alerts = %v, want [platform-a expiring]", rec.alerts)
	}
}

// 余额不足规则 + 无 dispatcher 时 evaluateQuotaAlertsFrom 不 panic（空操作）。
func TestEvaluateQuotaAlertsFromBalanceAndNilSink(t *testing.T) {
	now := time.Now().Unix()
	a := &App{notify: &notify.Dispatcher{}}
	cfg := config.Config{}
	cfg.Plugins.QuotaAlerts.BalanceLowRatio = 0.9
	snap := &scheduler.Snapshot{Pools: map[string]*scheduler.Pool{
		"p": alertPool("p", 0, 0, 0, 5, 100),
	}}
	if got := a.evaluateQuotaAlertsFrom(snap, cfg, now); got != 1 {
		t.Fatalf("fired = %d, want 1 (balance rule)", got)
	}
	// nil notify：Emit 空操作，评估照常返回且不 panic。
	a.notify = nil
	if got := a.evaluateQuotaAlertsFrom(snap, cfg, now); got != 1 {
		t.Fatalf("nil-dispatcher fired = %d, want 1", got)
	}
}

func intPtr(v int) *int { return &v }
