package config

import (
	"testing"

	"aiworkgate/internal/model"
)

// TestQuotaAlertsDefaults pins the shipped global thresholds.
func TestQuotaAlertsDefaults(t *testing.T) {
	c := Default()
	if c.Plugins.QuotaAlerts.RunsOutWarningDays != 7 {
		t.Fatalf("RunsOutWarningDays = %d, want 7", c.Plugins.QuotaAlerts.RunsOutWarningDays)
	}
	if c.Plugins.QuotaAlerts.BalanceLowRatio != 0.90 {
		t.Fatalf("BalanceLowRatio = %v, want 0.9", c.Plugins.QuotaAlerts.BalanceLowRatio)
	}
}

// TestQuotaAlertThresholdsForNoOverride returns the model defaults when the
// config carries no overrides.
func TestQuotaAlertThresholdsForNoOverride(t *testing.T) {
	c := QuotaAlertsConfig{}
	got := c.ThresholdsFor("p", nil)
	want := model.DefaultQuotaAlertThresholds()
	if got != want {
		t.Fatalf("ThresholdsFor = %+v, want %+v", got, want)
	}
}

// TestQuotaAlertPluginOverride pins the per-plugin override (D-21③): a plugin
// whose voucher only lives 3 days gets a shorter window, and the other rules
// keep their global values.
func TestQuotaAlertPluginOverride(t *testing.T) {
	c := QuotaAlertsConfig{
		RunsOutWarningDays:     7,
		ResetWarningDays:       7,
		ResetWarningUsageRatio: 0.30,
		BalanceLowRatio:        0.90,
		BalanceLowAbsolute:     100,
	}
	three := 3
	overrides := map[string]QuotaAlertPluginOverride{
		"short": {RunsOutWarningDays: &three},
	}

	got := c.ThresholdsFor("short", overrides)
	if got.RunsOutWarningDays != 3 {
		t.Fatalf("override RunsOutWarningDays = %d, want 3", got.RunsOutWarningDays)
	}
	if got.ResetWarningDays != 7 {
		t.Fatalf("override leaked into ResetWarningDays = %d, want 7", got.ResetWarningDays)
	}
	if got.BalanceLowRatio != 0.90 {
		t.Fatalf("override leaked into BalanceLowRatio = %v, want 0.9", got.BalanceLowRatio)
	}

	// A plugin without an override keeps the global value.
	if got := c.ThresholdsFor("other", overrides); got.RunsOutWarningDays != 7 {
		t.Fatalf("unoverridden plugin RunsOutWarningDays = %d, want 7", got.RunsOutWarningDays)
	}
}
