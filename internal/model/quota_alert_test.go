package model

import "testing"

// TestEvaluateQuotaAlertsRunsOutNear is the acceptance echo: constructing
// runsOutAt = today + 3 days must fire the expiring alert.
func TestEvaluateQuotaAlertsRunsOutNear(t *testing.T) {
	now := int64(1_700_000_000)
	th := DefaultQuotaAlertThresholds()
	inputs := []QuotaAlertInput{{
		PluginID: "p", AccountID: "a", Remaining: 500, Total: 1000,
		RunsOutAt: now + 3*86400,
	}}
	got := EvaluateQuotaAlerts(inputs, now, th)
	if len(got) == 0 {
		t.Fatal("runsOutAt=today+3d must fire an alert")
	}
	if got[0].Kind != QuotaAlertExpiring {
		t.Fatalf("first alert kind = %v, want expiring", got[0].Kind)
	}
}

// TestEvaluateQuotaAlertsThreeWaysNonOverlap pins that the three rules are
// independent: an account can hit more than one, and one rule firing does not
// suppress the others.
func TestEvaluateQuotaAlertsThreeWaysNonOverlap(t *testing.T) {
	now := int64(1_700_000_000)
	th := DefaultQuotaAlertThresholds()

	// One account that hits expiring AND balance simultaneously.
	both := QuotaAlertInput{
		PluginID: "p", AccountID: "both", Remaining: 10, Total: 100,
		RunsOutAt: now + 2*86400,
	}
	got := EvaluateQuotaAlerts([]QuotaAlertInput{both}, now, th)
	if len(got) != 2 {
		t.Fatalf("expected both expiring and balance, got %d alerts: %+v", len(got), got)
	}
	kinds := map[QuotaAlertKind]bool{}
	for _, a := range got {
		kinds[a.Kind] = true
	}
	if !kinds[QuotaAlertExpiring] || !kinds[QuotaAlertBalance] {
		t.Fatalf("expected expiring+balance, got %v", kinds)
	}

	// A dedicated resetting case.
	resetting := QuotaAlertInput{
		PluginID: "p", AccountID: "reset", Remaining: 90, Total: 100,
		ResetAt: now + 3*86400,
	}
	got = EvaluateQuotaAlerts([]QuotaAlertInput{resetting}, now, th)
	if len(got) != 1 || got[0].Kind != QuotaAlertResetting {
		t.Fatalf("resetting case: got %+v, want exactly one resetting alert", got)
	}
}

// TestEvaluateQuotaAlertsZenFreeNoAlert pins rule ⑤: a channel without a quota
// concept (Unlimited) produces no alerts at all.
func TestEvaluateQuotaAlertsZenFreeNoAlert(t *testing.T) {
	now := int64(1_700_000_000)
	th := DefaultQuotaAlertThresholds()
	inputs := []QuotaAlertInput{
		{PluginID: "p", AccountID: "free", Unlimited: true},
		{PluginID: "p", AccountID: "free2", Unlimited: true, RunsOutAt: now + 1*86400, ResetAt: now + 2*86400},
	}
	got := EvaluateQuotaAlerts(inputs, now, th)
	if len(got) != 0 {
		t.Fatalf("zen-free must produce no alerts, got %+v", got)
	}
}

// TestEvaluateQuotaAlertsUnknownExpiryNoAlert pins that an unknown expiry
// (expires_at = 0, runsOutAt = 0) is neither treated as never-expire nor alerted.
func TestEvaluateQuotaAlertsUnknownExpiryNoAlert(t *testing.T) {
	now := int64(1_700_000_000)
	th := DefaultQuotaAlertThresholds()
	inputs := []QuotaAlertInput{
		{PluginID: "p", AccountID: "unknown", Remaining: 500, Total: 1000},
		{PluginID: "p", AccountID: "zero", Remaining: 0, Total: 0},
	}
	got := EvaluateQuotaAlerts(inputs, now, th)
	for _, a := range got {
		if a.Kind == QuotaAlertExpiring {
			t.Fatalf("unknown expiry must not fire the expiring alert: %+v", a)
		}
	}
}

// TestEvaluateQuotaAlertsLegacyExpiresAtFallback pins the Q1-A compatibility
// rule: legacy single-group data (no runsOutAt) uses expires_at for the
// expiring rule.
func TestEvaluateQuotaAlertsLegacyExpiresAtFallback(t *testing.T) {
	now := int64(1_700_000_000)
	th := DefaultQuotaAlertThresholds()
	inputs := []QuotaAlertInput{
		{PluginID: "p", AccountID: "legacy", Remaining: 500, Total: 1000, ExpiresAt: now + 2*86400},
	}
	got := EvaluateQuotaAlerts(inputs, now, th)
	if len(got) != 1 || got[0].Kind != QuotaAlertExpiring {
		t.Fatalf("legacy expires_at fallback: got %+v, want one expiring alert", got)
	}
}

// TestEvaluateQuotaAlertsThresholdDisabled pins that a zero threshold disables
// that rule without affecting the others.
func TestEvaluateQuotaAlertsThresholdDisabled(t *testing.T) {
	now := int64(1_700_000_000)
	th := DefaultQuotaAlertThresholds()
	th.RunsOutWarningDays = 0 // silence the expiring rule only
	inputs := []QuotaAlertInput{
		{PluginID: "p", AccountID: "a", Remaining: 5, Total: 100, RunsOutAt: now + 2*86400},
	}
	got := EvaluateQuotaAlerts(inputs, now, th)
	if len(got) != 1 || got[0].Kind != QuotaAlertBalance {
		t.Fatalf("disabled expiring rule: got %+v, want only balance", got)
	}
}

// TestEvaluateQuotaAlertsPastExpiryNoAlert: an expiry already in the past is
// not "即将" (it is expired, handled elsewhere).
func TestEvaluateQuotaAlertsPastExpiryNoAlert(t *testing.T) {
	now := int64(1_700_000_000)
	th := DefaultQuotaAlertThresholds()
	inputs := []QuotaAlertInput{
		{PluginID: "p", AccountID: "past", Remaining: 500, Total: 1000, RunsOutAt: now - 86400},
	}
	got := EvaluateQuotaAlerts(inputs, now, th)
	for _, a := range got {
		if a.Kind == QuotaAlertExpiring {
			t.Fatalf("a past expiry must not fire the expiring alert: %+v", a)
		}
	}
}
