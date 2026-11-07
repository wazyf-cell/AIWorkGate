package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"aiworkgate/internal/config"
	"aiworkgate/internal/gateway"
	"aiworkgate/internal/model"
	"aiworkgate/internal/webui"
)

// 与 internal/gateway/redact_e2e_test.go 同一密钥形状（sk-ant-+hex → API_KEY
// 规则）；邮箱做第二个断言对象。不能用 {{KIND_××}} 占位符形做原值——规则视既
// 有占位符为「已脱」而跳过。
const (
	redactTestKey   = "sk-ant-" + "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678901"
	redactTestEmail = "bob@quill.dev"
)

// TestRedactOptionsFromConfigMapsFields pins the config → gateway mapping.
func TestRedactOptionsFromConfigMapsFields(t *testing.T) {
	cfg := config.Default()
	cfg.Redact.Enabled = true
	cfg.Redact.KeyFile = "k.key"
	cfg.Redact.RouteAware = false
	cfg.Redact.Kinds = []string{"SECRET"}
	cfg.Redact.Categories = []string{"ssn"}

	opts := redactOptionsFromConfig(cfg)
	if !opts.On || opts.KeyFile != "k.key" || opts.RouteAware {
		t.Fatalf("opts = %+v", opts)
	}
	if len(opts.Kinds) != 1 || opts.Kinds[0] != "SECRET" || len(opts.Categories) != 1 || opts.Categories[0] != "ssn" {
		t.Fatalf("lists = %v / %v", opts.Kinds, opts.Categories)
	}
}

// TestRedactOptionsChangedDetectsDifferences gates the rebuild: equal options
// must not swap the handler (keeps caches and rate windows warm).
func TestRedactOptionsChangedDetectsDifferences(t *testing.T) {
	base := redactOptionsFromConfig(config.Default())
	if redactOptionsChanged(base, base) {
		t.Fatal("identical options reported as changed")
	}
	// 列表顺序无关（网关侧折叠成 map）。
	a := gateway.RedactOptions{On: true, Kinds: []string{"A", "B"}, Categories: []string{"ssn"}}
	b := gateway.RedactOptions{On: true, Kinds: []string{"B", "A"}, Categories: a.Categories}
	if redactOptionsChanged(a, b) {
		t.Fatal("reordered lists reported as changed")
	}
	if !redactOptionsChanged(a, gateway.RedactOptions{On: false, Kinds: a.Kinds, Categories: a.Categories}) {
		t.Fatal("On flip not detected")
	}
	if !redactOptionsChanged(a, gateway.RedactOptions{On: true, Kinds: []string{"A"}, Categories: a.Categories}) {
		t.Fatal("kinds change not detected")
	}
}

// TestValidateRedactCategoriesRejectsUnknown keeps the closed category set
// aligned with internal/redact's rule table; unknown kinds stay open on purpose
// (the gateway drops lookup misses, so a typo cannot misconfigure anything).
func TestValidateRedactCategoriesRejectsUnknown(t *testing.T) {
	if err := validateRedactCategories([]string{"ssn", "iban"}); err != nil {
		t.Fatalf("valid ids rejected: %v", err)
	}
	if err := validateRedactCategories(nil); err != nil {
		t.Fatalf("empty list rejected: %v", err)
	}
	err := validateRedactCategories([]string{"ssn", "no-such-category"})
	if err == nil || !strings.Contains(err.Error(), "no-such-category") {
		t.Fatalf("unknown id accepted: %v", err)
	}
}

// fakeGWRegistry is the minimal PluginRegistry the direct plugin path needs:
// a local manifest model list avoids the upstream /v1/models fetch, and
// Endpoint points at the echo upstream.
type fakeGWRegistry struct {
	statuses []model.PluginStatus
	baseURL  string
}

func (f *fakeGWRegistry) List() []model.PluginStatus { return f.statuses }

