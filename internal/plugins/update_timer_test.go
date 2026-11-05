package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"aiworkgate/internal/config"
	"aiworkgate/internal/market"
	"aiworkgate/internal/store"
)

// recordingPreparer is the market.Installer seam: it returns a fixed prepared
// directory without any network.
type recordingPreparer struct {
	prepared *market.Prepared
	err      error
	calls    int
}

func (f *recordingPreparer) Prepare(context.Context, string, market.Options) (*market.Prepared, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.prepared, nil
}

func (f *recordingPreparer) Cleanup(*market.Prepared) error { return nil }

// recordingUpdater is the UpdateDirWithRollback seam.
type recordingUpdater struct {
	mu         sync.Mutex
	calls      []string // "id|srcDir"
	err        error
	rolledBack bool
}

func (f *recordingUpdater) UpdateDirWithRollback(_ context.Context, id, srcDir string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, id+"|"+srcDir)
	return f.rolledBack, f.err
}

func (f *recordingUpdater) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// timerRegistry serves npm metadata (no tarball needed for CheckUpdate).
func timerRegistry(t *testing.T, pkg, latest string, versions ...string) *market.Registry {
	t.Helper()
	vs := map[string]any{}
	for _, v := range versions {
		vs[v] = map[string]any{
			"name": pkg, "version": v,
			"dist": map[string]any{"shasum": "x", "tarball": "http://127.0.0.1/x/" + v + ".tgz"},
		}
	}
	body := map[string]any{"name": pkg, "dist-tags": map[string]string{"latest": latest}, "versions": vs}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	reg, err := market.NewRegistry(srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func timerWithStore(t *testing.T, policy string) (*Manager, *store.Store, *UpdateTimer) {
	t.Helper()
	dataDir := t.TempDir()
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr := New(Options{DataDir: t.TempDir()})
	reg := timerRegistry(t, "pkg", "1.2.0", "1.0.0", "1.2.0")
	timer := NewUpdateTimer(mgr, st, func() config.Config {
		return config.Config{Plugins: config.PluginsConfig{
			MarketMirror: reg.Base(), MarketUpdatePolicy: policy,
		}}
	})
	if err := st.UpsertMarketInstall(store.MarketInstall{
		PluginID: "p1", Package: "pkg", Source: "npm", Version: "1.0.0",
	}); err != nil {
		t.Fatalf("UpsertMarketInstall: %v", err)
	}
	return mgr, st, timer
}

func waitForScan(t *testing.T, timer *UpdateTimer) UpdateScan {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if scan, ok := timer.LastScan(); ok {
			return scan
		}
		if time.Now().After(deadline) {
			t.Fatal("timer never produced a scan")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ① 定时触发一次 CheckUpdate（短间隔代替真实 6h，fake clock 由 interval 注入）。
func TestUpdateTimerFiresAtInterval(t *testing.T) {
	_, _, timer := timerWithStore(t, config.MarketUpdatePolicyPrompt)
	timer.interval = func() time.Duration { return 15 * time.Millisecond }

	ctx, cancel := context.WithCancel(context.Background())
	go timer.Run(ctx)
	scan := waitForScan(t, timer)
	cancel()

	if scan.Policy != config.MarketUpdatePolicyPrompt {
		t.Fatalf("policy = %q, want prompt", scan.Policy)
	}
	if len(scan.Available) != 1 || scan.Available[0].To != "1.2.0" || scan.Available[0].From != "1.0.0" {
		t.Fatalf("available = %+v, want one 1.0.0 -> 1.2.0 diff", scan.Available)
	}
	if len(scan.Applied) != 0 {
		t.Fatalf("applied = %+v, want none under prompt", scan.Applied)
	}
}

// ② prompt 只记录可用更新，绝不自动装。
func TestUpdateTimerPromptNeverInstalls(t *testing.T) {
	_, st, timer := timerWithStore(t, config.MarketUpdatePolicyPrompt)
	preparer := &recordingPreparer{}
	updater := &recordingUpdater{}
	timer.preparer = preparer
	timer.updater = updater

	scan, err := timer.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(scan.Available) != 1 || scan.Available[0].To != "1.2.0" {
		t.Fatalf("available = %+v", scan.Available)
	}
	if preparer.calls != 0 || len(updater.snapshot()) != 0 {
		t.Fatalf("prompt must not prepare or update: preparer calls=%d updater=%v",
			preparer.calls, updater.snapshot())
	}
	rows, err := st.ListMarketInstalls()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Version != "1.0.0" {
		t.Fatalf("store version changed under prompt: %+v", rows)
	}
}

// ③ auto 显式开启时走 UpdateDirWithRollback（不许裸 UpdateDir），且失败落 Errors、
// 记录不前进。
func TestUpdateTimerAutoAppliesThroughRollback(t *testing.T) {
	_, st, timer := timerWithStore(t, config.MarketUpdatePolicyAuto)
	root := t.TempDir()
	preparer := &recordingPreparer{prepared: &market.Prepared{
		Root:   root,
		Record: market.InstallRecord{Version: "1.2.0", SHA256: "abc123"},
	}}
	updater := &recordingUpdater{}
	timer.preparer = preparer
	timer.updater = updater

	scan, err := timer.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(scan.Applied) != 1 || scan.Applied[0].PluginID != "p1" ||
		scan.Applied[0].From != "1.0.0" || scan.Applied[0].To != "1.2.0" {
		t.Fatalf("applied = %+v", scan.Applied)
	}
	if calls := updater.snapshot(); len(calls) != 1 || calls[0] != "p1|"+root {
		t.Fatalf("updater calls = %v, want exactly p1|<root>", calls)
	}
	rows, err := st.ListMarketInstalls()
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Version != "1.2.0" || rows[0].SHA256 != "abc123" {
		t.Fatalf("record after auto apply = %+v", rows[0])
	}

	// 回退原语失败：更新不落地、版本不前进、错误进 scan.Errors。
	if err := st.UpsertMarketInstall(store.MarketInstall{
		PluginID: "p1", Package: "pkg", Source: "npm", Version: "1.0.0",
	}); err != nil {
		t.Fatal(err)
	}
	updater.err = errors.New("rollback also failed")
	updater.rolledBack = true
	preparer.prepared = &market.Prepared{Root: root, Record: market.InstallRecord{Version: "1.2.0", SHA256: "def456"}}

	scan, err = timer.Tick(context.Background())
	if err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	msg, got := scan.Errors["p1"]
	if !got || !strings.Contains(msg, "rollback also failed") || !strings.Contains(msg, "rolled back: true") {
		t.Fatalf("errors = %v, want the rollback failure named", scan.Errors)
	}
	rows, err = st.ListMarketInstalls()
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Version != "1.0.0" {
		t.Fatalf("record must not advance after a failed apply, got %+v", rows[0])
	}
}
