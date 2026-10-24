package store

import (
	"errors"
	"reflect"
	"testing"

	"aiworkgate/internal/model"
)

func strPtr(value string) *string { return &value }
func levelsPtr(values ...string) *[]string {
	return &values
}

func TestModelOverrideRoundTrip(t *testing.T) {
	s := openTestStore(t)

	empty, err := s.GetModelOverrides("p1")
	if err != nil {
		t.Fatalf("GetModelOverrides on empty table: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("overrides on empty table = %v, want none", empty)
	}

	full := ModelOverride{
		DisplayName:   strPtr("  我的模型  "),
		Vendor:        strPtr(" 智谱 "),
		ContextLength: intPtr(200000),
		Vision:        boolPtr(true),
		Thinking:      levelsPtr("low", " high ", "low", ""),
		Multiplier:    floatPtr(0.5),
		Free:          boolPtr(false),
	}
	if err := s.PutModelOverride("p1", " glm-5 ", full); err != nil {
		t.Fatalf("PutModelOverride: %v", err)
	}

	got, err := s.GetModelOverrides("p1")
	if err != nil {
		t.Fatalf("GetModelOverrides: %v", err)
	}
	entry, ok := got["glm-5"]
	if !ok {
		t.Fatalf("overrides = %v, want glm-5", got)
	}
	if entry.DisplayName == nil || *entry.DisplayName != "我的模型" {
		t.Fatalf("display_name = %v, want trimmed 我的模型", entry.DisplayName)
	}
	if entry.Vendor == nil || *entry.Vendor != "智谱" {
		t.Fatalf("vendor = %v, want trimmed 智谱", entry.Vendor)
	}
	if entry.ContextLength == nil || *entry.ContextLength != 200000 {
		t.Fatalf("context_length = %v, want 200000", entry.ContextLength)
	}
	if entry.Vision == nil || !*entry.Vision {
		t.Fatalf("vision = %v, want true", entry.Vision)
	}
	if entry.Free == nil || *entry.Free {
		t.Fatalf("free = %v, want an explicit false", entry.Free)
	}
	if entry.Multiplier == nil || *entry.Multiplier != 0.5 {
		t.Fatalf("multiplier = %v, want 0.5", entry.Multiplier)
	}
	if entry.Thinking == nil || !reflect.DeepEqual(*entry.Thinking, []string{"low", "high"}) {
		t.Fatalf("thinking = %v, want [low high]", entry.Thinking)
	}
	if entry.UpdatedAt == 0 {
		t.Fatal("updated_at was not stamped")
	}
	wantFields := []string{"display_name", "vendor", "context_length", "vision", "thinking", "multiplier", "free"}
	if fields := entry.Fields(); !reflect.DeepEqual(fields, wantFields) {
		t.Fatalf("Fields() = %v, want %v", fields, wantFields)
	}

	// A put replaces the whole row: this is the store contract, the admin API
	// does read-modify-write for partial updates.
	if err := s.PutModelOverride("p1", "glm-5", ModelOverride{Vendor: strPtr("智谱AI")}); err != nil {
		t.Fatalf("PutModelOverride (partial row): %v", err)
	}
	got, _ = s.GetModelOverrides("p1")
	entry = got["glm-5"]
	if entry.DisplayName != nil || entry.ContextLength != nil || entry.Thinking != nil {
		t.Fatalf("full-row write kept stale fields: %+v", entry)
	}
	if entry.Vendor == nil || *entry.Vendor != "智谱AI" {
		t.Fatalf("vendor = %v, want 智谱AI", entry.Vendor)
	}
	if fields := entry.Fields(); !reflect.DeepEqual(fields, []string{"vendor"}) {
		t.Fatalf("Fields() = %v, want [vendor]", fields)
	}

	// Other plugins and models stay separate.
	if err := s.PutModelOverride("p2", "glm-5", ModelOverride{Free: boolPtr(true)}); err != nil {
		t.Fatalf("PutModelOverride (other plugin): %v", err)
	}
	if other, _ := s.GetModelOverrides("p2"); len(other) != 1 || other["glm-5"].Free == nil {
		t.Fatalf("p2 overrides = %v", other)
	}
	if mine, _ := s.GetModelOverrides("p1"); len(mine) != 1 {
		t.Fatalf("p1 overrides = %v, want a single row", mine)
	}
}

func TestModelOverrideDeleteAndClear(t *testing.T) {
	s := openTestStore(t)
	if err := s.PutModelOverride("p1", "m1", ModelOverride{Vendor: strPtr("厂商")}); err != nil {
		t.Fatalf("PutModelOverride: %v", err)
	}
	if err := s.DeleteModelOverride("p1", "m1"); err != nil {
		t.Fatalf("DeleteModelOverride: %v", err)
	}
	if got, _ := s.GetModelOverrides("p1"); len(got) != 0 {
		t.Fatalf("overrides after delete = %v, want none", got)
	}
	// Deleting twice is not an error (the admin API stays idempotent).
	if err := s.DeleteModelOverride("p1", "m1"); err != nil {
		t.Fatalf("second DeleteModelOverride: %v", err)
	}

	// An override with no field stores nothing, so the row disappears.
	if err := s.PutModelOverride("p1", "m1", ModelOverride{DisplayName: strPtr("   ")}); err != nil {
		t.Fatalf("PutModelOverride (blank): %v", err)
	}
	if got, _ := s.GetModelOverrides("p1"); len(got) != 0 {
		t.Fatalf("blank override stored a row: %v", got)
	}
}

func TestModelOverrideValidation(t *testing.T) {
	s := openTestStore(t)
	cases := []struct {
		name   string
		plugin string
		model  string
		entry  ModelOverride
	}{
		{"empty plugin", "", "m", ModelOverride{Vendor: strPtr("v")}},
		{"empty model", "p", "", ModelOverride{Vendor: strPtr("v")}},
		{"zero context", "p", "m", ModelOverride{ContextLength: intPtr(0)}},
		{"negative context", "p", "m", ModelOverride{ContextLength: intPtr(-1)}},
		{"negative multiplier", "p", "m", ModelOverride{Multiplier: floatPtr(-0.1)}},
		{"long display name", "p", "m", ModelOverride{DisplayName: strPtr(longString(maxOverrideNameRunes + 1))}},
		{"long vendor", "p", "m", ModelOverride{Vendor: strPtr(longString(maxOverrideVendorRunes + 1))}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := s.PutModelOverride(tc.plugin, tc.model, tc.entry)
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("PutModelOverride error = %v, want ErrInvalidArgument", err)
			}
		})
	}

	// Boundary values are accepted.
	if err := s.PutModelOverride("p", "m", ModelOverride{
		ContextLength: intPtr(1), Multiplier: floatPtr(0),
		DisplayName: strPtr(longString(maxOverrideNameRunes)),
	}); err != nil {
		t.Fatalf("boundary override rejected: %v", err)
	}
	if _, err := s.GetModelOverrides(""); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("GetModelOverrides(empty) error = %v, want ErrInvalidArgument", err)
	}
	if err := s.DeleteModelOverride("", "m"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("DeleteModelOverride(empty) error = %v, want ErrInvalidArgument", err)
	}
}

