package store

import (
	"reflect"
	"testing"

	"aiworkgate/internal/model"
)

func TestModelPrefsRoundTripAndSurvivesReopen(t *testing.T) {
	dir := testTempDir(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if got, err := s.GetModelPrefs("p1"); err != nil {
		t.Fatalf("GetModelPrefs on empty: %v", err)
	} else if len(got) != 0 {
		t.Fatalf("prefs on empty table = %v, want none", got)
	}

	full := ModelPrefs{
		DisplayName: strPtr("  我的模型  "),
		VendorLabel: strPtr(" 智谱 "),
		Efforts:     levelsPtr("low", " high ", "low", ""),
	}
	if err := s.PutModelPrefs(" p1 ", " glm-5 ", full); err != nil {
		t.Fatalf("PutModelPrefs: %v", err)
	}

	got, err := s.GetModelPrefs("p1")
	if err != nil {
		t.Fatalf("GetModelPrefs: %v", err)
	}
	entry, ok := got["glm-5"] // 键已规范化
	if !ok {
		t.Fatalf("glm-5 missing from %v", got)
	}
	if entry.DisplayName == nil || *entry.DisplayName != "我的模型" {
		t.Fatalf("display_name = %v, want 我的模型 (trimmed)", entry.DisplayName)
	}
	if entry.VendorLabel == nil || *entry.VendorLabel != "智谱" {
		t.Fatalf("vendor_label = %v, want 智谱 (trimmed)", entry.VendorLabel)
	}
	// efforts 规范化：去空白、去重、保持顺序。
	if entry.Efforts == nil || !reflect.DeepEqual(*entry.Efforts, []string{"low", "high"}) {
		t.Fatalf("efforts = %v, want [low high]", entry.Efforts)
	}
	if entry.UpdatedAt == 0 {
		t.Fatal("updated_at not set")
	}

	// 空 prefs = 删除行（回落默认），保持幂等。
	if err := s.PutModelPrefs("p1", "glm-5", ModelPrefs{}); err != nil {
		t.Fatalf("PutModelPrefs empty: %v", err)
	}
	got, _ = s.GetModelPrefs("p1")
	if len(got) != 0 {
		t.Fatalf("empty prefs should delete the row, got %v", got)
	}

	// 重启持久：关闭再打开，prefs（这次写一条非空的）仍在。
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if err := s2.PutModelPrefs("p1", "glm-5", ModelPrefs{DisplayName: strPtr("GLM-5 彩蛋")}); err != nil {
		t.Fatalf("PutModelPrefs before reopen persisted: %v", err)
	}
	got, err = s2.GetModelPrefs("p1")
	if err != nil {
		t.Fatalf("GetModelPrefs after reopen: %v", err)
	}
	if got["glm-5"].DisplayName == nil || *got["glm-5"].DisplayName != "GLM-5 彩蛋" {
		t.Fatalf("prefs lost after reopen: %v", got)
	}
}

func TestModelPrefsRenderName(t *testing.T) {
	// 自定义名 + 供应商 → 「自定义名 · 供应商」（D-27③）
	p := ModelPrefs{DisplayName: strPtr("我的模型"), VendorLabel: strPtr("智谱")}
	if got := p.RenderName("glm-5", "GLM"); got != "我的模型 · 智谱" {
		t.Fatalf("RenderName = %q, want 我的模型 · 智谱", got)
	}
	// 未设置 → 用插件报告值
	if got := (ModelPrefs{}).RenderName("glm-5", "GLM"); got != "glm-5 · GLM" {
		t.Fatalf("default RenderName = %q, want glm-5 · GLM", got)
	}
}

func TestModelPrefsCutEfforts(t *testing.T) {
	reported := []string{"low", "medium", "high"}
	// nil = 全保留
	if got := (ModelPrefs{}).CutEfforts(reported); !reflect.DeepEqual(got, reported) {
		t.Fatalf("no cut = %v, want %v", got, reported)
	}
	// 裁剪：只留 low+high，保持报告顺序
	cut := ModelPrefs{Efforts: levelsPtr("high", "low")}
	if got := cut.CutEfforts(reported); !reflect.DeepEqual(got, []string{"low", "high"}) {
		t.Fatalf("cut = %v, want [low high]", got)
	}
	// 空列表 = 不暴露任何档位
	none := ModelPrefs{Efforts: levelsPtr()}
	if got := none.CutEfforts(reported); len(got) != 0 {
		t.Fatalf("empty cut = %v, want none", got)
	}
}

func TestModelPrefsApplyInfoAndEntry(t *testing.T) {
	info := model.ModelInfo{
		ID: "glm-5", Name: "GLM-5", Vendor: "GLM",
		Thinking: []string{"low", "medium", "high"},
	}
	p := ModelPrefs{DisplayName: strPtr("我的模型"), VendorLabel: strPtr("智谱"), Efforts: levelsPtr("high")}
	out := p.ApplyInfo(info)
	if out.Name != "我的模型" || out.Vendor != "智谱" {
		t.Fatalf("ApplyInfo rename = %+v", out)
	}
	if !reflect.DeepEqual(out.Thinking, []string{"high"}) {
		t.Fatalf("ApplyInfo efforts = %v, want [high]", out.Thinking)
	}

	entry := model.ModelCatalogEntry{
		PluginID: "p1", ModelID: "glm-5", DisplayName: "GLM-5", Vendor: "GLM",
		Thinking: []string{"low", "medium", "high"},
	}
	outEntry := p.ApplyEntry(entry)
	if outEntry.DisplayName != "我的模型" || outEntry.Vendor != "智谱" {
		t.Fatalf("ApplyEntry rename = %+v", outEntry)
	}
	if !reflect.DeepEqual(outEntry.Thinking, []string{"high"}) {
		t.Fatalf("ApplyEntry efforts = %v, want [high]", outEntry.Thinking)
	}
}

func TestModelPrefsValidation(t *testing.T) {
	s := openTestStore(t)

	// 超长 name 拒绝
	long := make([]rune, 200)
	for i := range long {
		long[i] = 'a'
	}
	if err := s.PutModelPrefs("p", "m", ModelPrefs{DisplayName: strPtr(string(long))}); err == nil {
		t.Fatal("overlong display_name accepted")
	}

	// 缺 key 拒绝
	if err := s.PutModelPrefs("", "m", ModelPrefs{}); err == nil {
		t.Fatal("empty plugin_id accepted")
	}
	if err := s.PutModelPrefs("p", "", ModelPrefs{}); err == nil {
		t.Fatal("empty model_id accepted")
	}

	// 删除不存在的行不是错误（幂等）
	if err := s.DeleteModelPrefs("p", "no-such"); err != nil {
		t.Fatalf("DeleteModelPrefs missing row: %v", err)
	}
}
