package app

import (
	"context"
	"testing"

	"aiworkgate/internal/config"
	"aiworkgate/internal/model"
	"aiworkgate/internal/scheduler"
)

func TestResolveRoutingGroupsPatterns(t *testing.T) {
	universe := []string{"trae/gpt-4o", "trae/glm-5", "workbuddy/x", "other/y"}
	groups := []config.RoutingGroupConfig{
		{ID: "trae-all", Match: []string{"trae/*"}},
		{ID: "vendor", Match: []string{"re:^work"}},
		{ID: "exact", Match: []string{"other/y", "trae/glm-5", "other/y"}},
		{ID: "everything", Match: []string{"*"}},
		{ID: "disabled", Match: []string{"*"}, Disabled: true},
		{ID: "missing", Match: []string{"nope/*"}},
		{ID: "ruled", Match: []string{"trae/*"}, Rules: []config.RoutingRuleConfig{{
			Name: "big", Match: []string{"workbuddy/*"}, When: config.RoutingWhen{MinChars: 500, Image: true},
		}}},
		{ID: "weighted", Match: []string{"trae/*"}, Weights: map[string]int{"trae/glm-5": 3}},
	}
	out := resolveRoutingGroups(groups, universe)

	trae := out["trae-all"]
	if len(trae.Items) != 2 || trae.Items[0].PluginID != "trae" || trae.Items[0].Model != "glm-5" {
		t.Fatalf("trae-all = %+v", trae)
	}
	if vendor := out["vendor"]; len(vendor.Items) != 1 || vendor.Items[0].PluginID != "workbuddy" {
		t.Fatalf("vendor = %+v", vendor)
	}
	if exact := out["exact"]; len(exact.Items) != 2 {
		t.Fatalf("exact dedupe = %+v", exact)
	}
	if all := out["everything"]; len(all.Items) != 4 {
		t.Fatalf("everything = %+v", all)
	}
	if _, ok := out["disabled"]; ok {
		t.Fatalf("disabled group must not be published")
	}
	if _, ok := out["missing"]; ok {
		t.Fatalf("group with no members must not be published")
	}
	ruled := out["ruled"]
	if out["weighted"].Weights["trae/glm-5"] != 3 {
		t.Fatalf("weights = %v", out["weighted"].Weights)
	}
	if len(ruled.Rules) != 1 || ruled.Rules[0].Name != "big" ||
		ruled.Rules[0].When.MinChars != 500 || !ruled.Rules[0].When.Image ||
		len(ruled.Rules[0].Items) != 1 || ruled.Rules[0].Items[0].PluginID != "workbuddy" {
		t.Fatalf("ruled = %+v", ruled)
	}
}

func TestGlobMatchAcrossSlash(t *testing.T) {
	cases := map[string]bool{
		"trae/*":      true,
		"*/gpt-4o":    true,
		"trae/gpt-?o": true,
		"trae/gpt-4o": true,
		"trae/gpt-4x": false,
		"workbuddy/*": false,
	}
	for pattern, want := range cases {
		if got := routingPatternMatch(pattern, "trae/gpt-4o"); got != want {
			t.Fatalf("routingPatternMatch(%q) = %v, want %v", pattern, got, want)
		}
	}
}

func TestSnapshotPublishesDynamicGroups(t *testing.T) {
	app, _ := openAppWithStore(t)
	app.cfg = config.Default()
	app.cfg.RoutingGroups = []config.RoutingGroupConfig{{ID: "mock-all", Match: []string{"mock/*"}}}

	src := newFakeSchedulerSource()
	src.plugins = []model.PluginStatus{{
		Manifest: model.Manifest{ID: "mock", Name: "Mock", APILevel: 2},
		Enabled:  true, Running: true,
	}}
	src.accounts["mock"] = []model.Account{{ID: "acc_1", Enabled: true, Credential: &model.Credential{Present: true}}}
	src.models["mock"] = []model.ModelInfo{{ID: "glm"}}

	snap, err := app.snapshotFrom(context.Background(), src)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	binding, ok := snap.Bindings["mock-all"]
	if !ok || len(binding.Items) != 1 || binding.Items[0] != (scheduler.Source{PluginID: "mock", Model: "glm"}) {
		t.Fatalf("binding = %+v ok=%v", binding, ok)
	}

	// A vendor adding a model changes the group on the next snapshot. The
	// snapshot reads model lists through a TTL cache, so drop it like a catalog
	// refresh would.
	src.models["mock"] = []model.ModelInfo{{ID: "glm"}, {ID: "glm-mini"}}
	app.snapMu.Lock()
	app.snapModels = nil
	app.snapMu.Unlock()
	snap, err = app.snapshotFrom(context.Background(), src)
	if err != nil {
		t.Fatalf("snapshot 2: %v", err)
	}
	if got := len(snap.Bindings["mock-all"].Items); got != 2 {
		t.Fatalf("group members after refresh = %d, want 2", got)
	}
}
