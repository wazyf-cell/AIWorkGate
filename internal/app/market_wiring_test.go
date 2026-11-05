package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"aiworkgate/internal/config"
	"aiworkgate/internal/plugins"
	"aiworkgate/internal/store"
	"aiworkgate/internal/webui"
)

// fakeMarketSearchRegistry answers the npm /-/v1/search envelope so the real
// market.Registry client resolves against it (no real network).
func fakeMarketSearchRegistry(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/-/v1/search" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"objects": []map[string]any{
				{"package": map[string]any{"name": "opencode-trae-auth", "version": "0.2.30", "description": "trae auth plugin"}},
				{"package": map[string]any{"name": "opencode-workbuddy-auth", "version": "0.1.0", "description": "workbuddy auth plugin"}},
				{"package": map[string]any{"name": "opencode-example", "version": "9.9.9", "description": "example"}},
			},
			"total": 3,
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestMarketRoutesWiredInApp pins P2-4: with the app layer callbacks plugged via
// webOptions(), the /api/market/* routes answer 200 through the real client
// chain instead of the 501 that a nil callback produced before the wiring.
func TestMarketRoutesWiredInApp(t *testing.T) {
	srv := fakeMarketSearchRegistry(t)
	dataDir := t.TempDir()
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	a := &App{
		cfg:        config.Config{Plugins: config.PluginsConfig{MarketMirror: srv.URL}},
		cfgMu:      sync.RWMutex{},
		dataDir:    dataDir,
		store:      st,
		adminToken: "test-token",
	}
	a.pluginMgr = plugins.New(a.pluginOptions(dataDir, st, nil, nil, nil))
	t.Cleanup(func() { _ = a.pluginMgr.Close() })

	h := webui.New(a.webOptions())

	// Session handshake: POST /api/session?t=<launch token> → admin cookie.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/session?t=test-token", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("session = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "dsh_admin_session" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no admin session cookie issued")
	}
	request := func(method, target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, nil)
		req.AddCookie(cookie)
		r := httptest.NewRecorder()
		h.ServeHTTP(r, req)
		return r
	}

	// ① search：不再 501，走 httptest 假 registry 全链。
	rec = request(http.MethodGet, "/api/market/search?q=opencode&limit=5")
	if rec.Code != http.StatusOK {
		t.Fatalf("search = %d, want 200 (was 501 before wiring): %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode search body %q: %v", rec.Body.String(), err)
	}
	if len(payload.Results) != 3 || payload.Results[0]["name"] != "opencode-trae-auth" {
		t.Fatalf("search results = %v, want 3 hits headed by opencode-trae-auth", payload.Results)
	}

	// ② updates：空 market_installs 表 → 200 + 空列表（不是 501）。
	rec = request(http.MethodGet, "/api/market/updates")
	if rec.Code != http.StatusOK {
		t.Fatalf("updates = %d, want 200 (was 501 before wiring): %s", rec.Code, rec.Body.String())
	}
	var up struct {
		Updates []map[string]any `json:"updates"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &up); err != nil {
		t.Fatalf("decode updates body %q: %v", rec.Body.String(), err)
	}
	if len(up.Updates) != 0 {
		t.Fatalf("updates = %v, want empty (fresh store)", up.Updates)
	}
}

// TestBridgeExecutablePathPreference pins the exe resolution order:
// <dataDir>\bridge\magpie-bridge.exe first, dist\magpie-bridge.exe fallback
// (exe-dir dist, then cwd dist).
func TestBridgeExecutablePathPreference(t *testing.T) {
	root := t.TempDir()
	preferred := filepath.Join(root, "bridge", "magpie-bridge.exe")
	if err := os.MkdirAll(filepath.Dir(preferred), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(preferred, []byte("MZ"), 0o755); err != nil {
		t.Fatal(err)
	}

	// 把 dist fallback 放到测试进程的 cwd：实现里 cwd/dist 是回退候选之一。
	fixture := t.TempDir()
	fallback := filepath.Join(fixture, "dist", "magpie-bridge.exe")
	if err := os.MkdirAll(filepath.Dir(fallback), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fallback, []byte("MZ"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(fixture); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	a := &App{dataDir: root}
	got, err := a.bridgeExecutablePath()
	if err != nil {
		t.Fatalf("bridgeExecutablePath: %v", err)
	}
	if got != preferred {
		t.Fatalf("bridgeExecutablePath = %q, want <dataDir>/bridge preferred %q", got, preferred)
	}

	// dist fallback 生效：删掉优先位后应回到 cwd/dist。
	if err := os.Remove(preferred); err != nil {
		t.Fatal(err)
	}
	got, err = a.bridgeExecutablePath()
	if err != nil {
		t.Fatalf("bridgeExecutablePath after removing preferred: %v", err)
	}
	if got != fallback {
		t.Fatalf("fallback = %q, want dist fallback %q", got, fallback)
	}
}
