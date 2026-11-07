package presets

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aiworkgate/internal/model"
)

// fakeModelsDevDoc is a trimmed models.dev/api.json document: top-level object
// keyed by provider id, each with name and a models object keyed by model id.
const fakeModelsDevDoc = `{
  "openai": {
    "id": "openai",
    "name": "OpenAI",
    "models": {
      "gpt-test": {
        "id": "gpt-test",
        "name": "GPT Test",
        "modalities": {"input": ["text", "image"], "output": ["text"]},
        "limit": {"context": 128000, "output": 16384}
      }
    }
  },
  "deepseek": {
    "id": "deepseek",
    "name": "DeepSeek",
    "models": {
      "deepseek-chat": {
        "id": "deepseek-chat",
        "name": "DeepSeek Chat",
        "modalities": {"input": ["text"], "output": ["text"]},
        "limit": {"context": 64000}
      }
    }
  },
  "empty-provider": {"id": "empty-provider", "name": "No Models"}
}`

func fakeModelsDevServer(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newModelsDevFor(t *testing.T, srv *httptest.Server) *ModelsDevClient {
	t.Helper()
	c, err := NewModelsDev(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("NewModelsDev: %v", err)
	}
	return c
}

func TestModelsDevFetchAndLookup(t *testing.T) {
	c := newModelsDevFor(t, fakeModelsDevServer(t, fakeModelsDevDoc, http.StatusOK))

	// 裸模型 id：跨 vendor 命中，字段按文档形状填充。
	entries, err := c.Lookup(context.Background(), "deepseek-chat")
	if err != nil {
		t.Fatalf("Lookup deepseek-chat: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1: %+v", len(entries), entries)
	}
	e := entries[0]
	if e.ModelID != "deepseek-chat" || e.Vendor != "DeepSeek" {
		t.Fatalf("entry identity = %+v", e)
	}
	if e.ContextLength != 64000 {
		t.Fatalf("context = %d, want 64000", e.ContextLength)
	}
	if e.Vision {
		t.Fatalf("text-only model marked vision")
	}
	if e.DisplayName != "DeepSeek Chat · DeepSeek" {
		t.Fatalf("display = %q, want the vendor-suffixed name", e.DisplayName)
	}
	if e.Source != "" {
		t.Fatalf("client must not stamp Source, the chain does (got %q)", e.Source)
	}

	// vision 标记来自 modalities.input 含 image。
	entries, err = c.Lookup(context.Background(), "gpt-test")
	if err != nil || len(entries) != 1 {
		t.Fatalf("Lookup gpt-test: %d entries, err %v", len(entries), err)
	}
	if !entries[0].Vision || entries[0].ContextLength != 128000 {
		t.Fatalf("gpt-test = %+v, want vision + 128000 context", entries[0])
	}

	// vendor/model 复合查询只命中对应 vendor。
	entries, err = c.Lookup(context.Background(), "openai/gpt-test")
	if err != nil || len(entries) != 1 || entries[0].Vendor != "OpenAI" {
		t.Fatalf("vendor-prefixed lookup = %+v, err %v", entries, err)
	}
	if entries, err := c.Lookup(context.Background(), "deepseek/gpt-test"); err != nil || len(entries) != 0 {
		t.Fatalf("wrong-vendor prefix must be a clean miss: %+v, err %v", entries, err)
	}

	// 大小写不敏感 + 未知 id 是干净 miss（非错误）。
	if entries, err = c.Lookup(context.Background(), "GPT-TEST"); err != nil || len(entries) != 1 {
		t.Fatalf("case-insensitive lookup: %+v, err %v", entries, err)
	}
	if entries, err = c.Lookup(context.Background(), "no-such-model"); err != nil || len(entries) != 0 {
		t.Fatalf("unknown id: %+v, err %v", entries, err)
	}
}

func TestModelsDevFetchErrorsAreNotFatalAndNotPanics(t *testing.T) {
	// 404 → 错误（链会把它记成 failed 源并继续）。
	c := newModelsDevFor(t, fakeModelsDevServer(t, "nope", http.StatusNotFound))
	if _, err := c.Fetch(context.Background()); err == nil {
		t.Fatal("404 must be an error")
	}

	// 坏 JSON → 解析错误，不 panic。
	c = newModelsDevFor(t, fakeModelsDevServer(t, "{not json", http.StatusOK))
	if _, err := c.Fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("bad json error = %v, want a decode error", err)
	}

	// 空文档 → 错误。
	c = newModelsDevFor(t, fakeModelsDevServer(t, "{}", http.StatusOK))
	if _, err := c.Fetch(context.Background()); err == nil {
		t.Fatal("empty document must be an error")
	}

	// 网络不可达（服务已关）→ 错误。
	srv := fakeModelsDevServer(t, fakeModelsDevDoc, http.StatusOK)
	url := srv.URL
	srv.Close()
	c, err := NewModelsDev(url, srv.Client())
	if err != nil {
		t.Fatalf("NewModelsDev: %v", err)
	}
	if _, err := c.Fetch(context.Background()); err == nil {
		t.Fatal("unreachable server must be an error")
	}
}