func (f *fakeGWRegistry) Endpoint(pluginID string) (baseURL, token string, ok bool) {
	return f.baseURL, "tok", true
}

func (f *fakeGWRegistry) EnsureRunning(ctx context.Context, pluginID string) error { return nil }

func (f *fakeGWRegistry) Concurrency(pluginID string) int { return 2 }

func (f *fakeGWRegistry) Touch(pluginID string) {}

// compile-time interface check for the fake registry.
var _ gateway.PluginRegistry = (*fakeGWRegistry)(nil)

// fakeGWKeys admits every presented key (the hot-update tests only need the
// request to reach the plugin path).
type fakeGWKeys struct{}

func (fakeGWKeys) Verify(plain string) (id, name string, ok bool) {
	return "test-key-id", "test-key", plain != ""
}

func (fakeGWKeys) Touch(id string) {}

// redactApp builds the smallest App that serves /v1 through a real gateway on
// the direct plugin path (no scheduler), with an echo upstream: the response
// carries the (masked) request body back so the restore half of the wiring is
// exercised too.
func redactApp(t *testing.T, redactOn bool) (*App, func() []byte) {
	t.Helper()
	st := openTestStoreForApp(t)
	dir := t.TempDir()

	var mu sync.Mutex
	var received []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		received = body
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		quoted, _ := json.Marshal(string(body))
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":`+string(quoted)+`}}]}`)
	}))
	t.Cleanup(upstream.Close)

	cfg := config.Default()
	cfg.Redact.Enabled = redactOn
	// httptest.NewRequest 的 RemoteAddr 是 192.0.2.1:1234；全局允许列表放行它。
	cfg.Gateway.AllowIPs = []string{"192.0.2.0/24"}
	// 与生产启动一致：Normalize 填默认 key_file 等字段，保证后续保存的
	// redactOptions 比较从同一基线出发（否则首个 Normalize 就算「变了」）。
	cfg.Normalize()

	// 网关 fail closed：认证通过但 client_keys 无行的 Key 会拿到 503，
	// 所以测试 Key 必须在 store 里有启用的行（fake 验证器返回它的 id）。
	if err := st.UpsertClientKey("test-key-id", "test-key", "agk_", "test-hash", true, 0); err != nil {
		t.Fatalf("upsert test key: %v", err)
	}

	a := &App{
		cfg:         cfg,
		dataDir:     dir,
		store:       st,
		gatewayKeys: fakeGWKeys{},
		gatewayPlugins: &fakeGWRegistry{
			statuses: []model.PluginStatus{{
				Manifest: model.Manifest{ID: "p1", Models: []model.ModelInfo{{ID: "m1"}}},
			}},
			baseURL: upstream.URL,
		},
	}
	a.setGatewayHandler(gateway.New(a.gatewayOptions(cfg, nopHostLogger{})))

	captured := func() []byte {
		mu.Lock()
		defer mu.Unlock()
		return received
	}
	return a, captured
}

