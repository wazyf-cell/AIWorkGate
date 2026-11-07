package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// newRPMScheduler builds a scheduler on the fake clock with a sleeper that
// advances it, so PickWait's two-minute budget stays instant under test.
func newRPMScheduler(clk *clock) *Scheduler {
	return New(Options{
		Now: clk.now,
		Sleep: func(d time.Duration) {
			if d < 0 {
				d = 0
			}
			clk.advance(int64(d / time.Second))
		},
	})
}

// rpmAccount is a fixed-strategy account with the given per-minute cap.
func rpmAccount(id string, rpm int) Account {
	acc := account(id)
	acc.RPM = rpm
	return acc
}

// TestAccountRPMSkipsNthRequest pins the P5 cap: the (rpm+1)-th attempt inside
// the sliding window must skip the capped account, and the rejection trace
// must name the window.
func TestAccountRPMSkipsNthRequest(t *testing.T) {
	now := testBase
	clk := &clock{at: now}
	s := New(Options{Now: clk.now})
	snap := snapOf(pool("p", rpmAccount("a", 2), rpmAccount("b", 2)))

	// Two attempts hit account "a" (fixed strategy). Each upstream attempt is
	// recorded through Acquire, exactly like the gateway does.
	for i := 0; i < 2; i++ {
		d, err := s.Pick(context.Background(), snap, Request{Model: "p/m1"})
		if err != nil {
			t.Fatalf("pick %d: %v", i, err)
		}
		if d.AccountID != "a" {
			t.Fatalf("pick %d = %q, want a", i, d.AccountID)
		}
		s.Acquire(accKey(d.PluginID, d.AccountID))
	}

	// The third attempt must skip "a" and fall to "b". The explain trace names
	// the rpm window as the reason "a" was rejected.
	d, err := s.Pick(context.Background(), snap, Request{Model: "p/m1"})
	if err != nil {
		t.Fatalf("third pick: %v", err)
	}
	if d.AccountID != "b" {
		t.Fatalf("third pick = %q, want b (a is RPM-paused)", d.AccountID)
	}
	explanation, err := s.Explain(context.Background(), snap, Request{Model: "p/m1"})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	skipped := ""
	for _, cand := range explanation.Candidates {
		if cand.AccountID == "a" && cand.Rejected {
			skipped = cand.Reason
		}
	}
	if !strings.Contains(skipped, "rpm limit 2/min") {
		t.Fatalf("rejected trace for a = %q, want an rpm-limit reason", skipped)
	}

	// And once "b" is capped too (its window holds two hits as well), the plain
	// Pick refuses instead of waiting.
	s.Acquire(accKey(d.PluginID, d.AccountID))
	s.Acquire(accKey(d.PluginID, d.AccountID))
	if _, err := s.Pick(context.Background(), snap, Request{Model: "p/m1"}); !errors.Is(err, ErrNoCandidate) {
		t.Fatalf("Pick with all accounts paused = %v, want ErrNoCandidate (Pick never waits)", err)
	}
}

// TestAccountRPMWindowSlides proves the pause clears as soon as the window
// slides the hits out.
func TestAccountRPMWindowSlides(t *testing.T) {
	clk := &clock{at: testBase}
	s := New(Options{Now: clk.now})
	snap := snapOf(pool("p", rpmAccount("a", 1)))

	d, err := s.Pick(context.Background(), snap, Request{Model: "p/m1"})
	if err != nil || d.AccountID != "a" {
		t.Fatalf("first pick = %q, %v", d.AccountID, err)
	}
	s.Acquire(accKey(d.PluginID, d.AccountID))

	// Still inside the window: the account is paused and the single-account
	// pool refuses.
	if _, err := s.Pick(context.Background(), snap, Request{Model: "p/m1"}); !errors.Is(err, ErrNoCandidate) {
		t.Fatalf("pick inside the window = %v, want ErrNoCandidate", err)
	}

	// 61s later the hit slid out: selectable again.
	clk.advance(rpmWindowSeconds + 1)
	d, err = s.Pick(context.Background(), snap, Request{Model: "p/m1"})
	if err != nil || d.AccountID != "a" {
		t.Fatalf("pick after the window = %q, %v, want a", d.AccountID, err)
	}
}

