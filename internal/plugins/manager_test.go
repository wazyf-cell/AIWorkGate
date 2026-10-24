package plugins

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aiworkgate/internal/config"
	"aiworkgate/internal/logging"
	"aiworkgate/internal/model"
	"aiworkgate/internal/secrets"
	"aiworkgate/internal/store"
)

// testTempDir is t.TempDir with a retrying cleanup. A plugin child killed a
// moment ago can still hold a handle on its working directory, which makes
// RemoveAll fail with "The directory is not empty" on Windows.
func testTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "aiworkgate-plugin-test-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() {
		for i := 0; i < 50; i++ {
			if err := os.RemoveAll(dir); err == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		_ = os.RemoveAll(dir)
	})
	return dir
}

func testConfig() config.Config {
	c := config.Default()
	c.Plugins.Concurrency = 2
	return c
}

// newTestManager builds a manager backed by a temporary data directory.
func newTestManager(t *testing.T) *Manager {
	t.Helper()
	return newTestManagerConfig(t, testConfig())
}

// newTestManagerConfig is newTestManager with a caller-supplied configuration
// (e.g. plugins.trusted_keys).
func newTestManagerConfig(t *testing.T, cfg config.Config) *Manager {
	t.Helper()
	dir := testTempDir(t)

	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	sec, err := secrets.Open(dir)
	if err != nil {
		_ = st.Close()
		t.Fatalf("open secrets: %v", err)
	}
	hostLog, err := logging.NewLogger(dir)
	if err != nil {
		_ = sec.Close()
		_ = st.Close()
		t.Fatalf("open host log: %v", err)
	}
	pluginLog, err := logging.NewPluginLogWriter(dir)
	if err != nil {
		_ = hostLog.Close()
		_ = sec.Close()
		_ = st.Close()
		t.Fatalf("open plugin log: %v", err)
	}

	mgr := New(Options{
		DataDir:   dir,
		Config:    func() config.Config { return cfg },
		Store:     st,
		Secrets:   sec,
		Log:       hostLog,
		PluginLog: pluginLog,
	})
	t.Cleanup(func() {
		_ = mgr.Close()
		_ = pluginLog.Close()
		_ = hostLog.Close()
		_ = sec.Close()
		_ = st.Close()
	})
	return mgr
}

// writePluginSource writes a manifest plus entry file into a fresh directory.
func writePluginSource(t *testing.T, id, startupMode string, exe []byte) string {
	t.Helper()
	dir := testTempDir(t)
	m := baseManifest()
	m.ID = id
	m.Name = "Plugin " + id
	m.Startup.Mode = startupMode
	m.Entry.Exe = id + "-plugin.exe"
	writeManifestFile(t, dir, m)
	writeTestFile(t, filepath.Join(dir, m.Entry.Exe), exe)
	return dir
}

func writeManifestFile(t *testing.T, dir string, m model.Manifest) {
	t.Helper()
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	writeTestFile(t, filepath.Join(dir, ManifestFileName), data)
}

func writeTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func settingsRow(t *testing.T, mgr *Manager, id string) (store.PluginSetting, bool) {
	t.Helper()
	rows, err := mgr.opts.Store.ListPluginSettings()
	if err != nil {
		t.Fatalf("list plugin settings: %v", err)
	}
	for _, row := range rows {
		if row.PluginID == id {
			return row, true
		}
	}
	return store.PluginSetting{}, false
}

var dummyExe = []byte("MZ placeholder entry executable used by unit tests")