func redactChatRequest(t *testing.T) *http.Request {
	t.Helper()
	body := `{"model":"p1/m1","messages":[{"role":"user","content":"mail ` + redactTestEmail + ` key ` + redactTestKey + ` now"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Host = "127.0.0.1"
	req.Header.Set("Authorization", "Bearer test-client-key")
	req.Header.Set("Content-Type", "application/json")
	return req
}

// TestSaveRedactConfigEnablesRedactionHotUpdate is the P10 收尾闭环：保存
// enabled=true 后，不重启进程，下一条 /v1 请求的正文即被脱敏、响应回程还原。
func TestSaveRedactConfigEnablesRedactionHotUpdate(t *testing.T) {
	a, received := redactApp(t, false)

	// off：原文直达上游。
	rr := httptest.NewRecorder()
	a.routeHTTP(rr, redactChatRequest(t))
	if rr.Code != http.StatusOK {
		t.Fatalf("off request = %d (%s)", rr.Code, rr.Body.String())
	}
	if got := received(); !strings.Contains(string(got), redactTestKey) {
		t.Fatalf("off mode upstream should receive plaintext, got %s", got)
	}

	// 保存：PUT 语义走 saveRedactConfig（校验 + 落盘 config.json + 热更新）。
	enabled := true
	status, err := a.saveRedactConfig(webui.RedactUpdate{Enabled: &enabled})
	if err != nil {
		t.Fatalf("saveRedactConfig: %v", err)
	}
	if !status.Enabled {
		t.Fatalf("status after save = %+v", status)
	}
	raw, readErr := os.ReadFile(filepath.Join(a.dataDir, "config.json"))
	if readErr != nil {
		t.Fatalf("config.json missing: %v", readErr)
	}
	var saved struct {
		Redact struct {
			Enabled bool `json:"enabled"`
		} `json:"redact"`
	}
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatalf("config.json unreadable: %v", err)
	}
	if !saved.Redact.Enabled {
		t.Fatalf("config.json does not carry enabled=true: %s", raw)
	}

	// on：上游只看到占位符，客户端拿回明文（回程还原），无指纹残留。
	rr = httptest.NewRecorder()
	a.routeHTTP(rr, redactChatRequest(t))
	if rr.Code != http.StatusOK {
		t.Fatalf("on request = %d (%s)", rr.Code, rr.Body.String())
	}
	got := received()
	if strings.Contains(string(got), redactTestKey) || strings.Contains(string(got), redactTestEmail) {
		t.Fatalf("on mode upstream still received plaintext: %s", got)
	}
	if !strings.Contains(string(got), "{{API_KEY_") || !strings.Contains(string(got), "{{EMAIL_") {
		t.Fatalf("on mode upstream did not receive placeholders: %s", got)
	}
	clientBody := rr.Body.String()
	if !strings.Contains(clientBody, redactTestKey) || !strings.Contains(clientBody, redactTestEmail) {
		t.Fatalf("client did not receive restored plaintext: %s", clientBody)
	}
	if strings.Contains(clientBody, "{{API_KEY_") || strings.Contains(clientBody, "{{EMAIL_") {
		t.Fatalf("client saw masking fingerprints: %s", clientBody)
	}
}

// TestSaveRedactConfigRejectsUnknownCategory keeps bad ids at the API edge and
// leaves the snapshot untouched on failure.
func TestSaveRedactConfigRejectsUnknownCategory(t *testing.T) {
	a, _ := redactApp(t, false)
	bad := "not-a-category"
	_, err := a.saveRedactConfig(webui.RedactUpdate{Categories: &[]string{bad}})
	if err == nil {
		t.Fatal("unknown category accepted")
	}
	if !strings.Contains(err.Error(), bad) {
		t.Fatalf("error %v does not name the bad id", err)
	}
	if got := a.configSnapshot().Redact.Categories; len(got) != 0 {
		t.Fatalf("failed save mutated the snapshot: %v", got)
	}
}

// TestSaveRedactConfigUnchangedOptionsKeepsHandler pins the no-op guard: a
// save that does not change the redact options must not rebuild the gateway.
func TestSaveRedactConfigUnchangedOptionsKeepsHandler(t *testing.T) {
	a, received := redactApp(t, false)
	before := a.currentGateway()

	enabled := false // 与快照同值：归一化后 redactOptions 相等
	if _, err := a.saveRedactConfig(webui.RedactUpdate{Enabled: &enabled}); err != nil {
		t.Fatalf("no-op save failed: %v", err)
	}
	if a.currentGateway() != before {
		t.Fatal("unchanged redact options rebuilt the gateway")
	}

	// 网关行为也不变（off 原文直达）。
	rr := httptest.NewRecorder()
	a.routeHTTP(rr, redactChatRequest(t))
	if rr.Code != http.StatusOK || !strings.Contains(string(received()), redactTestKey) {
		t.Fatalf("no-op save changed behavior: %d %s %s", rr.Code, rr.Body.String(), received())
	}
}
