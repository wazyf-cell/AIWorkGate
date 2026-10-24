package main

// M2 guard: the manifest's capability list must equal what the routes implement.
// A capability added to one side only (a fake button, or a hidden feature) fails
// here instead of surfacing as a dead UI control.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
)

func TestClineManifestCapabilitiesMatchRoutes(t *testing.T) {
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
	// Every declared login_* has a method entry, and vice versa.
	methods := map[string]bool{}
	for _, m := range mf.Login.Methods {
		methods[m] = true
	}
	if methods["oauth"] != containsCap("login_oauth") || methods["paste_code"] != containsCap("login_paste_code") {
		t.Fatalf("login.methods = %v, capabilities = %v", mf.Login.Methods, capabilities)
	}

	// Each non-login capability must have a live route, not a 404/501.
	srv, err := newServer(serverConfig{ID: "cline", Token: "tk", DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	handler := srv.routes()
	calls := []struct{ method, path, body string }{
		{http.MethodGet, "/accounts", ""},
		{http.MethodPost, "/v1/chat/completions", `{"model":"m","messages":[]}`},
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

	// quota must not be declared: Cline reports credit only inside a 402, so
	// /accounts carries an empty quota object. Declaring the capability would
	// make the host poll a source that has no number to give.
	if containsCap("quota") {
		t.Fatalf("capabilities = %v: quota is declared but /accounts has no quota source", capabilities)
	}
	for _, c := range mf.Capabilities {
		if c == "quota" {
			t.Fatalf("manifest capabilities = %v: quota is declared without a quota source", mf.Capabilities)
		}
	}

	// The account payload must stay number-free for the same reason.
	st, err := newStore(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := st.set(&clineCredential{AccessToken: "a"}); err != nil {
		t.Fatalf("set credential: %v", err)
	}
	acctSrv := &server{cfg: serverConfig{ID: "cline", Token: "tk"}, store: st, client: newUpstreamClient()}
	req := httptest.NewRequest(http.MethodGet, "/accounts", nil)
	req.Header.Set("Authorization", "Bearer tk")
	rec := httptest.NewRecorder()
	acctSrv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/accounts = %d, body %s", rec.Code, rec.Body.String())
	}
	var accounts struct {
		Accounts []struct {
			Quota map[string]any `json:"quota"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &accounts); err != nil {
		t.Fatalf("decode /accounts: %v (%s)", err, rec.Body.String())
	}
	if len(accounts.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(accounts.Accounts))
	}
	if q := accounts.Accounts[0].Quota; len(q) != 0 {
		t.Fatalf("quota = %v, want empty (unknown): there is no balance endpoint", q)
	}
}

func containsCap(name string) bool {
	for _, c := range capabilities {
		if c == name {
			return true
		}
	}
	return false
}
