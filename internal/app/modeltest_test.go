package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aiworkgate/internal/config"
	"aiworkgate/internal/model"
	"aiworkgate/internal/plugins"
	"aiworkgate/internal/scheduler"
	"aiworkgate/internal/store"
	"aiworkgate/internal/webui"
)

// ---------------------------------------------------------------------------
// pure rules: D-39 tiers and otherAPI classification
// ---------------------------------------------------------------------------

func TestSelectModelTestTier(t *testing.T) {
	cases := []struct {
		name        string
		info        model.ModelInfo
		wantTimeout time.Duration
		wantMax     int
		wantStream  bool
	}{
		{"reasoning via thinking tiers", model.ModelInfo{ID: "claude-sonnet", Thinking: []string{"low"}}, modelTestReasoningTimeout, 1024, true},
		{"reasoning via name", model.ModelInfo{ID: "deepseek-r1", Name: "DeepSeek R1"}, modelTestReasoningTimeout, 1024, true},
		{"light via free flag", model.ModelInfo{ID: "gpt-mini", Free: true}, modelTestLightTimeout, 16, false},
		{"light via small context", model.ModelInfo{ID: "phi-3", ContextLength: 8192}, modelTestLightTimeout, 16, false},
		{"light via name", model.ModelInfo{ID: "qwen-flash", ContextLength: 100000}, modelTestLightTimeout, 16, false},
		{"standard", model.ModelInfo{ID: "claude-opus", ContextLength: 200000}, modelTestStandardTimeout, 256, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := selectModelTestTier(tc.info)
			if got.Timeout != tc.wantTimeout || got.MaxTokens != tc.wantMax || got.Stream != tc.wantStream {
				t.Fatalf("tier = %+v, want %v/%d/stream=%v", got, tc.wantTimeout, tc.wantMax, tc.wantStream)
			}
		})
	}
}

