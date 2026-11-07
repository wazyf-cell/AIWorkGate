package app

// D-21③ 运行时接线（P9-2）：把调度器的三类额度告警评估接进宿主周期任务，
// 命中后经 internal/notify 通道推送。评估器与阈值配置在 P9-2 已完成，此前缺
// 的是生产调用点——告警在运行态从不发出；本文件补上。未配置通知渠道时
// Dispatcher.Emit 是无害空操作，评估本身只读内存快照。

import (
	"context"
	"fmt"
	"time"

	"aiworkgate/internal/config"
	"aiworkgate/internal/notify"
	"aiworkgate/internal/scheduler"
)

// quotaAlertInterval is how often the three alert rules re-evaluate. Quota
// windows move on a day scale; ten minutes bounds the alert latency without
// making the snapshot fan-out a noticeable cost.
const quotaAlertInterval = 10 * time.Minute

// notifyQuotaAlertSink adapts the notify dispatcher to scheduler.QuotaAlertSink.
type notifyQuotaAlertSink struct{ d *notify.Dispatcher }

func (s notifyQuotaAlertSink) NotifyQuotaAlert(pluginID, kind, title, body string) {
	if s.d == nil {
		return
	}
	s.d.Emit(pluginID, notify.Event{Kind: quotaNotifyKind(kind), Title: title, Body: body})
}

// quotaNotifyKind maps the alert rule id onto the notify event kinds a user
// can subscribe to (notify.Kinds uses the same quota_* vocabulary).
func quotaNotifyKind(kind string) string {
	switch kind {
	case "expiring":
		return notify.KindQuotaExpiring
	case "resetting":
		return notify.KindQuotaResetting
	case "balance":
		return notify.KindQuotaLow
	}
	return notify.KindQuotaLow
}

// quotaAlertLoop re-evaluates the D-21③ rules while the host runs. The first
// pass waits a minute so startup fan-out (plugin discovery, snapshot warm-up)
// has settled before the rules read their first snapshot.
func (a *App) quotaAlertLoop() {
	select {
	case <-a.shutdownCh:
		return
	case <-time.After(time.Minute):
	}
	for {
		a.evaluateQuotaAlerts()
		select {
		case <-a.shutdownCh:
			return
		case <-time.After(quotaAlertInterval):
		}
	}
}

// evaluateQuotaAlerts runs the rules once over the current snapshot.
func (a *App) evaluateQuotaAlerts() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	snap, err := a.schedulerSnapshot(ctx)
	if err != nil || snap == nil {
		a.logger.Warn("app", "quota alerts skipped: no snapshot", map[string]any{"error": fmt.Sprint(err)})
		return
	}
	a.evaluateQuotaAlertsFrom(snap, a.configSnapshot(), time.Now().Unix())
}

// evaluateQuotaAlertsFrom evaluates each pool with its own effective
// thresholds (D-21③: per-plugin overrides, e.g. a platform whose voucher only
// lives 3 days keeps a shorter window than the global 7) and returns how many
// alerts fired.
func (a *App) evaluateQuotaAlertsFrom(snap *scheduler.Snapshot, cfg config.Config, now int64) int {
	if snap == nil {
		return 0
	}
	sink := notifyQuotaAlertSink{d: a.notify}
	fired := 0
	for id, pool := range snap.Pools {
		if pool == nil {
			continue
		}
		per := &scheduler.Snapshot{Pools: map[string]*scheduler.Pool{id: pool}}
		th := cfg.Plugins.QuotaAlerts.ThresholdsFor(id, cfg.Plugins.QuotaAlertPluginOverrides)
		fired += len(a.dispatch.EvaluateQuotaAlerts(*per, now, th, sink))
	}
	return fired
}
