package presets

import (
	"context"
	"strings"
	"testing"

	"aiworkgate/internal/model"
)

// D-36 字段语义测试：Region/套餐子表、Only、NoList、EndpointHint、presetAliases
// 的解析、校验与生效点（vendor 源的 Only 过滤、Lookup 的别名迁移）。

func TestLoadParsesD36Fields(t *testing.T) {
	b, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// presetAliases 迁移：旧/短 id 解析到规范预设。
	for alias, want := range map[string]string{
		"azure":  "azure-openai",
		"qwen":   "dashscope",
		"glm":    "zhipu",
		"kimi":   "moonshot",
		"gemini": "google",
	} {
		p, ok := b.Lookup(alias)
		if !ok || p.ID != want {
			t.Fatalf("Lookup(%q) = %+v ok=%v, want canonical %q", alias, p, ok, want)
		}
	}
	// 直接命中优先于别名。
	if p, ok := b.Lookup("openai"); !ok || p.ID != "openai" {
		t.Fatalf("direct hit broken: %+v ok=%v", p, ok)
	}
	if _, ok := b.Lookup("no-such-alias"); ok {
		t.Fatal("unknown id must miss")
	}

	// Region 子表：dashscope 有国内外两个地域，各自带 base_url。
	ds, ok := b.Lookup("dashscope")
	if !ok || len(ds.Region) != 2 {
		t.Fatalf("dashscope region sub-table = %+v ok=%v", ds.Region, ok)
	}
	intl := false
	for _, r := range ds.Region {
		if r.ID == "intl" && strings.Contains(r.BaseURL, "dashscope-intl") && r.Name != "" {
			intl = true
		}
	}
	if !intl {
		t.Fatalf("dashscope intl region missing/malformed: %+v", ds.Region)
	}

	// Only 白名单 / NoList / EndpointHint。
	dp, _ := b.Lookup("deepseek")
	if len(dp.Only) == 0 || !dp.ExposesModel("deepseek-chat") || dp.ExposesModel("deepseek-vision") {
		t.Fatalf("deepseek Only semantics broken: only=%v exposes=%v/%v",
			dp.Only, dp.ExposesModel("deepseek-chat"), dp.ExposesModel("deepseek-vision"))
	}
	relay, _ := b.Lookup("relay")
	if !relay.NoList {
		t.Fatal("relay preset must carry no_list=true")
	}
	azure, _ := b.Lookup("azure-openai")
	if strings.TrimSpace(azure.EndpointHint) == "" {
		t.Fatal("azure preset must carry an endpoint_hint")
	}
	// 空白名单 = 全部放行。
	if !(Preset{}).ExposesModel("anything") {
		t.Fatal("empty Only must admit every model")
	}
}

func TestBundleValidationRejectsBadAliases(t *testing.T) {
	base := Bundle{Presets: []Preset{
		{Kind: KindVendor, ID: "openai", Name: "OpenAI", BaseURL: "https://api.openai.com/v1"},
	}}
	cases := []struct {
		name    string
		aliases map[string]string
		want    string
	}{
		{"alias to unknown preset", map[string]string{"oai": "nope"}, "unknown preset"},
		{"alias shadows a real id", map[string]string{"openai": "openai"}, "shadows"},
		{"empty alias key", map[string]string{"": "openai"}, "empty key"},
	}
	for _, tc := range cases {
		b := base
		b.PresetAliases = tc.aliases
		if err := b.validateAliases(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: validateAliases = %v, want %q", tc.name, err, tc.want)
		}
	}
	// 合法别名通过。
	ok := base
	ok.PresetAliases = map[string]string{"oai": "openai"}
	if err := ok.validateAliases(); err != nil {
		t.Fatalf("valid alias rejected: %v", err)
	}
}

func TestPresetValidationRejectsBadD36Fields(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Preset)
		want   string
	}{
		{
			name:   "region without id",
			mutate: func(p *Preset) { p.Region = []PresetRegion{{Name: "x"}} },
			want:   "no id",
		},
		{
			name:   "region without name",
			mutate: func(p *Preset) { p.Region = []PresetRegion{{ID: "cn"}} },
			want:   "no name",
		},
		{
			name: "duplicate region id",
			mutate: func(p *Preset) {
				p.Region = []PresetRegion{{ID: "cn", Name: "国内"}, {ID: "cn", Name: "again"}}
			},
			want: "twice",
		},
		{
			name:   "region base_url not http",
			mutate: func(p *Preset) { p.Region = []PresetRegion{{ID: "cn", Name: "n", BaseURL: "ftp://x"}} },
			want:   "http(s)",
		},
		{
			name:   "empty Only member",
			mutate: func(p *Preset) { p.Only = []string{"a", "  "} },
			want:   "empty model id",
		},
	}
	for _, tc := range cases {
		p := Preset{Kind: KindVendor, ID: "x", Name: "X", BaseURL: "https://x.example/v1"}
		tc.mutate(&p)
		if err := p.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: Validate = %v, want %q", tc.name, err, tc.want)
		}
	}
	// 合法 D-36 字段组合通过（remote 不需要 base_url，但 region 校验仍生效）。
	p := Preset{Kind: KindRemote, ID: "remote", Name: "R"}
	p.Region = []PresetRegion{{ID: "cn", Name: "国内"}}
	if err := p.Validate(); err != nil {
		t.Fatalf("valid remote region rejected: %v", err)
	}
}

// TestFallbackChainHonorsOnly pins the D-36 Only filter inside the vendor
// source of the fallback chain: an excluded model must not resolve as vendor.
func TestFallbackChainHonorsOnly(t *testing.T) {
	fc := FallbackChain{
		Vendor: Bundle{Presets: []Preset{
			{Kind: KindVendor, ID: "deepseek", Name: "DeepSeek", BaseURL: "https://api.deepseek.com/v1",
				Models: []string{"deepseek-chat", "deepseek-vision"},
				Only:   []string{"deepseek-chat"}},
		}},
		Snapshot: []model.ModelInfo{{ID: "deepseek-vision", Vendor: "DeepSeek"}},
	}
	entry, _, err := fc.Resolve(context.Background(), "deepseek-chat")
	if err != nil || entry.Source != SourceVendor {
		t.Fatalf("allow-listed model: %+v err %v", entry, err)
	}
	// 被排除的模型绕过 vendor，直接落到快照层。
	entry, _, err = fc.Resolve(context.Background(), "deepseek-vision")
	if err != nil || entry.Source != SourceSnapshot {
		t.Fatalf("excluded model must skip vendor: %+v err %v", entry, err)
	}
}