func TestDiscoverMergesSettings(t *testing.T) {
	mgr := newTestManager(t)
	if err := mgr.InstallDir(writePluginSource(t, "alpha", model.StartupOnDemand, dummyExe)); err != nil {
		t.Fatalf("install alpha: %v", err)
	}
	if err := mgr.InstallDir(writePluginSource(t, "demo", model.StartupOnDemand, dummyExe)); err != nil {
		t.Fatalf("install demo: %v", err)
	}
	if err := mgr.opts.Store.UpsertPluginSettings("demo", false, model.StartupAlways, 5, "08:30", true, 1234); err != nil {
		t.Fatalf("upsert settings: %v", err)
	}
	if err := mgr.Discover(); err != nil {
		t.Fatalf("discover: %v", err)
	}

	list := mgr.List()
	if len(list) != 2 {
		t.Fatalf("list length = %d, want 2", len(list))
	}
	if list[0].Manifest.ID != "alpha" || list[1].Manifest.ID != "demo" {
		t.Fatalf("list order = %q,%q; want alpha,demo", list[0].Manifest.ID, list[1].Manifest.ID)
	}
	demo, ok := mgr.Get("demo")
	if !ok {
		t.Fatal("demo not found after discover")
	}
	if demo.Enabled {
		t.Error("demo should be disabled from the store row")
	}
	if demo.InstalledAt != 1234 {
		t.Errorf("installed_at = %d, want 1234", demo.InstalledAt)
	}
	if demo.Running || demo.Healthy || demo.Failed {
		t.Errorf("demo runtime state = running:%v healthy:%v failed:%v, want all false", demo.Running, demo.Healthy, demo.Failed)
	}
	if got := mgr.Concurrency("demo"); got != 5 {
		t.Errorf("concurrency = %d, want 5", got)
	}
	alpha, _ := mgr.Get("alpha")
	if !alpha.Enabled {
		t.Error("alpha should default to enabled")
	}
}

func TestDiscoverCorruptManifestNotFatal(t *testing.T) {
	mgr := newTestManager(t)
	if err := mgr.InstallDir(writePluginSource(t, "good", model.StartupOnDemand, dummyExe)); err != nil {
		t.Fatalf("install good: %v", err)
	}
	root := mgr.pluginsDir()
	writeTestFile(t, filepath.Join(root, "broken", ManifestFileName), []byte("{not json"))
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatalf("mkdir empty: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".staging-leftover"), 0o755); err != nil {
		t.Fatalf("mkdir staging: %v", err)
	}

	if err := mgr.Discover(); err != nil {
		t.Fatalf("discover must not fail on broken plugins: %v", err)
	}
	if _, ok := mgr.Get("good"); !ok {
		t.Fatal("valid plugin disappeared")
	}
	broken, ok := mgr.Get("broken")
	if !ok {
		t.Fatal("broken plugin should still be listed")
	}
	if !broken.Failed || broken.LastError == "" {
		t.Errorf("broken status = failed:%v error:%q, want failed with a message", broken.Failed, broken.LastError)
	}
	empty, ok := mgr.Get("empty")
	if !ok || !empty.Failed {
		t.Fatalf("empty dir should be listed as failed, got %+v", empty)
	}
	if _, ok := mgr.Get(".staging-leftover"); ok {
		t.Error("staging directories must be ignored")
	}
}

func TestIdleReaperSelection(t *testing.T) {
	mgr := newTestManager(t)
	if err := mgr.InstallDir(writePluginSource(t, "lazy", model.StartupOnDemand, dummyExe)); err != nil {
		t.Fatalf("install lazy: %v", err)
	}
	now := time.Now()
	mgr.mu.Lock()
	p := mgr.plugins["lazy"]
	p.running = true
	p.lastUsed = now.Add(-11 * time.Minute)
	p.idleTimeout = 10 * time.Minute
	mgr.mu.Unlock()

	if got := mgr.idleIDs(now); len(got) != 1 || got[0] != "lazy" {
		t.Fatalf("idleIDs = %v, want [lazy]", got)
	}
	mgr.Touch("lazy")
	if got := mgr.idleIDs(now); len(got) != 0 {
		t.Fatalf("idleIDs after touch = %v, want none", got)
	}

	// A call in flight must survive a manifest idle_timeout_s shorter than the
	// call: the refcount, not lastUsed alone, keeps the plugin alive.
	mgr.mu.Lock()
	p.lastUsed = now.Add(-11 * time.Minute)
	mgr.mu.Unlock()
	release := mgr.BeginCall("lazy")
	if got := mgr.idleIDs(now); len(got) != 0 {
		t.Fatalf("idleIDs with an in-flight call = %v, want none", got)
	}
	release()
	if got := mgr.idleIDs(now); len(got) != 1 || got[0] != "lazy" {
		t.Fatalf("idleIDs after the call returned = %v, want [lazy]", got)
	}

	mgr.mu.Lock()
	p.mode = model.StartupAlways
	mgr.mu.Unlock()
	mgr.mu.Lock()
	p.lastUsed = now.Add(-time.Hour)
	mgr.mu.Unlock()
	if got := mgr.idleIDs(now); len(got) != 0 {
		t.Fatalf("always-mode plugin must not be reaped, got %v", got)
	}
}

