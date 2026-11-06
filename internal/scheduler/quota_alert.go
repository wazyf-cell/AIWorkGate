package scheduler

import (
	"aiworkgate/internal/model"
)

// QuotaAlertSink delivers one quota alert. The app wires it to the existing
// internal/notify channel (one call per alert); a nil sink silently drops
// alerts so an unwired host never crashes.
type QuotaAlertSink interface {
	NotifyQuotaAlert(pluginID, kind, title, body string)
}

// EvaluateQuotaAlerts runs the three independent quota alert rules (D-21③)
// over every account of every pool in the snapshot and dispatches each hit
// through sink.
//
// Alerts are deliberately evaluated over the *full* account set rather than
// only the account the scheduler picked (D-21③): an unselected channel is
// still checked and notified, so a quota that is about to expire is never
// hidden behind scheduling choices. Channels without any quota concept
// (Unlimited) are skipped entirely — the evaluator never invents an empty
// alert for them.
func (s *Scheduler) EvaluateQuotaAlerts(snap Snapshot, now int64, th model.QuotaAlertThresholds, sink QuotaAlertSink) []model.QuotaAlert {
	var inputs []model.QuotaAlertInput
	for _, pool := range snap.Pools {
		if pool == nil {
			continue
		}
		for _, acc := range pool.Accounts {
			inputs = append(inputs, model.QuotaAlertInput{
				PluginID:  pool.PluginID,
				AccountID: acc.ID,
				Remaining: acc.Remaining,
				Total:     acc.Total,
				Unlimited: acc.Unlimited,
				RunsOutAt: acc.RunsOutAt,
				ResetAt:   acc.ResetAt,
				ExpiresAt: acc.ExpiresAt,
			})
		}
	}
	alerts := model.EvaluateQuotaAlerts(inputs, now, th)
	if sink != nil {
		for _, a := range alerts {
			sink.NotifyQuotaAlert(a.PluginID, a.Kind.String(), a.Title, a.Body)
		}
	}
	return alerts
}
