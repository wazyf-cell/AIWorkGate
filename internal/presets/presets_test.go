package presets

import (
	"context"
	"errors"
	"testing"

	"aiworkgate/internal/model"
)

func TestLoadValidatesBundle(t *testing.T) {
	b, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(b.Presets) == 0 {
		t.Fatal("bundle is empty")
	}

	// 四类都在。
	for _, kind := range []Kind{KindVendor, KindRelay, KindLocal, KindRemote} {
		if got := len(b.ByKind(kind)); got == 0 {
			t.Fatalf("no %s preset in bundle", kind)
		}
	}

	// vendor 预设含常见厂商，且都带 base_url 模板与默认 models 路径
	// （新增供应商只需填 key）。
	vendors := b.ByKind(KindVendor)
	ids := map[string]bool{}
	for _, v := range vendors {
		ids[v.ID] = true
		if v.BaseURL == "" {
			t.Fatalf("vendor %q has empty base_url", v.ID)
		}
		if v.DefaultModelsPath() == "" {
			t.Fatalf("vendor %q has empty models path", v.ID)
		}
	}
	for _, want := range []string{"openai", "anthropic", "deepseek", "dashscope", "zhipu", "moonshot", "google"} {
		if !ids[want] {
			t.Fatalf("vendor preset %q missing from bundle", want)
		}
	}

	// remote 仅定义不实现连接：允许没有 base_url。
	r, ok := b.Lookup("remote")
	if !ok || r.BaseURL != "" {
		t.Fatalf("remote preset must exist without base_url: %+v", r)
	}
}

func TestFormatModelName(t *testing.T) {
	cases := []struct {
		display, vendor, modelID string
		want                     string
	}{
		{"我的模型", "智谱", "glm-5", "我的模型 · 智谱"},  // 自定义名 · 供应商
		{"", "智谱", "glm-5", "glm-5 · 智谱"},          // 无自定义名 → 用模型 id
		{"我的模型", "", "glm-5", "我的模型"},            // 无 vendor → 只有名
		{"", "", "glm-5", "glm-5"},
		{"  ", "  ", "  ", ""},
	}
	for i, tc := range cases {
		if got := FormatModelName(tc.display, tc.vendor, tc.modelID); got != tc.want {
			t.Fatalf("case %d: FormatModelName(%q,%q,%q) = %q, want %q",
				i, tc.display, tc.vendor, tc.modelID, got, tc.want)
		}
	}
}

// fakeModelsDev 是可注入的 models.dev 客户端。
type fakeModelsDev struct {
	entries []CatalogEntry
	err     error
	calls   int
}

func (f *fakeModelsDev) Lookup(_ context.Context, _ string) ([]CatalogEntry, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.entries, nil
}

func TestFallbackChainOrder(t *testing.T) {
	md := &fakeModelsDev{entries: []CatalogEntry{{ModelID: "md-model"}}}
	fc := FallbackChain{
		Vendor: Bundle{Presets: []Preset{
			{Kind: KindVendor, ID: "deepseek", Name: "DeepSeek", BaseURL: "https://api.deepseek.com/v1",
				Models: []string{"deepseek-chat"}},
		}},
		ModelsDev: md,
		Plugin:    []model.ModelInfo{{ID: "plugin-model", Vendor: "Plug"}},
		Snapshot:  []model.ModelInfo{{ID: "snap-model", Vendor: "Snap"}},
	}

	// 1. vendor 命中 → SourceVendor，不经过 models.dev。
	entry, _, err := fc.Resolve(context.Background(), "deepseek-chat")
	if err != nil || entry.Source != SourceVendor {
		t.Fatalf("vendor hit: entry=%+v err=%v", entry, err)
	}
	if md.calls != 0 {
		t.Fatalf("models.dev was consulted before vendor, calls=%d", md.calls)
	}

	// 2. vendor 未命中、models.dev 命中 → SourceModelsDev。
	entry, _, err = fc.Resolve(context.Background(), "md-model")
	if err != nil || entry.Source != SourceModelsDev {
		t.Fatalf("models.dev hit: entry=%+v err=%v", entry, err)
	}

	// 3. vendor+md 未命中、插件自带命中 → SourcePlugin。
	entry, _, err = fc.Resolve(context.Background(), "plugin-model")
	if err != nil || entry.Source != SourcePlugin {
		t.Fatalf("plugin hit: entry=%+v err=%v", entry, err)
	}

	// 4. 只有本地快照命中 → SourceSnapshot。
	entry, _, err = fc.Resolve(context.Background(), "snap-model")
	if err != nil || entry.Source != SourceSnapshot {
		t.Fatalf("snapshot hit: entry=%+v err=%v", entry, err)
	}

	// 5. 全 miss：干净未中（各源都 "没有" 而非 "拉取失败"）→ 报错，但 failed 表为空。
	_, failed, err := fc.Resolve(context.Background(), "nope")
	if err == nil {
		t.Fatal("unknown model resolved without error")
	}
	t.Logf("all-miss error: %v (failed=%v)", err, failed)
	if len(failed) != 0 {
		t.Fatalf("clean misses are not fetch failures, got failed=%v", failed)
	}

	// 6. models.dev 报错 → 记录失败并继续回落。
	boom := &fakeModelsDev{err: errors.New("models.dev unreachable")}
	fc.ModelsDev = boom
	entry, failed, err = fc.Resolve(context.Background(), "plugin-model")
	if err != nil || entry.Source != SourcePlugin {
		t.Fatalf("plugin hit after md failure: entry=%+v failed=%v err=%v", entry, failed, err)
	}
	if !contains(failed, SourceModelsDev) {
		t.Fatalf("failed list %v should contain models_dev", failed)
	}
}

func contains(list []Source, want Source) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