func TestClassifyOtherAPI(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"explicit English", http.StatusBadRequest, `{"error":{"message":"model not found in this api"}}`, true},
		{"unknown model", http.StatusBadRequest, `unknown model "x"`, true},
		{"404 naming a model", http.StatusNotFound, `no such endpoint for model`, true},
		{"Chinese wording", http.StatusBadRequest, `该模型不在此 API`, true},
		{"model exists elsewhere", http.StatusUnprocessableEntity, `model not supported for this api`, true},
		{"quota exhaustion", http.StatusTooManyRequests, `insufficient quota`, false},
		{"unauthorized", http.StatusUnauthorized, `not logged in`, false},
		{"400 without model words", http.StatusBadRequest, `context length exceeded`, false},
		{"server error", http.StatusInternalServerError, `model not found`, false},
		{"success status", http.StatusOK, `model not found`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyOtherAPI(tc.status, tc.body); got != tc.want {
				t.Fatalf("classifyOtherAPI(%d, %q) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// mock plugin source: success and failure branches
// ---------------------------------------------------------------------------

type fakeModelTestSource struct {
	status    model.PluginStatus
	models    []model.ModelInfo
	base      string
	token     string
	ensureErr error
}

func (f *fakeModelTestSource) Get(string) (model.PluginStatus, bool)       { return f.status, true }
func (f *fakeModelTestSource) EnsureRunning(context.Context, string) error { return f.ensureErr }
func (f *fakeModelTestSource) ModelsFresh(context.Context, string) ([]model.ModelInfo, error) {
	return f.models, nil
}
func (f *fakeModelTestSource) Endpoint(string) (string, string, bool) { return f.base, f.token, true }

func mockPluginServer(t *testing.T, status int, body string, capture func(map[string]any)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err == nil && capture != nil {
				capture(payload)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func withModelTestSource(t *testing.T, source modelTestSource) {
	t.Helper()
	previous := modelTestSourceFor
	modelTestSourceFor = func(*App) modelTestSource { return source }
	t.Cleanup(func() { modelTestSourceFor = previous })
}

func TestModelTestHappyPath(t *testing.T) {
	var gotBody map[string]any
	srv := mockPluginServer(t, http.StatusOK, `{"choices":[{"message":{"content":"pong"}}]}`, func(b map[string]any) { gotBody = b })

	a := &App{}
	withModelTestSource(t, &fakeModelTestSource{
		status: model.PluginStatus{Enabled: true, Manifest: model.Manifest{APILevel: 1}},
		models: []model.ModelInfo{{ID: "mock-chat", ContextLength: 200000}},
		base:   srv.URL, token: "tok",
	})
	previousSnap := modelTestSnapshotFor
	modelTestSnapshotFor = func(*App, context.Context) (*scheduler.Snapshot, error) { return nil, nil }
	t.Cleanup(func() { modelTestSnapshotFor = previousSnap })

	res, err := a.modelTest("mock", "mock-chat")
	if err != nil {
		t.Fatalf("modelTest: %v", err)
	}
	if !res.OK || res.Status != http.StatusOK || res.MS < 0 {
		t.Fatalf("result = %+v, want ok=true status=200 with ms", res)
	}
	if res.Model != "mock/mock-chat" || res.Error != "" || res.OtherAPI {
		t.Fatalf("result = %+v", res)
	}
	if gotBody["model"] != "mock-chat" || gotBody["max_tokens"].(float64) != 256 {
		t.Fatalf("plugin saw %+v, want model mock-chat max_tokens 256", gotBody)
	}
}

func TestModelTestFailureBranches(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		wantOther bool
	}{
		{"model not on this api", http.StatusNotFound, `{"error":{"message":"model not found in this api"}}`, true},
		{"not logged in", http.StatusUnauthorized, `{"error":{"message":"not logged in, please sign in first"}}`, false},
		{"generic 500", http.StatusInternalServerError, `{"error":{"message":"upstream exploded"}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := mockPluginServer(t, tc.status, tc.body, nil)
			a := &App{}
			withModelTestSource(t, &fakeModelTestSource{
				status: model.PluginStatus{Enabled: true},
				models: []model.ModelInfo{{ID: "mock-chat"}},
				base:   srv.URL, token: "tok",
			})
			previousSnap := modelTestSnapshotFor
			modelTestSnapshotFor = func(*App, context.Context) (*scheduler.Snapshot, error) { return nil, nil }
			t.Cleanup(func() { modelTestSnapshotFor = previousSnap })

			res, err := a.modelTest("mock", "mock-chat")
			if err != nil {
				t.Fatalf("modelTest: %v", err)
			}
			if res.OK || res.Status != tc.status {
				t.Fatalf("result = %+v, want ok=false status=%d", res, tc.status)
			}
			if res.OtherAPI != tc.wantOther {
				t.Fatalf("otherAPI = %v, want %v (error %q)", res.OtherAPI, tc.wantOther, res.Error)
			}
			if strings.TrimSpace(res.Error) == "" {
				t.Fatalf("result = %+v, want a readable error", res)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// batch: per-provider parallelism, one provider failure isolation, order
// ---------------------------------------------------------------------------

func TestModelTestBatchParallelAndIsolated(t *testing.T) {
	var a App
	previous := modelTestRun
	defer func() { modelTestRun = previous }()

	providersRan := make(chan string, 8)
	bDone := make(chan struct{})
	modelTestRun = func(_ *App, provider, modelID string) webui.ModelTestResult {
		providersRan <- provider
		switch provider {
		case "a":
			// A blocks until provider b has finished: if the batch were
			// sequential this would deadlock the test instead of passing.
			select {
			case <-bDone:
			case <-time.After(5 * time.Second):
				return webui.ModelTestResult{Model: provider + "/" + modelID, Error: "provider b never ran (no parallelism)"}
			}
		case "b":
			close(bDone)
		case "boom":
			panic("provider boom exploded")
		}
		return webui.ModelTestResult{OK: true, Model: provider + "/" + modelID}
	}

	items := []webui.ModelTestItem{
		{Provider: "a", Model: "m1"},
		{Provider: "boom", Model: "m2"},
		{Provider: "a", Model: "m3"},
		{Provider: "b", Model: "m4"},
	}
	results, err := a.modelTestBatch(items)
	if err != nil {
		t.Fatalf("modelTestBatch: %v", err)
	}
	if len(results) != len(items) {
		t.Fatalf("got %d results, want %d", len(results), len(items))
	}
	// Order is preserved.
	for i, item := range items {
		if results[i].Model != item.Provider+"/"+item.Model {
			t.Fatalf("results[%d] = %+v, want order-preserving entry %s", i, results[i], item)
		}
	}
	if results[1].Error != "内部错误" || results[1].OK {
		t.Fatalf("boom provider result = %+v, want isolated failure", results[1])
	}
	if !results[0].OK || !results[3].OK {
		t.Fatalf("panic isolation leaked: %+v", results)
	}
	// Providers a, boom, b all ran (a waits for b, so b must have started); a
	// serves two items, so it runs twice. Count with a deadline instead of a
	// bare select so a slow goroutine cannot be misread as missing.
	seen := map[string]int{}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case p := <-providersRan:
			seen[p]++
		default:
			time.Sleep(5 * time.Millisecond)
		}
		if seen["a"] >= 2 && seen["b"] >= 1 && seen["boom"] >= 1 {
			break
		}
	}
	if seen["a"] != 2 || seen["boom"] != 1 || seen["b"] != 1 {
		t.Fatalf("provider runs = %v, want a:2 boom:1 b:1", seen)
	}
}

// ---------------------------------------------------------------------------
// acceptance: the real fake plugin through the scheduler + the webui route
// ---------------------------------------------------------------------------

func installModelTestPlugin(t *testing.T, a *App, id string) {
	t.Helper()
	src := filepath.Join(t.TempDir(), id)
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	mf := model.Manifest{
		ID:           id,
		Name:         "Model test fixture",
		Version:      "0.0.1",
		APILevel:     1,
		Capabilities: []string{model.CapUpstream, model.CapAccounts},
		Startup:      model.StartupConfig{Mode: model.StartupAlways},
		Entry:        model.Entry{Exe: "fake-plugin.exe"},
	}
	raw, err := json.Marshal(mf)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, plugins.ManifestFileName), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "fake-plugin.exe"), directFakePlugin(t), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := a.pluginMgr.InstallDir(src); err != nil {
		t.Fatalf("InstallDir: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		status, ok := a.pluginMgr.Get(id)
		if ok && status.Running && status.Healthy && status.Port != 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("plugin %q did not become healthy", id)
}

func TestModelTestThroughRealPluginRoute(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	a := &App{
		cfg:     config.Config{},
		cfgMu:   sync.RWMutex{},
		dataDir: dir,
		store:   st,
	}
	a.pluginMgr = plugins.New(a.pluginOptions(dir, st, nil, nil, nil))
	t.Cleanup(func() { _ = a.pluginMgr.Close() })
	a.dispatch = scheduler.New(scheduler.Options{Sticky: stickyStoreAdapter{store: st}})

	installModelTestPlugin(t, a, "fake")

	// The fake plugin has no /v1/chat/completions handler: the request reaches
	// the plugin and it answers 404. This is the failure branch the acceptance
	// asks for (analogous to trae not being logged in) and it proves the whole
	// chain (route → scheduler pin → plugin POST → shaping) runs.
	res, err := a.modelTest("fake", "fake-chat")
	if err != nil {
		t.Fatalf("modelTest: %v", err)
	}
	if res.OK || res.Status != http.StatusNotFound || res.MS < 0 || res.Model != "fake/fake-chat" {
		t.Fatalf("result = %+v, want ok=false status=404 model=fake/fake-chat", res)
	}
	if !strings.Contains(res.Error, "404") || strings.Contains(res.Error, "Bearer") {
		t.Fatalf("error = %q, want a readable 404 reason without credentials", res.Error)
	}

	// Same chain through the admin HTTP route, through the real webui mux.
	h := webui.New(webui.Options{
		SessionToken:   "test-token",
		ModelTest:      a.modelTest,
		ModelTestBatch: a.modelTestBatch,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/models/fake/fake-chat/test", nil)
	req.AddCookie(&http.Cookie{Name: "dsh_admin_session", Value: "test-token"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/models/fake/fake-chat/test = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var routed webui.ModelTestResult
	if err := json.Unmarshal(rec.Body.Bytes(), &routed); err != nil {
		t.Fatalf("decode route result: %v (%s)", err, rec.Body.String())
	}
	if routed.OK || routed.Status != http.StatusNotFound || routed.Model != "fake/fake-chat" {
		t.Fatalf("routed result = %+v, want the same failure shape", routed)
	}
	// The probe row lands in the existing diagnostics table under model_test.
	rows, err := a.listConnectivityProbes(20, webui.ProbeKindModelTest, "")
	if err != nil {
		t.Fatalf("probe history: %v", err)
	}
	if len(rows) == 0 || !rows[0].OK == false || rows[0].Kind != webui.ProbeKindModelTest {
		t.Fatalf("probe rows = %+v, want a failed model_test row", rows)
	}
	t.Logf("acceptance result: %+v", routed)
}

func TestModelTestMissingModel(t *testing.T) {
	srv := mockPluginServer(t, http.StatusOK, `{}`, nil)
	a := &App{}
	withModelTestSource(t, &fakeModelTestSource{
		status: model.PluginStatus{Enabled: true},
		models: []model.ModelInfo{{ID: "other-model"}},
		base:   srv.URL, token: "tok",
	})
	previousSnap := modelTestSnapshotFor
	modelTestSnapshotFor = func(*App, context.Context) (*scheduler.Snapshot, error) { return nil, nil }
	t.Cleanup(func() { modelTestSnapshotFor = previousSnap })

	res, err := a.modelTest("mock", "ghost")
	if err != nil {
		t.Fatalf("modelTest: %v", err)
	}
	if res.OK || res.Status != http.StatusNotFound || !strings.Contains(res.Error, "模型不存在") {
		t.Fatalf("result = %+v, want model_missing", res)
	}
}

func TestModelTestEnsureRunningTimeout(t *testing.T) {
	a := &App{}
	withModelTestSource(t, &fakeModelTestSource{
		status:    model.PluginStatus{Enabled: true},
		models:    []model.ModelInfo{{ID: "m"}},
		ensureErr: errors.New("context deadline exceeded"),
	})
	res, err := a.modelTest("mock", "m")
	if err != nil {
		t.Fatalf("modelTest: %v", err)
	}
	if res.OK || !strings.Contains(res.Error, "插件未就绪") {
		t.Fatalf("result = %+v, want ensure-running failure", res)
	}
}

func TestModelTestErrorMessage(t *testing.T) {
	got := modelTestErrorMessage(http.StatusBadRequest, []byte(`{"error":{"message":"bad"}}`))
	if got != "bad" {
		t.Fatalf("message = %q", got)
	}
	got = modelTestErrorMessage(http.StatusBadRequest, []byte(`plain text error`))
	if got != "plain text error" {
		t.Fatalf("message = %q", got)
	}
	got = modelTestErrorMessage(http.StatusBadRequest, nil)
	if got != http.StatusText(http.StatusBadRequest) {
		t.Fatalf("message = %q, want status text", got)
	}
}
