package scheduler

import (
	"context"
	"math"
	"testing"
	"time"

	"aiworkgate/internal/model"
)

// TestExpiryThreeState pins the three-state expiry ranking (D-21④): a known
// expiry always sorts before an unknown one, and two unknowns no longer collapse
// into a single math.MaxInt64 value that the remaining comparator then decides.
func TestExpiryThreeState(t *testing.T) {
	now := int64(1_000_000)

	// A known future expiry ranks before an unknown one.
	if !expiryLess(now+86400, 0) {
		t.Fatal("known future expiry must rank before unknown")
	}
	// Two unknowns tie on state and key; expiryLess must not claim a strict
	// order between them (the caller's remaining/id comparator decides).
	if expiryLess(0, 0) {
		t.Fatal("two unknowns must not be strictly ordered by expiry alone")
	}
	// Two known values order by value.
	if !expiryLess(now+86400, now+2*86400) {
		t.Fatal("known expiries must order by value")
	}
	// A past known expiry still ranks before unknown (it is known, not absent).
	if !expiryLess(now-10, 0) {
		t.Fatal("a past known expiry must rank before unknown")
	}
}

// pickOne runs Pick for one physical model and returns the chosen account id.
func pickOne(t *testing.T, s *Scheduler, snap *Snapshot, modelID string) string {
	t.Helper()
	d, err := s.Pick(context.Background(), snap, Request{Model: modelID})
	if err != nil {
		t.Fatalf("Pick(%s): %v", modelID, err)
	}
	return d.AccountID
}

// TestCreditFirstKnownWinsOverUnknown is the accident regression (D-21③): an
// account whose expiry is known and soon must be picked over one with more
// remaining but no expiry.
func TestCreditFirstKnownWinsOverUnknown(t *testing.T) {
	now := testBase
	known := account("known")
	known.Remaining, known.Total = 10, 100
	known.ExpiresAt = now + 3*86400
	unknown := account("unknown")
	unknown.Remaining, unknown.Total = 999, 1000
	unknown.ExpiresAt = 0

	s := New(Options{Now: func() time.Time { return time.Unix(now, 0) }})
	p := pool("p", known, unknown)
	p.Strategy = StrategyCreditFirst
	if got := pickOne(t, s, snapOf(p), "p/m1"); got != "known" {
		t.Fatalf("credit_first picked %q, want known (soonest expiry wins over more remaining)", got)
	}
}

// TestCreditFirstUnknownFallsToRemaining keeps the old behaviour for the
// all-unknown case: with no expiry data at all, remaining decides.
func TestCreditFirstUnknownFallsToRemaining(t *testing.T) {
	now := testBase
	a := account("a")
	a.Remaining, a.Total = 5, 10
	b := account("b")
	b.Remaining, b.Total = 9, 10

	s := New(Options{Now: func() time.Time { return time.Unix(now, 0) }})
	p := pool("p", a, b)
	p.Strategy = StrategyCreditFirst
	if got := pickOne(t, s, snapOf(p), "p/m1"); got != "b" {
		t.Fatalf("credit_first with all-unknown picked %q, want b (more remaining)", got)
	}
}

// TestExpiringFirstPicksNearestRunsOut pins the expiring_first strategy: it
// orders by runsOutAt ascending (F-14), not by remaining.
func TestExpiringFirstPicksNearestRunsOut(t *testing.T) {
	now := testBase
	far := account("far")
	far.Remaining, far.Total = 900, 1000
	far.RunsOutAt = now + 30*86400
	soon := account("soon")
	soon.Remaining, soon.Total = 100, 1000
	soon.RunsOutAt = now + 2*86400
	unknown := account("unknown")
	unknown.Remaining, unknown.Total = 950, 1000
	unknown.RunsOutAt, unknown.ExpiresAt = 0, 0

	s := New(Options{Now: func() time.Time { return time.Unix(now, 0) }})
	p := pool("p", far, soon, unknown)
	p.Strategy = StrategyExpiringFirst
	if got := pickOne(t, s, snapOf(p), "p/m1"); got != "soon" {
		t.Fatalf("expiring_first picked %q, want soon (nearest runsOutAt)", got)
	}
}

// TestExpiringFirstFallsBackToExpiresAt checks the legacy fallback: when
// runsOutAt is unknown the account is ordered by expires_at, and an unknown
// both is last — never treated as never-expire.
func TestExpiringFirstFallsBackToExpiresAt(t *testing.T) {
	now := testBase
	legacy := account("legacy")
	legacy.Remaining, legacy.Total = 5, 100
	legacy.ExpiresAt = now + 5*86400
	none := account("none")
	none.Remaining, none.Total = 50, 100

	s := New(Options{Now: func() time.Time { return time.Unix(now, 0) }})
	p := pool("p", none, legacy)
	p.Strategy = StrategyExpiringFirst
	if got := pickOne(t, s, snapOf(p), "p/m1"); got != "legacy" {
		t.Fatalf("expiring_first fallback picked %q, want legacy (expires_at known)", got)
	}
}

// TestPoolExpiryIgnoresUnknown pins that a pool whose accounts all report no
// expiry must not look "more available forever" than one that does.
func TestPoolExpiryIgnoresUnknown(t *testing.T) {
	now := testBase
	allUnknown := []Account{{ID: "a"}, {ID: "b"}}
	if got := poolExpiry(allUnknown); got != math.MaxInt64 {
		t.Fatalf("poolExpiry(all unknown) = %d, want MaxInt64", got)
	}
	known := []Account{{ID: "a"}, {ID: "b", ExpiresAt: now + 3*86400}}
	if got := poolExpiry(known); got != now+3*86400 {
		t.Fatalf("poolExpiry(mixed) = %d, want the known expiry", got)
	}
}

// TestValidStrategyIncludesExpiringFirst pins the enum wiring.
func TestValidStrategyIncludesExpiringFirst(t *testing.T) {
	if !ValidStrategy(StrategyExpiringFirst) {
		t.Fatal("expiring_first must be a valid strategy")
	}
	found := false
	for _, n := range StrategyNames {
		if n == StrategyExpiringFirst {
			found = true
		}
	}
	if !found {
		t.Fatal("expiring_first must appear in StrategyNames")
	}
}

// ensure model import is used (pool() helper references it via the shared
// test helpers); this file otherwise only needs the scheduler package.
var _ = model.ModelInfo{}