func longString(n int) string {
	out := make([]rune, n)
	for i := range out {
		out[i] = '模'
	}
	return string(out)
}

func TestModelOverrideMergePriority(t *testing.T) {
	// The plugin reports a complete set of values; the override only pins two
	// of them. Everything else must survive untouched (L1 > L2 > L3 chain).
	reported := model.ModelInfo{
		ID:            "glm-5",
		Name:          "插件名",
		Vendor:        "插件厂商",
		Multiplier:    1,
		Thinking:      []string{"low"},
		ContextLength: 128000,
		Vision:        false,
		Free:          false,
		Tools:         true,
	}
	override := ModelOverride{
		DisplayName:   strPtr("人工名"),
		ContextLength: intPtr(200000),
	}
	merged := override.ApplyInfo(reported)
	if merged.Name != "人工名" || merged.ContextLength != 200000 {
		t.Fatalf("overridden fields = %+v", merged)
	}
	if merged.Vendor != "插件厂商" || merged.Multiplier != 1 || merged.Free {
		t.Fatalf("reported fields were lost: %+v", merged)
	}
	if !reflect.DeepEqual(merged.Thinking, []string{"low"}) {
		t.Fatalf("thinking = %v, want the reported [low]", merged.Thinking)
	}
	if !merged.Tools || merged.Vision || merged.ID != "glm-5" {
		t.Fatalf("unrelated fields changed: %+v", merged)
	}
	// ApplyInfo must not alias the override slice.
	levels := []string{"low", "high"}
	override.Thinking = &levels
	merged = override.ApplyInfo(reported)
	(*override.Thinking)[0] = "mutated"
	if merged.Thinking[0] != "low" {
		t.Fatalf("ApplyInfo aliased the override slice: %v", merged.Thinking)
	}

	entry := model.ModelCatalogEntry{
		PluginID: "p1", ModelID: "glm-5", DisplayName: "插件名", Vendor: "插件厂商",
		ContextLength: 128000, Vision: false, Thinking: []string{"low"}, Multiplier: 2, Free: false,
	}
	mergedEntry := override.ApplyEntry(entry)
	if mergedEntry.DisplayName != "人工名" || mergedEntry.ContextLength != 200000 {
		t.Fatalf("entry merge = %+v", mergedEntry)
	}
	if mergedEntry.Vendor != "插件厂商" || mergedEntry.Multiplier != 2 {
		t.Fatalf("entry reported fields were lost: %+v", mergedEntry)
	}
}