func TestFailureStateExpiresAndRetries(t *testing.T) {
	mgr := newTestManager(t)
	mgr.failureWindow = 10 * time.Minute
	if err := mgr.InstallDir(writePluginSource(t, "demo", model.StartupOnDemand, dummyExe)); err != nil {
		t.Fatalf("install: %v", err)
	}
	mgr.mu.Lock()
	p := mgr.plugins["demo"]
	p.failed = true
	p.failedAt = time.Now()
	p.lastError = "crash loop is stuck"
	mgr.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := mgr.EnsureRunning(ctx, "demo")
	if err == nil || !strings.Contains(err.Error(), "crash loop is stuck") {
		t.Fatalf("EnsureRunning inside the window = %v, want the stored failure", err)
	}
	if status, _ := mgr.Get("demo"); !status.Failed {
		t.Fatal("failure inside the window should stay visible")
	}

	// Age the failure past the window without sleeping: the state must expire
	// and the next attempt must actually retry instead of replaying the error.
	mgr.mu.Lock()
	p.failedAt = time.Now().Add(-11 * time.Minute)
	mgr.mu.Unlock()
	if status, _ := mgr.Get("demo"); status.Failed {
		t.Fatal("failure older than the window should have expired")
	}
	retryCtx, retryCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer retryCancel()
	err = mgr.EnsureRunning(retryCtx, "demo")
	if err == nil {
		t.Fatal("the placeholder entry cannot start, so the retry must fail")
	}
	if strings.Contains(err.Error(), "crash loop is stuck") {
		t.Fatalf("EnsureRunning replayed the expired failure: %v", err)
	}
}

func TestEndpointHidesUnhealthyPlugin(t *testing.T) {
	mgr := &Manager{plugins: map[string]*pluginState{
		"p": {id: "p", enabled: true, running: true, port: 4321, token: "tok",
			lastError: "health check failed: dial tcp: connection refused"},
	}}
	if _, _, ok := mgr.Endpoint("p"); ok {
		t.Fatal("a running but unhealthy plugin must not expose an endpoint")
	}
	if _, _, err := mgr.endpoint("p"); err == nil || !strings.Contains(err.Error(), "unhealthy") {
		t.Fatalf("endpoint error = %v, want an unhealthy explanation", err)
	}

	// An in-flight start is still dialable: EnsureRunning is waiting on it.
	mgr.plugins["p"].starting = true
	if base, _, ok := mgr.Endpoint("p"); !ok || base != "http://127.0.0.1:4321" {
		t.Fatalf("Endpoint during start = %q,%v; want the port", base, ok)
	}

	mgr.plugins["p"].starting = false
	mgr.plugins["p"].healthy = true
	if _, _, ok := mgr.Endpoint("p"); !ok {
		t.Fatal("a healthy plugin must expose its endpoint")
	}

	mgr.plugins["p"].healthy = false
	mgr.plugins["p"].running = false
	if _, _, ok := mgr.Endpoint("p"); ok {
		t.Fatal("a stopped plugin must not expose an endpoint")
	}
}

func TestRuntimeCadenceDefaults(t *testing.T) {
	mgr := New(Options{})
	defer func() { _ = mgr.Close() }()
	if mgr.healthEvery != 10*time.Second {
		t.Errorf("health every = %s, want 10s", mgr.healthEvery)
	}
	if healthFailureStrikes != 2 {
		t.Errorf("health strikes = %d, want 2", healthFailureStrikes)
	}
	if coldStartTimeout != mgr.startTimeout {
		t.Errorf("cold start wait = %s, supervisor start timeout = %s; want them aligned",
			coldStartTimeout, mgr.startTimeout)
	}
}

