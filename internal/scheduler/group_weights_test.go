package scheduler

import (
	"context"
	"testing"
	"time"
)

func TestGroupWeightsDistribution(t *testing.T) {
	now := testBase
	snap := snapOf(pool("pa", account("a")), pool("pb", account("b")))
	snap.Bindings["grp"] = Binding{
		Logical: "grp",
		Items:   []Source{{PluginID: "pa", Model: "m1"}, {PluginID: "pb", Model: "m1"}},
		Weights: map[string]int{"pa/m1": 1, "pb/m1": 3},
	}
	s := New(Options{Now: func() time.Time { return time.Unix(now, 0) }})
	counts := map[string]int{}
	order := make([]string, 0, 4)
	for i := 0; i < 4; i++ {
		decision, err := s.Pick(context.Background(), snap, Request{Model: "grp"})
		if err != nil {
			t.Fatalf("Pick %d: %v", i, err)
		}
		counts[decision.PluginID]++
		order = append(order, decision.PluginID)
	}
	if counts["pa"] != 1 || counts["pb"] != 3 {
		t.Fatalf("counts = %v (order %v), want pa=1 pb=3", counts, order)
	}
	if order[0] != "pb" {
		t.Fatalf("first pick = %v, want pb (highest weight first)", order)
	}
}
