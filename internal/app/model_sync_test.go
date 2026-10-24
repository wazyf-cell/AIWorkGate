package app

import (
	"testing"
	"time"
)

func TestSyncRetryBackoffSchedule(t *testing.T) {
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.Local)
	today := "2026-10-21"
	raw := ""
	for i, want := range []time.Duration{30 * time.Minute, time.Hour, 2 * time.Hour, 2 * time.Hour} {
		raw = nextRetry(raw, today, now)
		state := parseSyncRetry(raw)
		if state.attempts != i+1 {
			t.Fatalf("attempt %d: attempts = %d, want %d", i+1, state.attempts, i+1)
		}
		if got := time.Unix(state.next, 0).Sub(now); got != want {
			t.Fatalf("attempt %d: wait = %v, want %v", i+1, got, want)
		}
		if state.day != today {
			t.Fatalf("attempt %d: day = %q, want %q", i+1, state.day, today)
		}
	}
}

func TestSyncRetryResetsOnNewDay(t *testing.T) {
	today := "2026-10-21"
	raw := "2026-10-20|6|9999999999"
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.Local)
	if !retryDue(raw, today, now) {
		t.Fatal("a state from yesterday must not block today's run")
	}
	state := parseSyncRetry(nextRetry(raw, today, now))
	if state.attempts != 1 {
		t.Fatalf("attempts after a new day = %d, want 1", state.attempts)
	}
	if got := time.Unix(state.next, 0).Sub(now); got != 30*time.Minute {
		t.Fatalf("wait after a new day = %v, want 30m", got)
	}
}

func TestSyncRetryDue(t *testing.T) {
	today := "2026-10-21"
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.Local)
	at := func(d time.Duration) string {
		return nextRetry("", today, now.Add(d))
	}
	for _, tc := range []struct {
		name string
		raw  string
		want bool
	}{
		{"malformed state never blocks", "garbage", true},
		{"empty state never blocks", "", true},
		{"before the wait", at(0), false},
		{"exactly at the wait", at(-30 * time.Minute), true},
		{"after the wait", at(-time.Hour), true},
	} {
		if got := retryDue(tc.raw, today, now); got != tc.want {
			t.Errorf("%s: retryDue = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestParseSyncRetryRejectsBadValues(t *testing.T) {
	for _, raw := range []string{"", "x", "2026-10-21", "2026-10-21|0|1", "2026-10-21|x|1", "2026-10-21|1|x", "2026-10-21|-2|1", "a|1|2|3"} {
		if state := parseSyncRetry(raw); state != (syncRetry{}) {
			t.Errorf("parseSyncRetry(%q) = %+v, want zero", raw, state)
		}
	}
}
