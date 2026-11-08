package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"aiworkgate/internal/config"
	"aiworkgate/internal/market"
	"aiworkgate/internal/store"
	"aiworkgate/internal/webui"
)

// fakeMarketUpdateRegistry serves npm metadata for one package (no tarball
// needed: CheckUpdate only reads versions).
func fakeMarketUpdateRegistry(t *testing.T, pkg, latest string, versions ...string) *httptest.Server {
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
	return srv
}

// fakeUpdatePreparer is the market.Installer seam: it returns a fixed prepared
// directory without any network.
type fakeUpdatePreparer struct {
	prepared *market.Prepared
	err      error
	calls    int
	lastSpec string
}

func (f *fakeUpdatePreparer) Prepare(_ context.Context, spec string, _ market.Options) (*market.Prepared, error) {
	f.calls++
	f.lastSpec = spec
	if f.err != nil {
		return nil, f.err
	}
	return f.prepared, nil
}

func (f *fakeUpdatePreparer) Cleanup(*market.Prepared) error { return nil }

// fakeUpdater is the UpdateDirWithRollback seam.
type fakeUpdater struct {
	rolledBack bool
	err        error
	calls      []string
}

func (f *fakeUpdater) UpdateDirWithRollback(_ context.Context, id, srcDir string) (bool, error) {
	f.calls = append(f.calls, id+"|"+srcDir)
	return f.rolledBack, f.err
}

func marketUpdateApp(t *testing.T, mirror string) *App {
	t.Helper()
	dataDir := t.TempDir()
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	a := &App{
		cfg:     config.Config{Plugins: config.PluginsConfig{MarketMirror: mirror}},
		cfgMu:   sync.RWMutex{},
		dataDir: dataDir,
		store:   st,
	}
	return a
}

func installMarketRow(t *testing.T, st *store.Store, id, pkg, version string) {
	t.Helper()
	if err := st.UpsertMarketInstall(store.MarketInstall{PluginID: id, Package: pkg, Source: "npm", Version: version}); err != nil {
		t.Fatalf("UpsertMarketInstall: %v", err)
	}
}

func marketVersion(t *testing.T, st *store.Store, id string) store.MarketInstall {
	t.Helper()
	rows, err := st.ListMarketInstalls()
	if err != nil {
		t.Fatalf("ListMarketInstalls: %v", err)
	}
	for _, row := range rows {
		if row.PluginID == id {
			return row
		}
	}
	t.Fatalf("market row for %q missing", id)
	return store.MarketInstall{}
}

// ① 成功路径：Prepare → UpdateDirWithRollback → 前进版本。
func TestMarketUpdateAppliesAndAdvances(t *testing.T) {
	srv := fakeMarketUpdateRegistry(t, "pkg", "1.2.0", "1.0.0", "1.2.0")
	a := marketUpdateApp(t, srv.URL)
	installMarketRow(t, a.store, "p1", "pkg", "1.0.0")

	root := t.TempDir()
	preparer := &fakeUpdatePreparer{prepared: &market.Prepared{
		Root:   root,
		Record: market.InstallRecord{Version: "1.2.0", SHA256: "abc123"},
	}}
	updater := &fakeUpdater{}
	restoreSeams(t, preparer, updater)

	res, err := a.marketUpdate("p1")
	if err != nil {
		t.Fatalf("marketUpdate: %v", err)
	}
	if !res.OK || res.From != "1.0.0" || res.To != "1.2.0" || res.RolledBack {
		t.Fatalf("result = %+v", res)
	}
	if preparer.calls != 1 || preparer.lastSpec != "pkg@1.2.0" {
		t.Fatalf("preparer calls = %d spec = %q, want 1 / pkg@1.2.0", preparer.calls, preparer.lastSpec)
	}
	if len(updater.calls) != 1 || updater.calls[0] != "p1|"+root {
		t.Fatalf("updater calls = %v, want exactly p1|<root>", updater.calls)
	}
	row := marketVersion(t, a.store, "p1")
	if row.Version != "1.2.0" || row.SHA256 != "abc123" {
		t.Fatalf("record after update = %+v, want 1.2.0/abc123", row)
	}
}

// ② 回退原语失败：错误报 rolled back，版本不前进。
func TestMarketUpdateRollbackReportsAndKeepsVersion(t *testing.T) {
	srv := fakeMarketUpdateRegistry(t, "pkg", "1.2.0", "1.0.0", "1.2.0")
	a := marketUpdateApp(t, srv.URL)
	installMarketRow(t, a.store, "p1", "pkg", "1.0.0")

	preparer := &fakeUpdatePreparer{prepared: &market.Prepared{
		Root:   t.TempDir(),
		Record: market.InstallRecord{Version: "1.2.0", SHA256: "abc123"},
	}}
	updater := &fakeUpdater{rolledBack: true, err: errors.New("health check failed")}
	restoreSeams(t, preparer, updater)

	res, err := a.marketUpdate("p1")
	if err == nil {
		t.Fatalf("rollback failure must surface as an error, got %+v", res)
	}
	if !strings.Contains(err.Error(), "rolled back: true") {
		t.Fatalf("error does not name the rollback state: %v", err)
	}
	if res.OK {
		t.Fatalf("result must not claim success: %+v", res)
	}
	row := marketVersion(t, a.store, "p1")
	if row.Version != "1.0.0" {
		t.Fatalf("record advanced after failed apply: %+v", row)
	}
}

// ③ 未知 id → ErrPluginNotFound；无更新 / 非 registry 包 → ErrInvalidRequest。
func TestMarketUpdateUnknownAndNoUpdate(t *testing.T) {
	srv := fakeMarketUpdateRegistry(t, "pkg", "1.2.0", "1.0.0", "1.2.0")
	a := marketUpdateApp(t, srv.URL)

	// Unknown plugin.
	if _, err := a.marketUpdate("ghost"); !errors.Is(err, webui.ErrPluginNotFound) {
		t.Fatalf("unknown plugin error = %v, want ErrPluginNotFound", err)
	}

	// Installed but already at latest.
	installMarketRow(t, a.store, "p1", "pkg", "1.2.0")
	if _, err := a.marketUpdate("p1"); !errors.Is(err, webui.ErrInvalidRequest) {
		t.Fatalf("no-update error = %v, want ErrInvalidRequest", err)
	}

	// GitHub-shaped package has no registry entry.
	installMarketRow(t, a.store, "gh1", "owner/repo", "1.0.0")
	if _, err := a.marketUpdate("gh1"); !errors.Is(err, webui.ErrInvalidRequest) {
		t.Fatalf("non-registry package error = %v, want ErrInvalidRequest", err)
	}

	// Empty id.
	if _, err := a.marketUpdate("  "); !errors.Is(err, webui.ErrInvalidRequest) {
		t.Fatalf("empty id error = %v, want ErrInvalidRequest", err)
	}
}

// restoreSeams injects the preparer/updater seams and restores them after the
// test.
func restoreSeams(t *testing.T, preparer marketUpdatePreparer, updater marketUpdater) {
	t.Helper()
	prevPreparer, prevUpdater := marketUpdatePreparerFor, marketUpdaterFor
	marketUpdatePreparerFor = func(*App) (marketUpdatePreparer, error) { return preparer, nil }
	marketUpdaterFor = func(*App) marketUpdater { return updater }
	t.Cleanup(func() {
		marketUpdatePreparerFor, marketUpdaterFor = prevPreparer, prevUpdater
	})
}
