package scheduler

// D-44 routing comparators: free and pace.
//
// free spends unmetered allowance first (unlimited, then a free plan), then
// the account with the most remaining quota. pace compares remaining ratio
// per hour left in the account's window (ResetAt, falling back to RunsOutAt),
// so an account that is behind its even-burn line gets the next request. Both
// give an unmeasured account a probe slot every probeEvery picks instead of
// starving it forever.
//
// cheap IS implemented, as the inter-pool mode InterPoolCheap in scheduler.go:
// candidate pools are ordered by the configured per-model token price from the
// D-31③ price engine, falling back to smart ordering when a price is unknown
// or equal. This file keeps only the free/pace account comparators.

import "strings"

// probeEvery is how many free/pace picks pass between forced probes of an
// account whose quota has never been measured.
const probeEvery = 10

// freshPaceHours is the even-burn horizon when the window end is unknown: the
// same one-week horizon magpie uses for FreshPace (provider/routing.go:633).
const freshPaceHours = 7 * 24.0

// freeFirstLess orders unmetered accounts first, then half-decayed quota.
func freeFirstLess(a, b Account) bool {
	if a.Unlimited != b.Unlimited {
		return a.Unlimited
	}
	af, bf := freePlan(a.Plan), freePlan(b.Plan)
	if af != bf {
		return af
	}
	ra := remainingRatio(a.Remaining, a.Total)
	rb := remainingRatio(b.Remaining, b.Total)
	if ra != rb {
		return ra > rb
	}
	if a.ExpiresAt != b.ExpiresAt {
		return expiryLess(a.ExpiresAt, b.ExpiresAt)
	}
	return a.ID < b.ID
}

// paceLess prefers the account with the most remaining quota per hour left.
func paceLess(a, b Account, now int64) bool {
	pa := paceOf(a, now)
	pb := paceOf(b, now)
	if pa != pb {
		return pa > pb
	}
	ra := remainingRatio(a.Remaining, a.Total)
	rb := remainingRatio(b.Remaining, b.Total)
	if ra != rb {
		return ra > rb
	}
	if a.ExpiresAt != b.ExpiresAt {
		return expiryLess(a.ExpiresAt, b.ExpiresAt)
	}
	return a.ID < b.ID
}

func paceOf(a Account, now int64) float64 {
	return remainingRatio(a.Remaining, a.Total) / hoursUntilWindowEnd(a, now)
}

// hoursUntilWindowEnd uses the periodic reset when the bridge reported one,
// else the券/batch run-out time; an unknown end gets the one-week horizon
// rather than infinity (which would make every account tie at zero pace).
func hoursUntilWindowEnd(a Account, now int64) float64 {
	end := a.ResetAt
	if end <= 0 {
		end = a.RunsOutAt
	}
	if end <= 0 || end <= now {
		return freshPaceHours
	}
	hours := float64(end-now) / 3600
	if hours < 1 {
		return 1
	}
	return hours
}

func freePlan(plan string) bool {
	return strings.EqualFold(strings.TrimSpace(plan), "free")
}

// measured reports whether an account ever published a quota number.
func measured(a Account) bool {
	return a.Unlimited || a.Total > 0 || a.Remaining > 0
}

// probePickLocked returns an unmeasured account on every probeEvery-th pick.
// Caller holds s.mu.
func (s *Scheduler) probePickLocked(accounts []Account) (int, bool) {
	s.probeSeq++
	if s.probeSeq%probeEvery != 0 {
		return 0, false
	}
	for i := range accounts {
		if !measured(accounts[i]) {
			return i, true
		}
	}
	return 0, false
}
