package scheduler

import (
	"strings"
	"testing"
	"time"
)

// D-46 (plans/2026-11-05 §D-46): failure dedup (NoPenalty) and the strategy
// fail-open. The selection layer must stay usable when the error follows the
// request rather than the account, and when a strategy comparison panics.

func TestReportCauseNoPenaltySkipsPenalty(t *testing.T) {
	clk := &clock{at: testBase}
	var logs []string
	// A plain report of the same failure keeps the full penalty...
	s := New(Options{Now: clk.now, Cooldowns: newFakeCooldowns()})
	s.ReportCause(AccountKey{PluginID: "p", AccountID: "first"}, "m1",
		FailureCause{Status: 503, Message: "upstream exploded"}, 0, 0)
	if _, ok := s.accountCD[AccountKey{PluginID: "p", AccountID: "first"}]; !ok {
		t.Fatal("first report of a signature lost its cooldown")
	}
	s.Close()

	store := newFakeCooldowns()
	s2 := New(Options{
		Now:       clk.now,
		Cooldowns: store,
		Log: func(msg string, kv map[string]any) {
			logs = append(logs, msg)
		},
	})
	defer s2.Close()
	key := AccountKey{PluginID: "p", AccountID: "a"}
	// ...the NoPenalty report records nothing at all: no cooldown, no strike,
	// no circuit evidence, no durable row.
	s2.ReportCause(key, "m1", FailureCause{Status: 503, Message: "upstream exploded", NoPenalty: true}, 0, 0)
	if _, ok := s2.accountCD[key]; ok {
		t.Fatal("NoPenalty report created an account cooldown")
	}
	if _, ok := s2.modelCD[modelCooldownKey{Key: key, Model: "m1"}]; ok {
		t.Fatal("NoPenalty report created a model cooldown")
	}
	if s2.pool5xx["p"] != 0 {
		t.Fatalf("pool5xx = %d, want 0 (no circuit evidence)", s2.pool5xx["p"])
	}
	s2.FlushCooldowns()
	if _, ok := store.row("p", "a", ""); ok {
		t.Fatal("NoPenalty report persisted a cooldown row")
	}

	// A plain report afterwards still has its full effect.
	s2.ReportCause(key, "m1", FailureCause{Status: 503, Message: "upstream exploded"}, 0, 0)
	if _, ok := s2.accountCD[key]; !ok {
		t.Fatal("plain report after NoPenalty lost its cooldown")
	}
	if len(logs) != 0 {
		t.Fatalf("NoPenalty wrote scheduler log lines: %v", logs)
	}

	// The rate-limit tier is covered too: NoPenalty must not grow the strikes.
	s3 := New(Options{Now: clk.now})
	defer s3.Close()
	s3.ReportCause(key, "m1", FailureCause{Status: 429, Message: "slow down", NoPenalty: true}, 0, 0)
	if state := s3.modelCD[modelCooldownKey{Key: key, Model: "m1"}]; state.Strikes != 0 {
		t.Fatalf("NoPenalty 429 strikes = %d, want 0", state.Strikes)
	}
}

func TestReportCauseNoPenaltyKeepsCredits(t *testing.T) {
	s := New(Options{Now: func() time.Time { return time.Unix(testBase, 0) }})
	defer s.Close()
	key := AccountKey{PluginID: "p", AccountID: "a"}
	s.ReportCause(key, "m1", FailureCause{Status: 500, Message: "boom", NoPenalty: true}, 0, 2.5)
	if got := s.Credits(key); got != 2.5 {
		t.Fatalf("credits = %v, want 2.5 (credit bookkeeping unaffected)", got)
	}
}

// TestStrategyPanicFallsBackToOriginalOrder locks the D-46② fail-open: a panic
// inside the strategy comparison is contained, logged, and answered with the
// unweighted original order instead of crashing the request.
func TestStrategyPanicFallsBackToOriginalOrder(t *testing.T) {
	var (
		logMsg    string
		logFields map[string]any
	)
	s := New(Options{
		Now:  func() time.Time { return time.Unix(testBase, 0) },
		Rand: func() float64 { panic("rng exploded") },
		Log: func(msg string, kv map[string]any) {
			logMsg = msg
			logFields = kv
		},
	})
	defer s.Close()

	// random draws from Rand on every Pick, so both the panic and the fallback
	// path run.
	p := pool("p", account("first"), account("second"))
	p.Strategy = StrategyRandom
	snap := snapOf(p)

	got := pick(t, s, snap, "p/m1")
	if got.AccountID != "first" {
		t.Fatalf("panic fallback picked %q, want the original order's first account", got.AccountID)
	}
	if logMsg != "scheduler strategy panic fallback" {
		t.Fatalf("log message = %q, want the fallback event", logMsg)
	}
	if logFields["plugin"] != "p" || logFields["strategy"] != StrategyRandom {
		t.Fatalf("log fields = %+v", logFields)
	}
	if kv, _ := logFields["panic"].(string); !strings.Contains(kv, "rng exploded") {
		t.Fatalf("panic value missing from log fields: %+v", logFields)
	}

	// A healthy strategy keeps working unchanged afterwards.
	s.randFn = func() float64 { return 0.99 }
	if got := pick(t, s, snap, "p/m1"); got.AccountID == "" {
		t.Fatal("post-panic Pick produced no account")
	}
}