// TestFallbackChainWithRealModelsDev pins the D-27⑤ end-to-end order with the
// real transport: vendor miss → models.dev (httptest) hit; models.dev failure
// → 插件; everything above missing → 快照.
func TestFallbackChainWithRealModelsDev(t *testing.T) {
	c := newModelsDevFor(t, fakeModelsDevServer(t, fakeModelsDevDoc, http.StatusOK))
	fc := FallbackChain{
		Vendor: Bundle{Presets: []Preset{
			{Kind: KindVendor, ID: "deepseek", Name: "DeepSeek", BaseURL: "https://api.deepseek.com/v1",
				Models: []string{"deepseek-chat"}},
		}},
		ModelsDev: c,
		Plugin:    []model.ModelInfo{{ID: "plugin-only", Vendor: "Plug"}},
		Snapshot:  []model.ModelInfo{{ID: "snap-only", Vendor: "Snap"}},
	}
	ctx := context.Background()

	// vendor 命中不经过 models.dev。
	entry, _, err := fc.Resolve(ctx, "deepseek-chat")
	if err != nil || entry.Source != SourceVendor {
		t.Fatalf("vendor first: %+v err %v", entry, err)
	}

	// vendor miss → models.dev 命中，Source 被链盖章。
	entry, _, err = fc.Resolve(ctx, "gpt-test")
	if err != nil || entry.Source != SourceModelsDev {
		t.Fatalf("models.dev hit: %+v err %v", entry, err)
	}
	if entry.ContextLength != 128000 || !entry.Vision {
		t.Fatalf("models.dev entry lost metadata: %+v", entry)
	}

	// models.dev 挂掉（网络错误）→ 记 failed，继续插件源。
	dead, _ := NewModelsDev("http://127.0.0.1:1/x", nil)
	fc.ModelsDev = dead
	entry, failed, err := fc.Resolve(ctx, "plugin-only")
	if err != nil || entry.Source != SourcePlugin {
		t.Fatalf("plugin fallback after md failure: %+v failed=%v err=%v", entry, failed, err)
	}
	if !contains(failed, SourceModelsDev) {
		t.Fatalf("failed %v should contain models_dev", failed)
	}

	// 插件也没有 → 快照兜底，failed 仍记录 models_dev。
	entry, failed, err = fc.Resolve(ctx, "snap-only")
	if err != nil || entry.Source != SourceSnapshot {
		t.Fatalf("snapshot fallback: %+v failed=%v err=%v", entry, failed, err)
	}
	if !contains(failed, SourceModelsDev) {
		t.Fatalf("failed %v should contain models_dev", failed)
	}
}

// The 15s timeout is a plan constant, pinned here so a refactor cannot drop it.
func TestModelsDevTimeoutIsFifteenSeconds(t *testing.T) {
	if DefaultModelsDevTimeout != 15*time.Second {
		t.Fatalf("DefaultModelsDevTimeout = %v, want 15s", DefaultModelsDevTimeout)
	}
}
