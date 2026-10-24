package plugins

import (
	"testing"

	"aiworkgate/internal/config"
	"aiworkgate/internal/model"
	"aiworkgate/internal/store"
)

// TestDeriveSettingsPrecedence pins the global-vs-per-plugin rule: the persisted
// row overrides only where the user actually set it (non-NULL strict, non-zero
// concurrency); otherwise the global/manifest base wins.
func TestDeriveSettingsPrecedence(t *testing.T) {
	cfg := config.Default()
	cfg.Plugins.Concurrency = 2
	cfg.Plugins.StrictEgress = true // global default forces strict

	mgr := New(Options{Config: func() config.Config { return cfg }})
	t.Cleanup(func() { _ = mgr.Close() })

	mf := model.Manifest{
		ID:          "demo",
		Startup:     model.StartupConfig{Mode: ""},
		Concurrency: model.ConcurrencyConfig{Suggested: 0},
		Egress:      model.EgressConfig{Strict: false},
		Checkin:     model.CheckinConfig{DefaultTime: ""},
	}

	// Base with no persisted row: global concurrency and global strict apply.
	base := mgr.deriveSettings("demo", mf, map[string]store.PluginSetting{})
	if base.concurrency != 2 {
		t.Fatalf("no-row concurrency = %d, want global 2", base.concurrency)
	}
	if !base.strict {
		t.Fatalf("no-row strict = false, want global true")
	}

	// Row present but unset for concurrency (0) and strict (StrictSet=false):
	// still 跟随全局.
	follow := mgr.deriveSettings("demo", mf, map[string]store.PluginSetting{
		"demo": {Enabled: true, Mode: "", Concurrency: 0, Strict: false, StrictSet: false},
	})
	if follow.concurrency != 2 {
		t.Fatalf("follow concurrency = %d, want global 2", follow.concurrency)
	}
	if !follow.strict {
		t.Fatalf("follow strict = false, want global true (unset row must not override)")
	}

	// Row present with explicit overrides wins over the global defaults.
	set := mgr.deriveSettings("demo", mf, map[string]store.PluginSetting{
		"demo": {Enabled: true, Mode: model.StartupOnDemand, Concurrency: 5, Strict: false, StrictSet: true, CheckinTime: "08:30"},
	})
	if set.concurrency != 5 {
		t.Fatalf("explicit concurrency = %d, want row value 5", set.concurrency)
	}
	if set.strict {
		t.Fatalf("explicit strict = true, want row override false")
	}
	if set.mode != model.StartupOnDemand {
		t.Fatalf("mode = %q, want on_demand", set.mode)
	}
	if set.checkin != "08:30" {
		t.Fatalf("checkin = %q, want 08:30", set.checkin)
	}
}
