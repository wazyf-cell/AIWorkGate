package config

import "testing"

func TestDefaultMarketUpdateConfig(t *testing.T) {
	d := Default()
	if d.Plugins.MarketUpdateIntervalMinutes != DefaultMarketUpdateIntervalMinutes {
		t.Fatalf("interval = %d, want the 6h default %d",
			d.Plugins.MarketUpdateIntervalMinutes, DefaultMarketUpdateIntervalMinutes)
	}
	if d.Plugins.MarketUpdatePolicy != MarketUpdatePolicyPrompt {
		t.Fatalf("policy = %q, want prompt", d.Plugins.MarketUpdatePolicy)
	}
}

func TestNormalizeMarketUpdateConfig(t *testing.T) {
	// A zero interval means "off" (explicit disable, 0=关): Normalize must not
	// silently resurrect a default, or the off switch could never stay off. The
	// 360-minute default is delivered by Default(), i.e. fresh installs and the
	// settings UI.
	c := Config{}
	c.Normalize()
	if c.Plugins.MarketUpdateIntervalMinutes != 0 {
		t.Fatalf("zero interval must stay off, normalized to %d",
			c.Plugins.MarketUpdateIntervalMinutes)
	}
	if c.Plugins.MarketUpdatePolicy != MarketUpdatePolicyPrompt {
		t.Fatalf("zero policy normalized to %q, want prompt", c.Plugins.MarketUpdatePolicy)
	}

	// 0 stays disabled and an explicit auto with case/space survives.
	c = Config{Plugins: PluginsConfig{MarketUpdateIntervalMinutes: 0, MarketUpdatePolicy: " AUTO "}}
	c.Normalize()
	if c.Plugins.MarketUpdateIntervalMinutes != 0 {
		t.Fatalf("explicit 0 must stay disabled, got %d", c.Plugins.MarketUpdateIntervalMinutes)
	}
	if c.Plugins.MarketUpdatePolicy != MarketUpdatePolicyAuto {
		t.Fatalf("policy = %q, want auto", c.Plugins.MarketUpdatePolicy)
	}

	// An unrecognized policy is folded to prompt, not kept.
	c = Config{Plugins: PluginsConfig{MarketUpdatePolicy: "sometimes"}}
	c.Normalize()
	if c.Plugins.MarketUpdatePolicy != MarketUpdatePolicyPrompt {
		t.Fatalf("policy = %q, want prompt", c.Plugins.MarketUpdatePolicy)
	}

	// A negative interval is clamped to off (0), never to a leaky default.
	c = Config{Plugins: PluginsConfig{MarketUpdateIntervalMinutes: -7}}
	c.Normalize()
	if c.Plugins.MarketUpdateIntervalMinutes != 0 {
		t.Fatalf("negative interval clamped to %d, want 0", c.Plugins.MarketUpdateIntervalMinutes)
	}
}
