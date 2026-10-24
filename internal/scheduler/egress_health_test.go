package scheduler

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestEgressSoftFilterSkipsDeadAccount: a dead exit is a soft filter, so a
// healthy sibling serves the request and the skipped one carries a readable
// reason.
func TestEgressSoftFilterSkipsDeadAccount(t *testing.T) {
	now := testBase
	s := New(Options{Now: func() time.Time { return time.Unix(now, 0) }})
	dead := account("dead")
	dead.Egress = EgressEvidence{Failed: true, Label: "公司代理", Error: "dial tcp: i/o timeout"}

	decision, err := s.Pick(context.Background(), snapOf(pool("p", dead, account("alive"))), Request{Model: "p/m1"})
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if decision.AccountID != "alive" {
		t.Fatalf("account = %q, want alive", decision.AccountID)
	}

	explained, err := s.Explain(context.Background(), snapOf(pool("p", dead, account("alive"))), Request{Model: "p/m1"})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	reason := ""
	for _, cand := range explained.Candidates {
		if cand.AccountID == "dead" {
			reason = cand.Reason
		}
	}
	if !strings.Contains(reason, "出口不可用") || !strings.Contains(reason, "公司代理") {
		t.Fatalf("dead account reason = %q, want the egress reason", reason)
	}
}

// TestEgressFailOpenWhenEveryAccountIsDead: evidence can be stale and a proxy
// can recover, so an all-dead pool still gets an attempt instead of a 503.
func TestEgressFailOpenWhenEveryAccountIsDead(t *testing.T) {
	now := testBase
	s := New(Options{Now: func() time.Time { return time.Unix(now, 0) }})
	only := account("only")
	only.Egress = EgressEvidence{Failed: true, Label: "px"}
	decision, err := s.Pick(context.Background(), snapOf(pool("p", only)), Request{Model: "p/m1"})
	if err != nil {
		t.Fatalf("Pick must fail open: %v", err)
	}
	if decision.AccountID != "only" {
		t.Fatalf("account = %q", decision.AccountID)
	}
}

// TestHealthCountsReportsEgressFailures: health stops counting a dead-exit
// account as selectable but reports why, so /healthz can tell "no account" from
// "all exits are down".
func TestHealthCountsReportsEgressFailures(t *testing.T) {
	now := testBase
	s := New(Options{Now: func() time.Time { return time.Unix(now, 0) }})
	dead := account("dead")
	dead.Egress = EgressEvidence{Failed: true, Label: "px"}
	alive := account("alive")
	selectable, total, egressFailed := s.HealthCounts(snapOf(pool("p", dead, alive)))
	if selectable != 1 || total != 2 || egressFailed != 1 {
		t.Fatalf("counts = %d/%d failed=%d, want 1/2 failed=1", selectable, total, egressFailed)
	}

	// SelectableCount stays the two-value form used elsewhere.
	if selectable, total := s.SelectableCount(snapOf(pool("p", dead, alive))); selectable != 1 || total != 2 {
		t.Fatalf("SelectableCount = %d/%d", selectable, total)
	}
}
