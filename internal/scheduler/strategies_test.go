package scheduler

import (
	"testing"
	"time"
)

func TestStrategyFreePrefersUnmetered(t *testing.T) {
	now := testBase
	rich := account("rich")
	rich.Remaining, rich.Total = 900, 1000
	unlimited := account("unlimited")
	unlimited.Unlimited = true
	freeTier := account("freeplan")
	freeTier.Plan = "free"
	freeTier.Remaining, freeTier.Total = 10, 1000

	p := pool("p", rich, freeTier, unlimited)
	p.Strategy = StrategyFree
	s := New(Options{Now: func() time.Time { return time.Unix(now, 0) }})
	if got := pick(t, s, snapOf(p), "p/m1"); got.AccountID != "unlimited" {
		t.Fatalf("free picked %s, want the unlimited account", got.AccountID)
	}

	p2 := pool("p", rich, freeTier)
	p2.Strategy = StrategyFree
	s2 := New(Options{Now: func() time.Time { return time.Unix(now, 0) }})
	if got := pick(t, s2, snapOf(p2), "p/m1"); got.AccountID != "freeplan" {
		t.Fatalf("free picked %s, want the free-plan account", got.AccountID)
	}
}

func TestStrategyPacePrefersTimeRelativeSurplus(t *testing.T) {
	now := testBase
	// 80% left with 1h to reset (0.8/h) is ahead of 20% left with 100h (0.002/h).
	ahead := account("ahead")
	ahead.Remaining, ahead.Total = 80, 100
	ahead.ResetAt = now + 3600
	behind := account("behind")
	behind.Remaining, behind.Total = 20, 100
	behind.ResetAt = now + 100*3600

	p := pool("p", behind, ahead)
	p.Strategy = StrategyPace
	s := New(Options{Now: func() time.Time { return time.Unix(now, 0) }})
	if got := pick(t, s, snapOf(p), "p/m1"); got.AccountID != "ahead" {
		t.Fatalf("pace picked %s, want the account ahead of its burn line", got.AccountID)
	}

	// runsOutAt is the fallback when the bridge has no periodic reset.
	soon := account("soon")
	soon.Remaining, soon.Total = 90, 100
	soon.RunsOutAt = now + 7200
	p2 := pool("p", behind, soon)
	p2.Strategy = StrategyPace
	s2 := New(Options{Now: func() time.Time { return time.Unix(now, 0) }})
	if got := pick(t, s2, snapOf(p2), "p/m1"); got.AccountID != "soon" {
		t.Fatalf("pace picked %s, want the runsOutAt-based account", got.AccountID)
	}
}

func TestStrategyProbesUnmeasuredAccounts(t *testing.T) {
	now := testBase
	measured := account("measured")
	measured.Remaining, measured.Total = 100, 100
	blind := account("blind") // never published a quota number

	p := pool("p", measured, blind)
	p.Strategy = StrategyFree
	s := New(Options{Now: func() time.Time { return time.Unix(now, 0) }})
	seenBlind := false
	for i := 0; i < probeEvery; i++ {
		if got := pick(t, s, snapOf(p), "p/m1"); got.AccountID == "blind" {
			seenBlind = true
		}
	}
	if !seenBlind {
		t.Fatalf("unmeasured account never got a probe slot in %d picks", probeEvery)
	}
}
