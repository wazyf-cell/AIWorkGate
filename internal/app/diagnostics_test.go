package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aiworkgate/internal/model"
	"aiworkgate/internal/secrets"
	"aiworkgate/internal/store"
	"aiworkgate/internal/webui"
)

// fakePluginProbe is the plugin manager slice the probe consumes.
type fakePluginProbe struct {
	statuses    map[string]model.PluginStatus
	models      []model.ModelInfo
	ensureErr   error
	modelsErr   error
	ensureCalls int
	deadline    time.Duration
}

func (f *fakePluginProbe) Get(id string) (model.PluginStatus, bool) {
	status, ok := f.statuses[id]
	return status, ok
}

func (f *fakePluginProbe) EnsureRunning(ctx context.Context, id string) error {
	f.ensureCalls++
	if deadline, ok := ctx.Deadline(); ok {
		f.deadline = time.Until(deadline)
	}
	return f.ensureErr
}

func (f *fakePluginProbe) ModelsFresh(ctx context.Context, id string) ([]model.ModelInfo, error) {
	if f.modelsErr != nil {
		return nil, f.modelsErr
	}
	if deadline, ok := ctx.Deadline(); ok {
		f.deadline = time.Until(deadline)
	}
	return f.models, nil
}

func withPluginProbe(t *testing.T, source pluginProbeSource) {
	t.Helper()
	previous := connectivityPluginSource
	connectivityPluginSource = func(*App) pluginProbeSource { return source }
	t.Cleanup(func() { connectivityPluginSource = previous })
}

func TestProbeAPluginModelFound(t *testing.T) {
	source := &fakePluginProbe{
		statuses: map[string]model.PluginStatus{
			"officeace": {Enabled: true, Running: true, Healthy: true},
		},
		models: []model.ModelInfo{{ID: "glm-5.3"}, {ID: "glm-5.2"}, {ID: "glm-4"}},
	}
	withPluginProbe(t, source)

	// The "<plugin>/<id>" form must be accepted and normalised.
	out := (&App{}).probePluginConnectivity("officeace", "officeace/glm-5.3")
	if !out.ok || out.status != http.StatusOK || out.errorCode != "" {
		t.Fatalf("outcome = %+v, want ok", out)
	}
	if out.target != "officeace" || out.targetID != "officeace/glm-5.3" {
		t.Fatalf("target = %q/%q", out.target, out.targetID)
	}
	if out.detail != "模型存在 · 3 个模型" {
		t.Fatalf("detail = %q", out.detail)
	}
	if out.latencyMS < 0 {
		t.Fatalf("latency = %d", out.latencyMS)
	}
	if source.deadline <= 0 || source.deadline > connectivityProbeTimeout+time.Second {
		t.Fatalf("models deadline = %s, want within %s", source.deadline, connectivityProbeTimeout)
	}
	if source.ensureCalls != 1 {
		t.Fatalf("EnsureRunning calls = %d, want 1", source.ensureCalls)
	}
}

func TestProbeAPluginModelMissing(t *testing.T) {
	source := &fakePluginProbe{
		statuses: map[string]model.PluginStatus{"officeace": {Enabled: true}},
		models:   []model.ModelInfo{{ID: "glm-5.3"}, {ID: "glm-4"}},
	}
	withPluginProbe(t, source)

	out := (&App{}).probePluginConnectivity("officeace", "glm-5.3x")
	if out.ok || out.errorCode != "model_missing" || out.status != http.StatusOK {
		t.Fatalf("outcome = %+v, want model_missing", out)
	}
	if out.detail != "模型不存在 · 2 个模型" || out.targetID != "officeace/glm-5.3x" {
		t.Fatalf("detail/target = %q/%q", out.detail, out.targetID)
	}
}

func TestProbeAPluginNotConfigured(t *testing.T) {
	withPluginProbe(t, &fakePluginProbe{statuses: map[string]model.PluginStatus{
		"off": {Enabled: false},
	}})
	if out := (&App{}).probePluginConnectivity("missing", "m"); out.errorCode != "not_configured" ||
		!strings.Contains(out.detail, "未安装") {
		t.Fatalf("unknown plugin = %+v", out)
	}
	if out := (&App{}).probePluginConnectivity("off", "m"); out.errorCode != "not_configured" ||
		!strings.Contains(out.detail, "已停用") {
		t.Fatalf("disabled plugin = %+v", out)
	}
}