func TestStartAllLeavesOnDemandStopped(t *testing.T) {
	mgr := newTestManager(t)
	if err := mgr.InstallDir(writePluginSource(t, "lazy", model.StartupOnDemand, dummyExe)); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := mgr.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	status, _ := mgr.Get("lazy")
	if status.Running {
		t.Fatal("on-demand plugin must stay stopped after StartAll")
	}
	if _, _, ok := mgr.Endpoint("lazy"); ok {
		t.Fatal("endpoint must not be exposed while stopped")
	}
	if err := mgr.Start("lazy"); err != nil {
		t.Fatalf("explicit Start: %v", err)
	}
	if err := mgr.Stop("lazy"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestConcurrencyDefaultsAndClamp(t *testing.T) {
	mgr := newTestManager(t)
	src := writePluginSource(t, "demo", model.StartupOnDemand, dummyExe)
	if err := mgr.InstallDir(src); err != nil {
		t.Fatalf("install: %v", err)
	}
	if got := mgr.Concurrency("demo"); got != 2 {
		t.Fatalf("default concurrency = %d, want manifest suggestion 2", got)
	}
	if err := mgr.opts.Store.UpsertPluginSettings("demo", true, model.StartupOnDemand, 99, "09:00", false, 0); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if got := mgr.Concurrency("demo"); got != 32 {
		t.Fatalf("concurrency = %d, want clamp to 32", got)
	}
	if err := mgr.opts.Store.UpsertPluginSettings("demo", true, model.StartupOnDemand, 0, "09:00", false, 0); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if got := mgr.Concurrency("demo"); got != 2 {
		t.Fatalf("zero stored concurrency = %d, want fallback 2", got)
	}
	if got := mgr.Concurrency("missing"); got != 2 {
		t.Fatalf("config default for unknown plugin = %d, want 2", got)
	}
}

func TestEnableDisablePersist(t *testing.T) {
	mgr := newTestManager(t)
	if err := mgr.InstallDir(writePluginSource(t, "demo", model.StartupOnDemand, dummyExe)); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := mgr.Disable("demo"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	row, ok := settingsRow(t, mgr, "demo")
	if !ok || row.Enabled {
		t.Fatalf("settings after disable = %+v (ok=%v), want disabled", row, ok)
	}
	if err := mgr.Enable("demo"); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	row, _ = settingsRow(t, mgr, "demo")
	if !row.Enabled {
		t.Fatal("settings after enable should be enabled")
	}
	if err := mgr.Stop("demo"); err != nil {
		t.Fatalf("Stop after Enable: %v", err)
	}
}

func TestEnableDisableKeepsFollowGlobalOverrides(t *testing.T) {
	// Regression: Enable/Disable used to persist the *resolved* settings, so a
	// plugin set to 跟随全局 came back with a pinned per-plugin override.
	mgr := newTestManager(t)
	if err := mgr.InstallDir(writePluginSource(t, "demo", model.StartupOnDemand, dummyExe)); err != nil {
		t.Fatalf("install: %v", err)
	}
	var strict *bool // NULL = follow the global value
	if err := mgr.opts.Store.UpdatePluginSettings("demo", true, model.StartupOnDemand, 0, "09:00", strict, 0); err != nil {
		t.Fatalf("clear overrides: %v", err)
	}
	if err := mgr.Disable("demo"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if err := mgr.Enable("demo"); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	view, ok, err := mgr.opts.Store.GetPluginSettingsView("demo")
	if err != nil || !ok {
		t.Fatalf("settings view after toggling = %+v (ok=%v, err=%v)", view, ok, err)
	}
	if !view.Enabled {
		t.Error("enabled = false after Enable")
	}
	if view.Concurrency != 0 {
		t.Errorf("concurrency = %d after toggling, want 0 (跟随全局)", view.Concurrency)
	}
	if view.StrictSet {
		t.Errorf("strict override = %v after toggling, want the column to stay NULL", view.Strict)
	}
	if err := mgr.Stop("demo"); err != nil {
		t.Fatalf("Stop after Enable: %v", err)
	}
}

func TestAccountsRequiresRunningPlugin(t *testing.T) {
	mgr := newTestManager(t)
	if err := mgr.InstallDir(writePluginSource(t, "demo", model.StartupOnDemand, dummyExe)); err != nil {
		t.Fatalf("install: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// The placeholder entry is not a real executable; the call must fail
	// instead of hanging.
	if _, err := mgr.Accounts(ctx, "demo"); err == nil {
		t.Fatal("expected an error from a plugin that cannot start")
	}
	if _, err := mgr.Accounts(ctx, "unknown"); err == nil {
		t.Fatal("expected ErrNotInstalled for an unknown plugin")
	}
}
