package app

import (
	"testing"

	"aiworkgate/internal/model"
)

// A fresh install has no stored settings. The TTLs and the hedge threshold must
// come from the documented baselines, otherwise the upstream KV cache is never
// reused and hedging never fires — the fields exist but are silently inert.
func TestPoolDefaultsFreshInstallUsesBaseline(t *testing.T) {
	got := poolDefaults(model.PoolSettings{}, false)

	if got.AccountConcurrency != 0 {
		t.Fatalf("account_concurrency = %d, want 0 (unlimited): a limit here would throttle pools that work today",
			got.AccountConcurrency)
	}
	if got.PoolStickyTTL != defaultPoolStickyTTL {
		t.Fatalf("pool_sticky_ttl = %d, want %d", got.PoolStickyTTL, defaultPoolStickyTTL)
	}
	if got.ConvStickyTTL != defaultConvStickyTTL {
		t.Fatalf("conv_sticky_ttl = %d, want %d", got.ConvStickyTTL, defaultConvStickyTTL)
	}
	if got.HedgeMS != defaultHedgeMS {
		t.Fatalf("hedge_ms = %d, want %d", got.HedgeMS, defaultHedgeMS)
	}
}

// The hedge baseline must stay inside the band the rest of the host enforces,
// or a fresh install would carry a value the API rejects on the first edit.
func TestHedgeBaselineIsWithinAcceptedRange(t *testing.T) {
	if defaultHedgeMS != 0 && (defaultHedgeMS < minSchedulerHedge || defaultHedgeMS > maxSchedulerHedge) {
		t.Fatalf("defaultHedgeMS = %d, outside the accepted range %d..%d",
			defaultHedgeMS, minSchedulerHedge, maxSchedulerHedge)
	}
	// dispatch.go only hedges when the threshold is at least 1s.
	if defaultHedgeMS != 0 && defaultHedgeMS < 1000 {
		t.Fatalf("defaultHedgeMS = %d, below the 1s floor dispatch honours", defaultHedgeMS)
	}
}

// A stored row is the operator's decision. A stored zero means "off" or
// "unlimited" and must NOT be replaced by the baseline — otherwise upgrading
// would silently switch hedging on for someone who turned it off.
func TestPoolDefaultsStoredZeroIsNotOverriddenByBaseline(t *testing.T) {
	stored := model.PoolSettings{
		Strategy:           "credit_first",
		AccountConcurrency: 0,
		PoolStickyTTL:      0,
		ConvStickyTTL:      0,
		HedgeMS:            0,
	}
	got := poolDefaults(stored, true)

	if got.PoolStickyTTL != 0 {
		t.Fatalf("pool_sticky_ttl = %d, want the stored 0 to survive", got.PoolStickyTTL)
	}
	if got.ConvStickyTTL != 0 {
		t.Fatalf("conv_sticky_ttl = %d, want the stored 0 to survive", got.ConvStickyTTL)
	}
	if got.HedgeMS != 0 {
		t.Fatalf("hedge_ms = %d, want the stored 0 (off) to survive", got.HedgeMS)
	}
	if got.AccountConcurrency != 0 {
		t.Fatalf("account_concurrency = %d, want 0", got.AccountConcurrency)
	}
}

// A stored non-zero value always wins over the baseline.
func TestPoolDefaultsStoredValuesWin(t *testing.T) {
	stored := model.PoolSettings{
		Strategy:           "fixed",
		AccountConcurrency: 3,
		PoolStickyTTL:      120,
		ConvStickyTTL:      240,
		HedgeMS:            1500,
	}
	got := poolDefaults(stored, true)

	if got.Strategy != "fixed" {
		t.Fatalf("strategy = %q, want stored", got.Strategy)
	}
	if got.AccountConcurrency != 3 || got.PoolStickyTTL != 120 ||
		got.ConvStickyTTL != 240 || got.HedgeMS != 1500 {
		t.Fatalf("stored values lost: %+v", got)
	}
}