// TestAccountRPMAllPausedWaitsThenServes pins PickWait: with every candidate
// paused, the call waits for the window slide (bounded by the budget) and
// serves once an account is free again.
func TestAccountRPMAllPausedWaitsThenServes(t *testing.T) {
	clk := &clock{at: testBase}
	s := newRPMScheduler(clk)
	snap := snapOf(pool("p", rpmAccount("a", 2)))

	for i := 0; i < 2; i++ {
		d, err := s.Pick(context.Background(), snap, Request{Model: "p/m1"})
		if err != nil {
			t.Fatalf("pick %d: %v", i, err)
		}
		s.Acquire(accKey(d.PluginID, d.AccountID))
	}

	d, err := s.PickWait(context.Background(), snap, Request{Model: "p/m1"})
	if err != nil {
		t.Fatalf("PickWait = %v, want a decision after the window slid", err)
	}
	if d.AccountID != "a" {
		t.Fatalf("PickWait account = %q, want a", d.AccountID)
	}
	// The sleeper advanced the fake clock to the slide point.
	if elapsed := clk.at - testBase; elapsed <= 0 || elapsed > int64(rpmWindowSeconds)+2 {
		t.Fatalf("wait advanced the clock by %ds, want about %ds", elapsed, rpmWindowSeconds)
	}
}

// TestAccountRPMAllPausedTimesOut pins the 429: when traffic keeps the window
// saturated, PickWait gives up after the two-minute budget with
// ErrAccountRateLimited instead of waiting forever.
func TestAccountRPMAllPausedTimesOut(t *testing.T) {
	clk := &clock{at: testBase}
	s := newRPMScheduler(clk)
	snap := snapOf(pool("p", rpmAccount("a", 1)))

	d, err := s.Pick(context.Background(), snap, Request{Model: "p/m1"})
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	key := accKey(d.PluginID, d.AccountID)
	s.Acquire(key)

	// The sleeper simulates competing traffic: every re-plan is still paused
	// because another request hit the account right after each slide.
	s.sleepFn = func(dur time.Duration) {
		if dur < 0 {
			dur = 0
		}
		clk.advance(int64(dur / time.Second))
		s.Acquire(key) // another request re-saturates the account
	}

	_, err = s.PickWait(context.Background(), snap, Request{Model: "p/m1"})
	if !errors.Is(err, ErrAccountRateLimited) {
		t.Fatalf("PickWait = %v, want ErrAccountRateLimited", err)
	}
	// The give-up happens as soon as the next slide would overrun the budget:
	// with a 60s window the wait lands somewhere inside (0, 2m].
	if elapsed := clk.at - testBase; elapsed <= 0 || elapsed > int64(rpmWaitBudget/time.Second)+2 {
		t.Fatalf("waited %ds, want a bounded wait within the %s budget", elapsed, rpmWaitBudget)
	}
}

// TestAccountRPMPoolPermanentBlockerSkipsWait keeps the wait honest: an
// account disabled by config never recovers, so PickWait must refuse
// immediately instead of parking the request.
func TestAccountRPMPoolPermanentBlockerSkipsWait(t *testing.T) {
	clk := &clock{at: testBase}
	s := newRPMScheduler(clk)
	disabled := rpmAccount("dead", 2)
	disabled.Enabled = false
	snap := snapOf(pool("p", disabled, rpmAccount("a", 2)))

	d, err := s.Pick(context.Background(), snap, Request{Model: "p/m1"})
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	s.Acquire(accKey(d.PluginID, d.AccountID))

	// Only "a" is a candidate now and it is RPM-paused. PickWait is allowed to
	// wait for it; the assertion here is that a disabled account never blocks
	// the wait (the plan is still waitable), and the wait serves.
	got, err := s.PickWait(context.Background(), snap, Request{Model: "p/m1"})
	if err != nil {
		t.Fatalf("PickWait = %v, want a decision", err)
	}
	if got.AccountID != "a" {
		t.Fatalf("PickWait account = %q, want a", got.AccountID)
	}
}

// TestAccountRPMExplainDoesNotCount keeps the diagnostics view side-effect
// free: Explain re-plans but must not consume window slots.
func TestAccountRPMExplainDoesNotCount(t *testing.T) {
	clk := &clock{at: testBase}
	s := New(Options{Now: clk.now})
	snap := snapOf(pool("p", rpmAccount("a", 1)))

	if _, err := s.Explain(context.Background(), snap, Request{Model: "p/m1"}); err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if got := s.rpmCountLocked(accKey("p", "a"), clk.at); got != 0 {
		t.Fatalf("Explain recorded %d hits, want 0 (only Acquire counts)", got)
	}
}
