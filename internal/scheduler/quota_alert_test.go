package scheduler

import (
	"testing"
	"time"

	"aiworkgate/internal/model"
)

// recordingSink captures the alerts a Scheduler.EvaluateQuotaAlerts call
// dispatched.
type recordingSink struct {
	plugin string
	kind   string
	title  string
	body   string
	n      int
}

func (r *recordingSink) NotifyQuotaAlert(pluginID, kind, title, body string) {
	r.plugin, r.kind, r.title, r.body = pluginID, kind, title, body
	r.n++
}

// TestEvaluateQuotaAlertsIndependentOfScheduling pins rule ④: alerts run over
// every account of every pool, not just the one the scheduler picked, and
// a nil sink still returns the alerts (so an unwired host can inspect them).
func TestEvaluateQuotaAlertsIndependentOfScheduling(t *testing.T) {
	now := testBase
	selected := account("selected")
	selected.Remaining, selected.Total = 900, 1000
	unselected := account("unselected")
	unselected.Remaining, unselected.Total = 500, 1000
	unselected.RunsOutAt = now + 2*86400 // about to expire, but never picked

	p := pool("p", selected, unselected)
	p.Strategy = StrategyFixed
	s := New(Options{Now: func() time.Time { return time.Unix(now, 0) }})
	snap := snapOf(p)

	sink := &recordingSink{}
	alerts := s.EvaluateQuotaAlerts(*snap, now, model.DefaultQuotaAlertThresholds(), sink)

	// The unselected account must be flagged even though the scheduler never
	// picked it.
	foundUnselected := false
	for _, a := range alerts {
		if a.AccountID == "unselected" && a.Kind == model.QuotaAlertExpiring {
			foundUnselected = true
		}
	}
	if !foundUnselected {
		t.Fatalf("unselected account with near runsOutAt was not alerted: %+v", alerts)
	}
	if sink.n != len(alerts) {
		t.Fatalf("sink dispatched %d alerts, evaluator produced %d", sink.n, len(alerts))
	}

	// A nil sink must not panic and still returns the alerts.
	if got := s.EvaluateQuotaAlerts(*snap, now, model.DefaultQuotaAlertThresholds(), nil); len(got) != len(alerts) {
		t.Fatalf("nil sink returned %d alerts, want %d", len(got), len(alerts))
	}
}

// TestEvaluateQuotaAlertsSkipsUnlimited pins rule ⑤ at the scheduler boundary:
// an Unlimited account never reaches the evaluator's output.
func TestEvaluateQuotaAlertsSkipsUnlimited(t *testing.T) {
	now := testBase
	limited := account("limited")
	limited.Remaining, limited.Total = 5, 100
	limited.RunsOutAt = now + 1*86400
	unlimited := account("free")
	unlimited.Unlimited = true
	unlimited.Remaining, unlimited.Total = 0, 0

	p := pool("p", limited, unlimited)
	s := New(Options{Now: func() time.Time { return time.Unix(now, 0) }})
	alerts := s.EvaluateQuotaAlerts(*snapOf(p), now, model.DefaultQuotaAlertThresholds(), nil)
	for _, a := range alerts {
		if a.AccountID == "free" {
			t.Fatalf("unlimited account must not be alerted: %+v", a)
		}
	}
	if len(alerts) == 0 {
		t.Fatal("the limited account with a near runsOutAt must be alerted")
	}
}

// TestEvaluateQuotaAlertsPluginIDPropagated pins that the alert carries the
// owning pool's plugin id (needed by the notifier to route the event).
func TestEvaluateQuotaAlertsPluginIDPropagated(t *testing.T) {
	now := testBase
	a := account("a")
	a.Remaining, a.Total = 1, 100
	a.RunsOutAt = now + 1*86400
	p := pool("plugin-x", a)
	s := New(Options{Now: func() time.Time { return time.Unix(now, 0) }})
	alerts := s.EvaluateQuotaAlerts(*snapOf(p), now, model.DefaultQuotaAlertThresholds(), nil)
	if len(alerts) == 0 {
		t.Fatalf("expected at least one alert, got none")
	}
	for _, al := range alerts {
		if al.PluginID != "plugin-x" {
			t.Fatalf("alert plugin id = %q, want plugin-x", al.PluginID)
		}
	}
	var expiring bool
	for _, al := range alerts {
		if al.Kind == model.QuotaAlertExpiring {
			expiring = true
		}
	}
	if !expiring {
		t.Fatalf("expected an expiring alert for the near-runsOut account, got %+v", alerts)
	}
}
