package app

import (
	"context"
	"sort"
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

func TestSnapshotExposesPricingForCheapMode(t *testing.T) {
	app, _ := openAppWithStore(t)
	app.cfg = config.Default()
	app.cfg.Pricing = config.PricingConfig{Currency: "USD", Models: map[string]config.Price{
		"m-cheap": {Input: 1, Output: 2, Multiplier: 0.5},
	}}
	src := newFakeSchedulerSource()
	src.plugins = []model.PluginStatus{{Manifest: model.Manifest{ID: "p", APILevel: 2}, Enabled: true, Running: true}}
	src.accounts["p"] = []model.Account{{ID: "a1", Enabled: true, Credential: &model.Credential{Present: true}}}
	src.models["p"] = []model.ModelInfo{{ID: "m-cheap"}}

	snap, err := app.snapshotFrom(context.Background(), src)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if got := snap.Prices["m-cheap"]; got != 1.5 {
		t.Fatalf("price = %v, want (1+2)*0.5", got)
	}
}

func TestResolveRoutingGroupsIntentAndTokens(t *testing.T) {
	universe := []string{"pa/flash", "pb/pro"}
	groups := []config.RoutingGroupConfig{
		{ID: "router", Match: []string{"*"}, Intent: &config.RoutingIntentConfig{
			Enabled: true, Upstream: "jev", Labels: []string{"simple", "hard"},
			Route: map[string]string{"simple": "cheap", "hard": "router"}, Default: "cheap", ProbeChars: 1500,
		}},
		{ID: "cheap", Match: []string{"pa/*"}},
		{ID: "rules", Match: []string{"*"}, Rules: []config.RoutingRuleConfig{{
			Name: "large", Match: []string{"pb/*"},
			When: config.RoutingWhen{MinTokens: 2000, MaxTokens: 9000},
		}}},
	}
	out := resolveRoutingGroups(groups, universe)
	intent := out["router"].Intent
	if intent == nil || intent.Upstream != "jev" || intent.Route["hard"] != "router" ||
		intent.Default != "cheap" || intent.ProbeChars != 1500 || len(intent.Labels) != 2 {
		t.Fatalf("intent binding = %+v", intent)
	}
	when := out["rules"].Rules[0].When
	if when.MinTokens != 2000 || when.MaxTokens != 9000 {
		t.Fatalf("rule when = %+v", when)
	}
}

// ── task-44：自动分组（sameModel 归一 + follow）──

func TestAutoGroupsSameModelAcrossPlugins(t *testing.T) {
	universe := []string{"pa/deepseek-v4.1-flash", "pa/glm", "pb/deepseek-v4-1-flash", "pb/glm", "pc/other"}
	auto := autoGroupsOf(universe)
	binding, ok := auto["auto-deepseek-v4-1-flash"]
	if !ok {
		t.Fatalf("auto groups = %v, want auto-deepseek-v4-1-flash", idsOf(auto))
	}
	// 拼写归一（v4.1 / v4-1）后两插件同键成组；同插件只占一名（pa/glm 与
	// pb/glm 也同键，但 glm 只有两家且按 universe 序）。
	if len(binding.Items) != 2 ||
		binding.Items[0] != (scheduler.Source{PluginID: "pa", Model: "deepseek-v4.1-flash"}) ||
		binding.Items[1] != (scheduler.Source{PluginID: "pb", Model: "deepseek-v4-1-flash"}) {
		t.Fatalf("binding = %+v", binding)
	}
	if _, ok := auto["auto-other"]; ok {
		t.Fatalf("single-provider model must not form a group")
	}
	glm, ok := auto["auto-glm"]
	if !ok || len(glm.Items) != 2 {
		t.Fatalf("glm auto group = %+v ok=%v", glm, ok)
	}
}

func TestAutoGroupsDedupePluginSpellings(t *testing.T) {
	// 同一插件上报同一模型的三种拼写：只占一名。
	universe := []string{"pa/DeepSeek-V4.1-Flash", "pa/deepseek-v4.1-flash", "pa/deepseek_v4.1_flash"}
	auto := autoGroupsOf(universe)
	if len(auto) != 0 {
		t.Fatalf("one plugin alone must not form groups, got %v", idsOf(auto))
	}
}

func TestMergeAutoBindingsUserGroupWinsAndFollows(t *testing.T) {
	universe := []string{"pa/glm-5", "pb/glm-5", "pc/glm-5"}
	user := map[string]scheduler.Binding{
		// 用户改过的自动组：只留 pa，pc 是后来接入的 → 尾随加入。
		"auto-glm-5": {Logical: "auto-glm-5", Display: "GLM", Items: []scheduler.Source{
			{PluginID: "pa", Model: "glm-5"},
		}},
		// 用户自己的非 auto 组：原样保留。
		"mine": {Logical: "mine", Items: []scheduler.Source{{PluginID: "pb", Model: "glm-5"}}},
	}
	out := mergeAutoBindings(user, universe)
	// pb 与 pc 都是用户保存后才出现的新 provider，都尾随加入；用户成员
	// （pa）保持在前，用户的展示名不动。
	followed := out["auto-glm-5"]
	if len(followed.Items) != 3 ||
		followed.Items[0].PluginID != "pa" || followed.Items[1].PluginID != "pb" || followed.Items[2].PluginID != "pc" {
		t.Fatalf("follow = %+v, want [pa, pb, pc]", followed.Items)
	}
	if followed.Display != "GLM" {
		t.Fatalf("display = %q, want the user's", followed.Display)
	}
	if len(out["mine"].Items) != 1 {
		t.Fatalf("user group must be untouched: %+v", out["mine"])
	}
}

func TestMergeAutoBindingsPublishesFreshAutoGroups(t *testing.T) {
	// 用户没存 auto- 组：自动组直接发布。
	universe := []string{"pa/pro", "pb/pro"}
	out := mergeAutoBindings(map[string]scheduler.Binding{}, universe)
	if len(out["auto-pro"].Items) != 2 {
		t.Fatalf("auto-pro = %+v", out["auto-pro"])
	}
	// 用户禁用了自己的 auto- 组（未发布）：同名自动组按现状发布。
	disabled := []config.RoutingGroupConfig{{ID: "auto-pro", Match: []string{"pa/*"}, Disabled: true}}
	published := resolveRoutingGroups(disabled, universe)
	out = mergeAutoBindings(published, universe)
	if len(out["auto-pro"].Items) != 2 {
		t.Fatalf("disabled user group must free the name for the auto group: %+v", out["auto-pro"])
	}
}

func TestSameModelKeyFoldsSpelling(t *testing.T) {
	same := [][2]string{
		{"deepseek-v4.1-flash", "DeepSeek-V4.1-Flash"},
		{"deepseek-v4.1-flash", "deepseek_v4_1_flash"},
	}
	for _, pair := range same {
		if a, b := sameModelKey(pair[0]), sameModelKey(pair[1]); a != b {
			t.Fatalf("sameModelKey(%q)=%q != (%q)=%q", pair[0], a, pair[1], b)
		}
	}
	if sameModelKey("deepseek-v4.1-flash") == sameModelKey("deepseek-v4.1-pro") {
		t.Fatalf("different models folded together")
	}
}

func idsOf(bindings map[string]scheduler.Binding) []string {
	out := make([]string, 0, len(bindings))
	for id := range bindings {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// ── task-44：一键模板（strongest / cheapest / steady）──

func templateTestCatalog() []model.ModelCatalogEntry {
	return []model.ModelCatalogEntry{
		{PluginID: "pa", ModelID: "pro", Multiplier: 2, ContextLength: 200000},
		// flash-air 是 small 非 tiny；nano-tiny 是 tiny 且更便宜——用于钉住
		// 「tiny 惩罚先于价格」（magpie grouptemplate.go:187-193）。
		{PluginID: "pa", ModelID: "flash-air", Multiplier: 0.4, ContextLength: 128000},
		{PluginID: "pa", ModelID: "nano-tiny", Multiplier: 0.1},
		{PluginID: "pb", ModelID: "pro", Multiplier: 1.5, ContextLength: 200000},
	}
}

func TestBuildRoutingTemplatesPicksStrongAndCheap(t *testing.T) {
	out := buildRoutingTemplates(templateTestCatalog())
	if len(out) != 3 {
		t.Fatalf("templates = %d, want 3", len(out))
	}
	byKind := map[string]routingTemplateView{}
	for _, tpl := range out {
		byKind[tpl.Kind] = tpl
	}
	smart := byKind["smart-split"]
	if smart.Why != "" {
		t.Fatalf("smart why = %q", smart.Why)
	}
	if len(smart.Match) != 2 || smart.Match[0] != "pa/pro" || smart.Match[1] != "pa/flash-air" {
		t.Fatalf("smart match = %v, want [pa/pro, pa/flash-air]", smart.Match)
	}
	if len(smart.Rules) != 1 || smart.Rules[0].Name != "easy-turn" ||
		smart.Rules[0].Match[0] != "pa/flash-air" || smart.Rules[0].When.MaxChars != 2000 {
		t.Fatalf("smart rule = %+v", smart.Rules)
	}
	thrifty := byKind["cheap-first"]
	if len(thrifty.Match) != 2 || thrifty.Match[0] != "pa/flash-air" || thrifty.Match[1] != "pa/pro" {
		t.Fatalf("thrifty match = %v, want [pa/flash-air, pa/pro]", thrifty.Match)
	}
	if len(thrifty.Rules) != 1 || thrifty.Rules[0].When.MinChars != 8000 {
		t.Fatalf("thrifty rule = %+v", thrifty.Rules)
	}
	steady := byKind["never-stuck"]
	if len(steady.Match) != 2 || steady.Match[0] != "pa/pro" || steady.Match[1] != "pb/pro" {
		t.Fatalf("steady match = %v, want one strongest per plugin", steady.Match)
	}
}

func TestBuildRoutingTemplatesNeedCheap(t *testing.T) {
	catalog := []model.ModelCatalogEntry{
		{PluginID: "pa", ModelID: "pro", Multiplier: 2},
		{PluginID: "pb", ModelID: "pro-max", Multiplier: 1.9},
	}
	out := buildRoutingTemplates(catalog)
	byKind := map[string]routingTemplateView{}
	for _, tpl := range out {
		byKind[tpl.Kind] = tpl
	}
	if byKind["smart-split"].Why == "" || byKind["cheap-first"].Why == "" {
		t.Fatalf("no cheap model available: smart/cheap-first must carry a why")
	}
	// steady 不需要便宜模型：两家各取最强即可用。
	if byKind["never-stuck"].Why != "" || len(byKind["never-stuck"].Match) != 2 {
		t.Fatalf("steady = %+v", byKind["never-stuck"])
	}
}

func TestBuildRoutingTemplatesNeedModels(t *testing.T) {
	out := buildRoutingTemplates(nil)
	for _, tpl := range out {
		if tpl.Why == "" || len(tpl.Match) != 0 {
			t.Fatalf("empty catalog: %s must be unavailable, got %+v", tpl.Kind, tpl)
		}
	}
}

// task-72：custom 连接模型纳入成员源——universe 条目 custom:<conn>/<name>，
// 组 match 用与 /v1/models 一致的裸名（deepseek-flash）也能精确命中；成员
// Source 的 PluginID 即 CustomModelPluginID(conn)（直连路径不受调度池影响）。
func TestResolveGroupMatchCustomBareName(t *testing.T) {
	universe := []string{
		"trae/gpt-4o",
		"custom:conn1/deepseek-flash",
		"custom:conn1/deepseek-v4-pro",
		"custom:conn2/deepseek-flash",
	}
	groups := []config.RoutingGroupConfig{
		{ID: "deepseek", Match: []string{"deepseek-flash"}},
		{ID: "deepseek-glob", Match: []string{"deepseek-*"}},
		{ID: "namespaced", Match: []string{"custom:conn1/*"}},
		{ID: "everything", Match: []string{"*"}},
	}
	out := resolveRoutingGroups(groups, universe)

	exact := out["deepseek"]
	if len(exact.Items) != 2 {
		t.Fatalf("bare-name match = %+v, want both custom connections", exact.Items)
	}
	for _, item := range exact.Items {
		if item.Model != "deepseek-flash" {
			t.Fatalf("model = %q, want deepseek-flash", item.Model)
		}
		if item.PluginID != "custom:conn1" && item.PluginID != "custom:conn2" {
			t.Fatalf("plugin = %q, want a custom connection namespace", item.PluginID)
		}
	}
	if glob := out["deepseek-glob"]; len(glob.Items) != 3 {
		t.Fatalf("glob = %+v, want all three custom rows", glob.Items)
	}
	if ns := out["namespaced"]; len(ns.Items) != 2 {
		t.Fatalf("namespaced = %+v, want only conn1 rows", ns.Items)
	}
	if all := out["everything"]; len(all.Items) != 4 {
		t.Fatalf("everything = %+v, want the plugin row too", all.Items)
	}
}

// task-72：descriptions 校验——键必须 ⊆ labels、值非空。
func TestValidateRoutingIntentDescriptions(t *testing.T) {
	base := func(descriptions map[string]string) []config.RoutingGroupConfig {
		return []config.RoutingGroupConfig{{
			ID: "grp", Match: []string{"trae/*"},
			Intent: &config.RoutingIntentConfig{
				Enabled:      true,
				Labels:       []string{"simple", "hard"},
				Route:        map[string]string{"simple": "grp", "hard": "grp"},
				Descriptions: descriptions,
			},
		}}
	}
	// 合法：键 ⊆ labels、值非空。
	if err := validateRoutingGroups(base(map[string]string{
		"simple": "日常闲聊或简单问答", "hard": "复杂技术设计",
	})); err != nil {
		t.Fatalf("valid descriptions rejected: %v", err)
	}
	// 键不在 labels。
	if err := validateRoutingGroups(base(map[string]string{"unknown": "x"})); err == nil {
		t.Fatal("description key outside labels accepted")
	}
	// 空描述。
	if err := validateRoutingGroups(base(map[string]string{"simple": "   "})); err == nil {
		t.Fatal("empty description accepted")
	}
	// 描述可以只覆盖部分标签（缺省回落 label 名）。
	if err := validateRoutingGroups(base(map[string]string{"hard": "复杂任务"})); err != nil {
		t.Fatalf("partial descriptions rejected: %v", err)
	}
}