func TestProbeAPluginTimeout(t *testing.T) {
	source := &fakePluginProbe{
		statuses:  map[string]model.PluginStatus{"officeace": {Enabled: true}},
		modelsErr: context.DeadlineExceeded,
	}
	withPluginProbe(t, source)

	out := (&App{}).probePluginConnectivity("officeace", "glm-5.3")
	if out.ok || out.errorCode != "timeout" || out.status != 0 {
		t.Fatalf("timeout outcome = %+v", out)
	}
	if out.detail == "" {
		t.Fatal("timeout detail is empty")
	}

	source.modelsErr = nil
	source.ensureErr = context.DeadlineExceeded
	out = (&App{}).probePluginConnectivity("officeace", "glm-5.3")
	if out.errorCode != "timeout" {
		t.Fatalf("ensure timeout = %+v", out)
	}
}

// fakeAStack serves GET /health and GET /v1/models with two models.
func fakeAStack(t *testing.T, models int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			io.WriteString(w, `{"status":"ok"}`)
		case "/v1/models":
			entries := make([]map[string]string, 0, models)
			for i := 0; i < models; i++ {
				entries = append(entries, map[string]string{"id": "a-model-" + string(rune('a'+i))})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": entries})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestProbeAHealthy(t *testing.T) {
	srv := fakeAStack(t, 2)
	a := &App{aBaseURL: srv.URL, aURL: srv.URL}
	a.aDiscoverPort = func() (int, bool) { return 0, false }

	out := a.probeAConnectivity()
	if !out.ok || out.status != http.StatusOK || out.errorCode != "" {
		t.Fatalf("outcome = %+v, want ok", out)
	}
	if out.target != "a" || out.targetID != hostPortOnly(srv.URL) {
		t.Fatalf("target = %q/%q, want host:port %q", out.target, out.targetID, hostPortOnly(srv.URL))
	}
	if out.detail != "A 运行中 · 2 个模型" {
		t.Fatalf("detail = %q", out.detail)
	}
	if strings.Contains(out.targetID, "http") || strings.Contains(out.targetID, "/") {
		t.Fatalf("target_id still carries scheme/path: %q", out.targetID)
	}
}

func TestProbeAUnreachable(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	a := &App{aBaseURL: deadURL, aURL: deadURL}
	a.aDiscoverPort = func() (int, bool) { return 0, false }
	out := a.probeAConnectivity()
	if out.ok || out.errorCode != "unreachable" {
		t.Fatalf("unreachable outcome = %+v", out)
	}
	if out.targetID != hostPortOnly(deadURL) || out.detail == "" {
		t.Fatalf("target/detail = %q/%q", out.targetID, out.detail)
	}
}

// newCustomModelApp builds a host with a real store and DPAPI secret store,
// both rooted in a temporary directory.
func newCustomModelApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sec, err := secrets.Open(dir)
	if err != nil {
		t.Fatalf("secrets.Open: %v", err)
	}
	t.Cleanup(func() { _ = sec.Close() })
	return &App{store: st, secrets: sec}
}

func addCustomModel(t *testing.T, a *App, baseURL string) store.CustomModel {
	t.Helper()
	row, err := a.store.UpsertCustomModel(store.CustomModel{
		Name: "my-model", BaseURL: baseURL, UpstreamModel: "vendor/model", Enabled: true,
	})
	if err != nil {
		t.Fatalf("UpsertCustomModel: %v", err)
	}
	if err := a.secrets.SetExtra(store.CustomModelSecretKey(row.ID), "sk-test-secret"); err != nil {
		t.Fatalf("SetExtra: %v", err)
	}
	return row
}

func TestProbeCustomModelSuccess(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)

	a := newCustomModelApp(t)
	row := addCustomModel(t, a, srv.URL)
	out := a.probeCustomModelConnectivity(row.ID)
	if !out.ok || out.status != http.StatusOK || out.errorCode != "" {
		t.Fatalf("outcome = %+v, want ok", out)
	}
	if out.target != "my-model" || out.targetID != row.ID {
		t.Fatalf("target = %q/%q", out.target, out.targetID)
	}
	if gotPath != "/v1/chat/completions" || gotAuth != "Bearer sk-test-secret" {
		t.Fatalf("upstream saw %s / %q", gotPath, gotAuth)
	}
	if !strings.Contains(out.detail, "连通") {
		t.Fatalf("detail = %q", out.detail)
	}
}

func TestProbeCustomModelUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"bad key sk-upstream-body"}}`)
	}))
	t.Cleanup(srv.Close)

	a := newCustomModelApp(t)
	row := addCustomModel(t, a, srv.URL)
	out := a.probeCustomModelConnectivity(row.ID)
	if out.ok || out.errorCode != "upstream_error" || out.status != http.StatusUnauthorized {
		t.Fatalf("401 outcome = %+v", out)
	}
	if strings.Contains(out.detail, "sk-upstream-body") || strings.Contains(out.detail, "sk-test-secret") {
		t.Fatalf("detail leaked the upstream body or key: %q", out.detail)
	}
}

func TestProbeCustomModelRedirectNotFollowed(t *testing.T) {
	var hits int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	t.Cleanup(target.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	a := newCustomModelApp(t)
	row := addCustomModel(t, a, srv.URL)
	out := a.probeCustomModelConnectivity(row.ID)
	if out.ok || out.errorCode != "upstream_error" || out.status != http.StatusFound {
		t.Fatalf("redirect outcome = %+v", out)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatal("redirect target was contacted: the probe followed a credential-bearing 3xx")
	}
}

func TestProbeCustomModelMissingRow(t *testing.T) {
	a := newCustomModelApp(t)
	out := a.probeCustomModelConnectivity("cm-missing")
	if out.ok || out.errorCode != "not_configured" || out.targetID != "cm-missing" {
		t.Fatalf("missing outcome = %+v", out)
	}
}

// TestProbeConnectivityPersistsBothOutcomes proves failed probes are recorded
// and the returned probe_id/at match the stored row.
func TestProbeConnectivityPersistsBothOutcomes(t *testing.T) {
	source := &fakePluginProbe{
		statuses: map[string]model.PluginStatus{"officeace": {Enabled: true}},
		models:   []model.ModelInfo{{ID: "glm-5.3"}},
	}
	withPluginProbe(t, source)

	a := newCustomModelApp(t) // reuse the temp store wiring
	okResult, err := a.probeConnectivity(webui.ProbeRequest{
		Kind: webui.ProbeKindPlugin, PluginID: "officeace", Model: "glm-5.3",
	})
	if err != nil {
		t.Fatalf("probeConnectivity: %v", err)
	}
	if !okResult.OK || okResult.ProbeID <= 0 || okResult.At <= 0 || okResult.Kind != "plugin" {
		t.Fatalf("ok result = %+v", okResult)
	}

	missing, err := a.probeConnectivity(webui.ProbeRequest{
		Kind: webui.ProbeKindPlugin, PluginID: "officeace", Model: "nope",
	})
	if err != nil {
		t.Fatalf("probeConnectivity missing: %v", err)
	}
	if missing.OK || missing.ErrorCode != "model_missing" || missing.ProbeID <= okResult.ProbeID {
		t.Fatalf("missing result = %+v", missing)
	}

	rows, err := a.listConnectivityProbes(20, "plugin", "")
	if err != nil {
		t.Fatalf("listConnectivityProbes: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("stored rows = %d, want 2 (%+v)", len(rows), rows)
	}
	if rows[0].ID != missing.ProbeID || rows[0].ErrorCode != "model_missing" || rows[0].CreatedAt != missing.At {
		t.Fatalf("newest row = %+v, want the failed probe", rows[0])
	}
	if rows[1].ID != okResult.ProbeID || !rows[1].OK || rows[1].ErrorCode != "" {
		t.Fatalf("oldest row = %+v, want the successful probe", rows[1])
	}
}

func TestRedactedProbeDetail(t *testing.T) {
	raw := `GET https://user:pw@example.com/v1/chat?api_key=secret failed with Bearer sk-live-123456`
	got := redactedProbeDetail(raw)
	for _, leak := range []string{"user:pw", "api_key=secret", "sk-live-123456"} {
		if strings.Contains(got, leak) {
			t.Fatalf("detail %q still contains %q", got, leak)
		}
	}
	if !strings.Contains(got, "<redacted>") {
		t.Fatalf("detail %q carries no redaction marker", got)
	}
	if got := redactedProbeDetail(strings.Repeat("长", connectivityProbeDetailRunes+50)); len([]rune(got)) > connectivityProbeDetailRunes+1 {
		t.Fatalf("detail not capped: %d runes", len([]rune(got)))
	}
}
