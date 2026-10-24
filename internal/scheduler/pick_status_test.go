package scheduler

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestPickSkipsUnservableStatusWhenUsableSiblingExists is the scheduling half of
// the 2026-10-23 review finding. SelectableCount (health) stopped counting
// accounts whose plugin status is expired/invalid; Pick has to agree whenever a
// usable sibling exists, otherwise health says "7 selectable" while a request
// still lands on the dead one and burns a retry.
func TestPickSkipsUnservableStatusWhenUsableSiblingExists(t *testing.T) {
	expired := account("expired-pick")
	expired.Status = "expired"
	usable := account("ok-pick")

	s := New(Options{Now: func() time.Time { return time.Unix(testBase, 0) }})
	snap := snapOf(pool("p", expired, usable))

	decision, err := s.Pick(context.Background(), snap, Request{Model: "p/m1"})
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if decision.AccountID != "ok-pick" {
		t.Fatalf("picked %q, want ok-pick (the expired account must yield)", decision.AccountID)
	}

	explain, err := s.Explain(context.Background(), snap, Request{Model: "p/m1"})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	var reason string
	for _, c := range explain.Candidates {
		if c.AccountID == "expired-pick" {
			reason = c.Reason
		}
	}
	if !strings.Contains(reason, "account expired (plugin status)") {
		t.Fatalf("explain reason for the expired account = %q, want the plugin-status reason", reason)
	}
}

// TestPickFailsOpenWhenEveryAccountIsUnservable pins the deliberate divergence
// the review asked about: health reports zero known-good accounts, but a pool
// where every account carries an unservable status still gets one attempt. That
// mirrors creditIneligibleReason's rule — a stale plugin status must not turn a
// whole channel into a hard 503.
func TestPickFailsOpenWhenEveryAccountIsUnservable(t *testing.T) {
	only := account("only-expired")
	only.Status = "expired"

	s := New(Options{Now: func() time.Time { return time.Unix(testBase, 0) }})
	snap := snapOf(pool("p", only))

	if selectable, total := s.SelectableCount(snap); selectable != 0 || total != 1 {
		t.Fatalf("SelectableCount = %d/%d, want 0/1 (health reports no usable account)", selectable, total)
	}

	decision, err := s.Pick(context.Background(), snap, Request{Model: "p/m1"})
	if err != nil {
		t.Fatalf("Pick: %v (must fail open, matching the credit filter)", err)
	}
	if decision.AccountID != "only-expired" {
		t.Fatalf("picked %q, want only-expired (fail open)", decision.AccountID)
	}
}
