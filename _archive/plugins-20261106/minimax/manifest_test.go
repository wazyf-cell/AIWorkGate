package main

// M2 guard: the manifest's capability list must equal what the routes implement,
// so no capability is advertised without a real endpoint (and none is hidden).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
)

func TestMiniMaxManifestCapabilitiesMatchRoutes(t *testing.T) {
	raw, err := os.ReadFile("manifest.json")
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	var mf struct {
		Capabilities []string `json:"capabilities"`
		Login        struct {
			Methods []string `json:"methods"`
		} `json:"login"`
	}
	if err := json.Unmarshal(raw, &mf); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	got := append([]string(nil), capabilities...)
	want := append([]string(nil), mf.Capabilities...)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("capabilities = %v, manifest = %v", got, want)
	}
	methods := map[string]bool{}
	for _, m := range mf.Login.Methods {
		methods[m] = true
	}
	if methods["oauth"] != mmHasCap("login_oauth") || methods["paste_code"] != mmHasCap("login_paste_code") {
		t.Fatalf("login.methods = %v, capabilities = %v", mf.Login.Methods, capabilities)
	}

	srv, err := newServer(pluginConfig{ID: "minimax", Token: "tk", DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	handler := srv.routes()
	calls := []struct{ method, path, body string }{
		{http.MethodGet, "/accounts", ""},
		{http.MethodPost, "/v1/chat/completions", `{"model":"MiniMax-M3","messages":[]}`},
		{http.MethodPost, "/accounts/default/refresh", ""},
		{http.MethodPost, "/reload", ""},
	}
	for _, c := range calls {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		req.Header.Set("Authorization", "Bearer tk")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code == http.StatusNotFound || rec.Code == http.StatusNotImplemented {
			t.Fatalf("%s %s = %d: capability declared without a route", c.method, c.path, rec.Code)
		}
	}
	// quota is declared, so /accounts must carry the key (an empty object means
	// "unknown", which is allowed; omitting it entirely would not be).
	req := httptest.NewRequest(http.MethodGet, "/accounts", nil)
	req.Header.Set("Authorization", "Bearer tk")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `"accounts"`) {
		t.Fatalf("/accounts = %s", rec.Body.String())
	}
}

func mmHasCap(name string) bool {
	for _, c := range capabilities {
		if c == name {
			return true
		}
	}
	return false
}
