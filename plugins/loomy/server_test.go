package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
)

// A plugin process that lost its token must not serve the authenticated surface:
// /admin/credentials would otherwise overwrite the credential without auth.
func TestRequireTokenFailsClosedWithoutConfiguredToken(t *testing.T) {
	s := &server{cfg: pluginConfig{}}
	reached := false
	h := s.requireToken(func(w http.ResponseWriter, r *http.Request) { reached = true })
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/admin/credentials", nil))
	if reached {
		t.Fatal("an endpoint was served without a configured plugin token")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// loadManifestCapabilities reads manifest.json's capability list.
func loadManifestCapabilities(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("manifest.json")
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	var mf struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &mf); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	return mf.Capabilities
}

// /meta must mirror the manifest: this plugin implements upstream and accounts
// only. It has no quota source (quota stays empty) and no refresh route, so
// advertising either would be a promise the process cannot keep.
func TestMetaCapabilitiesMirrorManifest(t *testing.T) {
	want := loadManifestCapabilities(t)
	st, err := newStore(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	srv := &server{cfg: pluginConfig{ID: "loomy", APILevel: 2, Token: "tk"}, store: st}

	req := httptest.NewRequest(http.MethodGet, "/meta", nil)
	req.Header.Set("Authorization", "Bearer tk")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/meta = %d, body %s", rec.Code, rec.Body.String())
	}
	var meta struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil {
		t.Fatalf("decode /meta: %v (%s)", err, rec.Body.String())
	}
	got := append([]string(nil), meta.Capabilities...)
	manifest := append([]string(nil), want...)
	sort.Strings(got)
	sort.Strings(manifest)
	if strings.Join(got, ",") != strings.Join(manifest, ",") {
		t.Fatalf("/meta capabilities = %v, manifest = %v", meta.Capabilities, want)
	}
	for _, c := range meta.Capabilities {
		if c == "quota" || c == "refresh" {
			t.Fatalf("capabilities = %v: %s is declared without an implementation", meta.Capabilities, c)
		}
	}
}

// The login flow is now IMPLEMENTED (WeChat scan + 讯飞 signed exchange), so the
// old "501 and no login_* capability" posture is deliberately reversed:
//
//   - the manifest must declare login_oauth, otherwise the host hides the entry;
//   - POST /accounts/login must NOT answer 501 (that means "no such feature" to
//     the host);
//   - without the signing pair it must answer 4xx naming the two env vars, so the
//     operator can fix it without reading source.
//
// This replaces TestLoginIsHonestlyUnsupported, whose assertions were the exact
// opposite. Keeping the old test would have locked the plugin into the state the
// user reported as a bug ("该插件没有声明登录方式").
func TestLoginIsDeclaredAndActionable(t *testing.T) {
	declared := false
	for _, c := range loadManifestCapabilities(t) {
		if c == "login_oauth" {
			declared = true
		}
	}
	if !declared {
		t.Fatal("manifest must declare login_oauth, otherwise the host shows no login entry")
	}

	st, err := newStore(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	// No signing pair: the start must be actionable, never 501.
	srv := &server{
		cfg:    pluginConfig{ID: "loomy", APILevel: 2, Token: "tk"},
		store:  st,
		client: newUpstreamClient(),
		logins: newLoginManager(),
	}
	req := httptest.NewRequest(http.MethodPost, "/accounts/login", nil)
	req.Header.Set("Authorization", "Bearer tk")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)

	if rec.Code == http.StatusNotImplemented {
		t.Fatalf("POST /accounts/login = 501; the host reads that as \"no login here\" and hides the entry")
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /accounts/login = %d, want 400 naming the env vars; body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, envAccessKeyID) || !strings.Contains(body, envAccessKeySecret) {
		t.Fatalf("body = %q, want both env var names", body)
	}
}