func TestModelOverridesSurviveCatalogSync(t *testing.T) {
	s := openTestStore(t)
	if err := s.ReplaceModelCatalog("p1", []model.ModelCatalogEntry{
		{ModelID: "glm-5", DisplayName: "旧名", ContextLength: 1000},
	}); err != nil {
		t.Fatalf("ReplaceModelCatalog: %v", err)
	}
	if err := s.PutModelOverride("p1", "glm-5", ModelOverride{DisplayName: strPtr("人工名")}); err != nil {
		t.Fatalf("PutModelOverride: %v", err)
	}

	// A sync replaces the reported values and must leave the override alone.
	if err := s.ReplaceModelCatalog("p1", []model.ModelCatalogEntry{
		{ModelID: "glm-5", DisplayName: "新名", ContextLength: 2000},
	}); err != nil {
		t.Fatalf("ReplaceModelCatalog (resync): %v", err)
	}
	overrides, err := s.GetModelOverrides("p1")
	if err != nil {
		t.Fatalf("GetModelOverrides: %v", err)
	}
	override, ok := overrides["glm-5"]
	if !ok || override.DisplayName == nil || *override.DisplayName != "人工名" {
		t.Fatalf("override after sync = %+v, want 人工名", overrides)
	}
	catalog, err := s.ListModelCatalog("p1")
	if err != nil || len(catalog) != 1 {
		t.Fatalf("ListModelCatalog = %v, %v", catalog, err)
	}
	if catalog[0].DisplayName != "新名" {
		t.Fatalf("catalog display_name = %q, want the freshly synced 新名", catalog[0].DisplayName)
	}
	merged := override.ApplyEntry(catalog[0])
	if merged.DisplayName != "人工名" || merged.ContextLength != 2000 {
		t.Fatalf("merged entry = %+v, want override name and synced context", merged)
	}

	// Clearing the override falls back to the synced value.
	if err := s.DeleteModelOverride("p1", "glm-5"); err != nil {
		t.Fatalf("DeleteModelOverride: %v", err)
	}
	overrides, _ = s.GetModelOverrides("p1")
	if _, ok := overrides["glm-5"]; ok {
		t.Fatalf("override still present after delete: %v", overrides)
	}
	fallback := (ModelOverride{}).ApplyEntry(catalog[0])
	if fallback.DisplayName != "新名" {
		t.Fatalf("fallback display_name = %q, want 新名", fallback.DisplayName)
	}
}
