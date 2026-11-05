package webui

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aiworkgate/internal/config"
	"aiworkgate/internal/market"
	"aiworkgate/internal/plugins"
	"aiworkgate/internal/store"
)

// ---------------------------------------------------------------------------
// handlers, with injected fake callbacks
// ---------------------------------------------------------------------------

func marketOptions(mutate func(*Options)) Options {
	opts := Options{SessionToken: NewSessionToken()}
	if mutate != nil {
		mutate(&opts)
	}
	return opts
}

func TestMarketSearchRoute(t *testing.T) {
	var gotQ string
	var gotLimit int
	h := New(marketOptions(func(o *Options) {
		o.MarketSearch = func(query string, limit int) (any, error) {
			gotQ, gotLimit = query, limit
			return []map[string]any{{
				"name": "opencode-trae-auth", "version": "0.2.30", "description": "trae auth plugin",
			}}, nil
		}
	}))

	rec := doRequest(t, h, http.MethodGet, "/api/market/search?q=trae", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("search = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var payload struct {
		Results []map[string]any `json:"results"`
	}
	decodeJSONBody(t, rec, &payload)
	if len(payload.Results) != 1 || payload.Results[0]["name"] != "opencode-trae-auth" {
		t.Fatalf("results = %v", payload.Results)
	}
	if gotQ != "trae" || gotLimit != 20 {
		t.Fatalf("callback saw q=%q limit=%d, want trae/20", gotQ, gotLimit)
	}

	if rec := doRequest(t, h, http.MethodGet, "/api/market/search", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing q = %d, want 400", rec.Code)
	}
	for _, bad := range []string{"?q=trae&limit=0", "?q=trae&limit=abc", "?q=trae&limit=500"} {
		if rec := doRequest(t, h, http.MethodGet, "/api/market/search"+bad, ""); rec.Code != http.StatusBadRequest {
			t.Fatalf("limit %q = %d, want 400", bad, rec.Code)
		}
	}
}

func TestMarketSearchErrors(t *testing.T) {
	cases := []struct {
		name string
		cb   func(string, int) (any, error)
		want int
	}{
		{"nil callback", nil, http.StatusNotImplemented},
		{"invalid request", func(string, int) (any, error) { return nil, fmt.Errorf("%w: bad spec", ErrInvalidRequest) }, http.StatusBadRequest},
		{"host failure", func(string, int) (any, error) { return nil, errors.New("registry down") }, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := New(marketOptions(func(o *Options) { o.MarketSearch = tc.cb }))
			rec := doRequest(t, h, http.MethodGet, "/api/market/search?q=x", "")
			if rec.Code != tc.want {
				t.Fatalf("code = %d, want %d (body %s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestMarketInstallRoute(t *testing.T) {
	var gotSpec, gotSource, gotLocal string
	h := New(marketOptions(func(o *Options) {
		o.MarketInstall = func(spec, source, localPath string) (any, error) {
			gotSpec, gotSource, gotLocal = spec, source, localPath
			return map[string]any{"ok": true, "id": "opencode-trae-auth", "version": "0.2.30"}, nil
		}
	}))

	rec := doRequest(t, h, http.MethodPost, "/api/market/install",
		`{"spec":"@magpie-community/opencode-trae-auth","source":"npm"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("install = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var payload map[string]any
	decodeJSONBody(t, rec, &payload)
	if payload["ok"] != true || payload["id"] != "opencode-trae-auth" || payload["version"] != "0.2.30" {
		t.Fatalf("install payload = %v", payload)
	}
	if gotSpec != "@magpie-community/opencode-trae-auth" || gotSource != "npm" || gotLocal != "" {
		t.Fatalf("callback saw spec=%q source=%q local=%q", gotSpec, gotSource, gotLocal)
	}

	// Contract violations and wrong methods.
	if rec := doRequest(t, h, http.MethodPost, "/api/market/install", `{"source":"npm"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing spec = %d, want 400", rec.Code)
	}
	if rec := doRequest(t, h, http.MethodPost, "/api/market/install", `{not json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json = %d, want 400", rec.Code)
	}
	if rec := doRequest(t, h, http.MethodGet, "/api/market/install", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET install = %d, want 405", rec.Code)
	}
}

func TestMarketInstallUnavailable(t *testing.T) {
	h := New(marketOptions(nil))
	rec := doRequest(t, h, http.MethodPost, "/api/market/install", `{"spec":"foo","source":"npm"}`)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("nil callback = %d, want 501", rec.Code)
	}
}

func TestMarketUpdatesRoute(t *testing.T) {
	h := New(marketOptions(func(o *Options) {
		o.MarketUpdates = func() (any, error) {
			return []map[string]any{{"id": "opencode-trae-auth", "from": "0.2.1", "to": "0.2.30", "available": true}}, nil
		}
	}))
	rec := doRequest(t, h, http.MethodGet, "/api/market/updates", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("updates = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var payload struct {
		Updates []map[string]any `json:"updates"`
	}
	decodeJSONBody(t, rec, &payload)
	if len(payload.Updates) != 1 || payload.Updates[0]["id"] != "opencode-trae-auth" || payload.Updates[0]["available"] != true {
		t.Fatalf("updates = %v", payload.Updates)
	}
}

// ---------------------------------------------------------------------------
// full chain: market prepare -> plugins.InstallDirOpts -> store record,
// driven through the real HTTP routes with a fake httptest registry.
// ---------------------------------------------------------------------------

func TestMarketInstallFullChain(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	mgr := plugins.New(plugins.Options{
		DataDir: dataDir,
		Store:   st,
		Config:  func() config.Config { return config.Config{} },
	})

	bridge := filepath.Join(t.TempDir(), "magpie-bridge.exe")
	if err := os.WriteFile(bridge, []byte("MZ fake bridge"), 0o755); err != nil {
		t.Fatal(err)
	}

	tarball := marketTGZ(t, []marketTarEntry{{name: "package/index.mjs", content: "export const plugin = {};\n"}})
	installReg, _ := serveRegistry(t, "@magpie-community/opencode-trae-auth", "0.2.30",
		map[string]string{"0.2.1": "x", "0.2.30": "y"}, tarball)
	updateReg, _ := serveRegistry(t, "@magpie-community/opencode-trae-auth", "0.3.0",
		map[string]string{"0.2.30": "y", "0.3.0": "z"}, tarball)

	inst := market.NewInstaller(installReg, filepath.Join(dataDir, "scratch"))
	const pkg = "@magpie-community/opencode-trae-auth"
	const pluginID = "opencode-trae-auth"

	h := New(marketOptions(func(o *Options) {
		o.MarketSearch = func(query string, limit int) (any, error) {
			return []map[string]any{}, nil // stub: the chain test drives install/updates
		}
		o.MarketInstall = func(spec, source, localPath string) (any, error) {
			p, err := inst.Prepare(ctx, spec, market.Options{
				Registry:         installReg,
				FinalPluginDir:   filepath.Join(dataDir, "plugins", pluginID),
				BridgeExecutable: bridge,
			})
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
			}
			if err := mgr.InstallDirOpts(p.Root, plugins.InstallOpts{Enabled: false}); err != nil {
				_ = inst.Cleanup(p)
				return nil, err
			}
			_ = inst.Cleanup(p)
			rec := p.Record
			if err := st.UpsertMarketInstall(store.MarketInstall{
				PluginID: pluginID, Package: pkg, Source: "npm",
				Version: rec.Version, SHA256: rec.SHA256,
			}); err != nil {
				return nil, err
			}
			return map[string]any{"ok": true, "id": pluginID, "version": rec.Version}, nil
		}
		o.MarketUpdates = func() (any, error) {
			rows, err := st.ListMarketInstalls()
			if err != nil {
				return nil, err
			}
			out := make([]map[string]any, 0, len(rows))
			for _, row := range rows {
				info, err := market.CheckUpdate(ctx, updateReg, row.Package, row.Version, market.UpdatePolicyPrompt)
				if err != nil {
					return nil, err
				}
				out = append(out, map[string]any{
					"id": row.PluginID, "from": info.From, "to": info.To, "available": info.Available,
				})
			}
			return out, nil
		}
	}))

	// ① Install through the wire: POST /api/market/install.
	rec := doRequest(t, h, http.MethodPost, "/api/market/install",
		fmt.Sprintf(`{"spec":%q,"source":"npm"}`, pkg))
	if rec.Code != http.StatusOK {
		t.Fatalf("install = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var payload struct {
		OK      bool   `json:"ok"`
		ID      string `json:"id"`
		Version string `json:"version"`
	}
	decodeJSONBody(t, rec, &payload)
	if !payload.OK || payload.ID != pluginID || payload.Version != "0.2.30" {
		t.Fatalf("install payload = %+v", payload)
	}

	// ② The prepared tree landed in <dataDir>\plugins\<id>, disabled.
	manifestPath := filepath.Join(dataDir, "plugins", pluginID, "manifest.json")
	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatalf("installed manifest missing: %v", err)
	}
	entryPath := filepath.Join(dataDir, "plugins", pluginID, "magpie-bridge.exe")
	gotEntry, err := os.ReadFile(entryPath)
	if err != nil {
		t.Fatalf("deployed entry missing: %v", err)
	}
	if string(gotEntry) != "MZ fake bridge" {
		t.Fatalf("entry content = %q", gotEntry)
	}
	enabled, _, _, _, _, ok, err := st.GetPluginSettings(pluginID)
	if err != nil || !ok {
		t.Fatalf("GetPluginSettings: ok=%v err=%v", ok, err)
	}
	if enabled {
		t.Fatal("market install must land disabled (T3)")
	}
	rows, err := st.ListMarketInstalls()
	if err != nil {
		t.Fatalf("ListMarketInstalls: %v", err)
	}
	if len(rows) != 1 || rows[0].Version != "0.2.30" || rows[0].SHA256 == "" || rows[0].Package != pkg {
		t.Fatalf("market install rows = %+v", rows)
	}

	// ③ Update check through the wire: GET /api/market/updates.
	rec = doRequest(t, h, http.MethodGet, "/api/market/updates", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("updates = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var up struct {
		Updates []map[string]any `json:"updates"`
	}
	decodeJSONBody(t, rec, &up)
	if len(up.Updates) != 1 {
		t.Fatalf("updates = %v", up.Updates)
	}
	u := up.Updates[0]
	if u["id"] != pluginID || u["from"] != "0.2.30" || u["to"] != "0.3.0" || u["available"] != true {
		t.Fatalf("update diff = %v, want 0.2.30 -> 0.3.0 available", u)
	}
}

// ---------------------------------------------------------------------------
// local test helpers (the market package's own helpers are unexported)
// ---------------------------------------------------------------------------

type marketTarEntry struct {
	name    string
	content string
}

func marketTGZ(t *testing.T, entries []marketTarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// serveRegistry answers metadata for one package (any non-.tgz path) and the
// same tarball on every .tgz path, so the market client resolves and downloads
// against the fake server.
func serveRegistry(t *testing.T, pkg, latest string, versions map[string]string, tarball []byte) (*market.Registry, *httptest.Server) {
	t.Helper()
	meta := map[string]any{marketKeyName: pkg}
	distTags := map[string]string{"latest": latest}
	vs := map[string]any{}
	for v := range versions {
		vs[v] = map[string]any{
			"name": pkg, "version": v,
			"dist": map[string]any{"shasum": "x", "integrity": "sha512-y"},
		}
	}
	meta[marketKeyDistTags] = distTags
	meta[marketKeyVersions] = vs

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".tgz") {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(tarball)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(meta)
	}))
	// Point every dist tarball at the fake server, after srv is in scope; the
	// handler marshals meta lazily per request, so this lands before any fetch.
	for v, entry := range vs {
		e := entry.(map[string]any)
		d := e["dist"].(map[string]any)
		d["tarball"] = srv.URL + "/" + pkg + "-" + v + ".tgz"
		e["dist"] = d
		vs[v] = e
	}
	t.Cleanup(srv.Close)
	reg, err := market.NewRegistry(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return reg, srv
}

const (
	marketKeyName     = "name"
	marketKeyDistTags = "dist-tags"
	marketKeyVersions = "versions"
)
