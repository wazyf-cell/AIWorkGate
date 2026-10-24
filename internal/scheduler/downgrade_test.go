package scheduler

import (
	"testing"

	"aiworkgate/internal/model"
)

func downgradeSnapshot() (*Snapshot, Decision) {
	p := pool("p", account("a"))
	p.Models = map[string]model.ModelInfo{
		"pro-model":   {ID: "pro-model", Multiplier: 2, ContextLength: 1_000_000},
		"flash-model": {ID: "flash-model", Multiplier: 0.5, ContextLength: 1_000_000},
		"free-flash":  {ID: "free-flash", Multiplier: 1, Free: true},
	}
	p.BackgroundDowngrade = true
	p.LongContextDowngrade = true
	p.DefaultReasoning = "medium"
	snap := &Snapshot{Pools: map[string]*Pool{"p": p}}
	return snap, Decision{PluginID: "p", AccountID: "a", Model: "pro-model", LogicalModel: "pro"}
}

func TestApplyDowngradesBackground(t *testing.T) {
	snap, decision := downgradeSnapshot()
	got, reason := ApplyDowngrades(snap, decision, BodyMeta{MaxTokens: 100, PromptChars: 400})
	if reason != DowngradeBackground || got.Model != "free-flash" {
		t.Fatalf("got %+v reason %q", got, reason)
	}
	if got.LogicalModel != "pro" || got.AccountID != "a" {
		t.Fatalf("identity fields changed: %+v", got)
	}
	if len(got.Reasons) == 0 {
		t.Fatal("no downgrade reason recorded")
	}

	// max_tokens above the threshold or a long prompt disables background.
	for _, meta := range []BodyMeta{
		{MaxTokens: 129, PromptChars: 400},
		{MaxTokens: 100, PromptChars: 513},
		{MaxTokens: 0, PromptChars: 400},
	} {
		if _, reason := ApplyDowngrades(snap, decision, meta); reason != "" {
			t.Fatalf("meta %+v downgraded unexpectedly (%s)", meta, reason)
		}
	}
}

func TestApplyDowngradesLongContext(t *testing.T) {
	snap, decision := downgradeSnapshot()
	got, reason := ApplyDowngrades(snap, decision, BodyMeta{MaxTokens: 4096, PromptChars: 500_000})
	if reason != DowngradeLongCtx || got.Model != "free-flash" {
		t.Fatalf("got %+v reason %q", got, reason)
	}
	// Explicit estimated tokens win over chars.
	if _, reason := ApplyDowngrades(snap, decision, BodyMeta{EstimatedTokens: 120_000}); reason != DowngradeLongCtx {
		t.Fatalf("estimated tokens reason = %q", reason)
	}
	if _, reason := ApplyDowngrades(snap, decision, BodyMeta{EstimatedTokens: 99_999, PromptChars: 10}); reason != "" {
		t.Fatalf("small request downgraded (%s)", reason)
	}
}

func TestApplyDowngradesDisabledAndNoFlash(t *testing.T) {
	snap, decision := downgradeSnapshot()
	p := snap.Pools["p"]
	p.BackgroundDowngrade = false
	p.LongContextDowngrade = false
	if _, reason := ApplyDowngrades(snap, decision, BodyMeta{MaxTokens: 10, PromptChars: 10, EstimatedTokens: 200_000}); reason != "" {
		t.Fatalf("disabled pool downgraded (%s)", reason)
	}

	p.BackgroundDowngrade = true
	delete(p.Models, "flash-model")
	delete(p.Models, "free-flash")
	if _, reason := ApplyDowngrades(snap, decision, BodyMeta{MaxTokens: 10, PromptChars: 10}); reason != "" {
		t.Fatalf("pool without a cheaper tier downgraded (%s)", reason)
	}
}

func TestDefaultReasoning(t *testing.T) {
	snap, decision := downgradeSnapshot()
	reasoning, ok := DefaultReasoning(snap, decision, false)
	if !ok || reasoning != "medium" {
		t.Fatalf("reasoning = %q, %v", reasoning, ok)
	}
	if _, ok := DefaultReasoning(snap, decision, true); ok {
		t.Fatal("an explicit reasoning_effort must not be overridden")
	}
	snap.Pools["p"].DefaultReasoning = ""
	if _, ok := DefaultReasoning(snap, decision, false); ok {
		t.Fatal("empty default injected")
	}
}
