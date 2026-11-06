package scheduler

import "math"

// Three-state expiry classification (D-21④, P9-2).
//
// The old expiryRank collapsed every unknown expiry (expires_at <= 0) into
// math.MaxInt64. That made all unknown accounts tie, so credit_first fell
// through to Remaining and the accident was possible: a subscription that was
// about to expire but still held plenty of credits lost out to one that never
// reported an expiry at all.
//
// The fix keeps unknown as an explicit third state that sorts *after* every
// known expiry, but never ties with "never expires": it is neither treated as
// never-expire nor allowed to blank out the ordering among knowns.

// expiryState is the three-state expiry classification.
type expiryState int

const (
	expiryKnown   expiryState = iota // 到期确定（expires_at > 0）
	expiryUnknown                    // 到期未知（expires_at <= 0）
)

// expiryOf returns the state and the sort key for one expiry timestamp.
// Unknown accounts get math.MaxInt64 as their key, which is larger than every
// real timestamp, so a known-soon-to-expire account always wins over an
// unknown one — and two unknowns no longer collapse into a single value that
// the remaining comparator then decides.
func expiryOf(at int64) (expiryState, int64) {
	if at <= 0 {
		return expiryUnknown, math.MaxInt64
	}
	return expiryKnown, at
}

// expiryLess orders two expiry timestamps: state first (known before unknown),
// then the actual value.
func expiryLess(a, b int64) bool {
	sa, ka := expiryOf(a)
	sb, kb := expiryOf(b)
	if sa != sb {
		return sa < sb
	}
	return ka < kb
}

// expiringFirstLess is the expiring_first strategy comparator: sort by
// runsOutAt ascending (F-14 券烧完语义), and when runsOutAt is unknown fall
// back to the legacy expires_at three-state view rather than treating the
// account as never-expire. Ties resolve to remaining then id, so the order is
// total and deterministic.
func expiringFirstLess(a, b Account) bool {
	ra, ka := expiryOf(a.RunsOutAt)
	rb, kb := expiryOf(b.RunsOutAt)
	if ra != rb {
		return ra < rb
	}
	if ka != kb {
		return ka < kb
	}
	// Both runsOutAt unknown: fall back to the legacy expires_at three-state.
	ea, eaKey := expiryOf(a.ExpiresAt)
	eb, ebKey := expiryOf(b.ExpiresAt)
	if ea != eb {
		return ea < eb
	}
	if eaKey != ebKey {
		return eaKey < ebKey
	}
	if a.Remaining != b.Remaining {
		return a.Remaining > b.Remaining
	}
	return a.ID < b.ID
}
