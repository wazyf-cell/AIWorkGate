package scheduler

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestBindingRulesSelectSources(t *testing.T) {
	now := testBase
	snap := snapOf(pool("pa", account("a")), pool("pb", account("b")))
	snap.Bindings["grp"] = Binding{
		Logical: "grp",
		Items:   []Source{{PluginID: "pa", Model: "m1"}},
		Rules: []BindingRule{{
			Name:  "big",
			When:  BindingWhen{MinChars: 100},
			Items: []Source{{PluginID: "pb", Model: "m1"}},
		}},
	}
	s := New(Options{Now: func() time.Time { return time.Unix(now, 0) }})

	small, err := s.Pick(context.Background(), snap, Request{Model: "grp", PromptChars: 10})
	if err != nil || small.PluginID != "pa" {
		t.Fatalf("small = %+v err=%v, want pa", small, err)
	}
	big, err := s.Pick(context.Background(), snap, Request{Model: "grp", PromptChars: 200})
	if err != nil || big.PluginID != "pb" {
		t.Fatalf("big = %+v err=%v, want pb", big, err)
	}

	explain, err := s.Explain(context.Background(), snap, Request{Model: "grp", PromptChars: 200})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	joined := strings.Join(explain.Reasons, "; ")
	if !strings.Contains(joined, "binding rule: big") {
		t.Fatalf("explain reasons = %v, want the winning rule", explain.Reasons)
	}
}

func TestBindingRulesImageAndAgent(t *testing.T) {
	now := testBase
	snap := snapOf(pool("pa", account("a")), pool("pb", account("b")))
	snap.Bindings["grp"] = Binding{
		Logical: "grp",
		Items:   []Source{{PluginID: "pa", Model: "m1"}},
		Rules: []BindingRule{{
			Name:  "vision",
			When:  BindingWhen{Image: true, Agent: "claude"},
			Items: []Source{{PluginID: "pb", Model: "m1"}},
		}},
	}
	s := New(Options{Now: func() time.Time { return time.Unix(now, 0) }})
	if d, _ := s.Pick(context.Background(), snap, Request{Model: "grp", HasImage: true, Agent: "Claude-Code/1.0"}); d.PluginID != "pb" {
		t.Fatalf("image+agent = %s, want pb", d.PluginID)
	}
	if d, _ := s.Pick(context.Background(), snap, Request{Model: "grp", Agent: "other"}); d.PluginID != "pa" {
		t.Fatalf("no image = %s, want pa", d.PluginID)
	}
}

func TestBindingRulesTokenBounds(t *testing.T) {
	snap := snapOf(pool("pa", account("a")), pool("pb", account("b")))
	snap.Bindings["grp"] = Binding{
		Logical: "grp",
		Items:   []Source{{PluginID: "pa", Model: "m1"}},
		Rules: []BindingRule{{
			Name:  "large-context",
			When:  BindingWhen{MinTokens: 1000, MaxTokens: 5000},
			Items: []Source{{PluginID: "pb", Model: "m1"}},
		}},
	}
	s := New(Options{Now: func() time.Time { return time.Unix(testBase, 0) }})

	for _, tc := range []struct {
		tokens int
		want   string
	}{
		{999, "pa"},
		{1000, "pb"},
		{5000, "pb"},
		{5001, "pa"},
	} {
		decision, err := s.Pick(context.Background(), snap, Request{Model: "grp", EstimatedTokens: tc.tokens})
		if err != nil {
			t.Fatalf("tokens=%d: %v", tc.tokens, err)
		}
		if decision.PluginID != tc.want {
			t.Fatalf("tokens=%d chose %s, want %s", tc.tokens, decision.PluginID, tc.want)
		}
	}
}
