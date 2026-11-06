package main

// Identity contract tests: /accounts must expose an identity block, the
// details endpoint must answer, and a request-mode reset must really change the
// exposed device id (the x-opencode-project fingerprint). mode=os has nothing
// behind it here and must be refused rather than faked.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testPluginToken = "tk"

func newAccountTestServer(t *testing.T) (*server, http.Handler) {
	t.Helper()
	srv, err := newServer(serverConfig{ID: pluginID, Token: testPluginToken, DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	return srv, srv.routes()
}

func accountRequest(t *testing.T, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testPluginToken)
	handler.ServeHTTP(rec, req)
	return rec
}

// accountsIdentity returns the identity block GET /accounts reports.
func accountsIdentity(t *testing.T, handler http.Handler) map[string]any {
	t.Helper()
	rec := accountRequest(t, handler, http.MethodGet, "/accounts", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /accounts = %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Accounts []struct {
			Identity map[string]any `json:"identity"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Accounts) != 1 {
		t.Fatalf("decode accounts: %v %s", err, rec.Body.String())
	}
	return out.Accounts[0].Identity
}

func identityString(t *testing.T, id map[string]any, key string) string {
	t.Helper()
	v, _ := id[key].(string)
	if strings.TrimSpace(v) == "" {
		t.Fatalf("identity.%s missing or empty in %v", key, id)
	}
	return v
}

func TestAccountsExposeDerivedIdentity(t *testing.T) {
	_, handler := newAccountTestServer(t)
	id := accountsIdentity(t, handler)
	if id == nil {
		t.Fatal("GET /accounts carries no identity block; the UI cannot offer a reset")
	}
	// The device id is the fingerprint actually sent as x-opencode-project, so
	// it must be the 40-hex projectID, not an invented value.
	if got := identityString(t, id, "device_id"); len(got) != 40 {
		t.Fatalf("device_id = %q, want the 40-hex project fingerprint", got)
	}
	if id["source"] != "derived" {
		t.Fatalf("source = %v, want derived", id["source"])
	}
	if id["user_agent"] != defaultUserAgent {
		t.Fatalf("user_agent = %v, want %q", id["user_agent"], defaultUserAgent)
	}
	// This plugin has no machine-level identity and no stored session id, so
	// those keys must be absent rather than fabricated.
	if _, ok := id["machine_id"]; ok {
		t.Fatalf("machine_id invented: %v", id)
	}
	if _, ok := id["session_id"]; ok {
		t.Fatalf("session_id invented: %v", id)
	}
}

func TestIdentityDetailsAnswers(t *testing.T) {
	_, handler := newAccountTestServer(t)
	want := accountsIdentity(t, handler)

	rec := accountRequest(t, handler, http.MethodGet, "/accounts/default/identity", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET identity = %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		OK       bool           `json:"ok"`
		Identity map[string]any `json:"identity"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v %s", err, rec.Body.String())
	}
	if !out.OK || out.Identity == nil {
		t.Fatalf("body = %s, want {\"ok\":true,\"identity\":{...}}", rec.Body.String())
	}
	if out.Identity["device_id"] != want["device_id"] {
		t.Fatalf("details device_id = %v, accounts = %v", out.Identity["device_id"], want["device_id"])
	}

	// An unknown account must not be answered as if it existed. This plugin has
	// one account, and every sub-route says so with the same 409.
	rec = accountRequest(t, handler, http.MethodGet, "/accounts/other/identity", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("unknown account = %d, want 409", rec.Code)
	}
}

func TestIdentityResetRequestRotatesDeviceID(t *testing.T) {
	srv, handler := newAccountTestServer(t)
	before := identityString(t, accountsIdentity(t, handler), "device_id")

	rec := accountRequest(t, handler, http.MethodPost, "/accounts/default/identity/reset", `{"mode":"request"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset = %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		OK       bool           `json:"ok"`
		Mode     string         `json:"mode"`
		Rotated  bool           `json:"rotated"`
		Identity map[string]any `json:"identity"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v %s", err, rec.Body.String())
	}
	if !out.OK || out.Mode != "request" || !out.Rotated {
		t.Fatalf("reset body = %s", rec.Body.String())
	}
	after := identityString(t, out.Identity, "device_id")
	if after == before {
		t.Fatalf("reset kept device_id %q; the new fingerprint was not applied", after)
	}
	// The exposed value must be the one /accounts and the wire now use.
	if got := identityString(t, accountsIdentity(t, handler), "device_id"); got != after {
		t.Fatalf("GET /accounts device_id = %q, reset reported %q", got, after)
	}
	// The key survives a fingerprint rotation.
	if got := srv.store.current().effectiveKey(); got != anonymousKey {
		t.Fatalf("reset changed the credential to %q", got)
	}
}

func TestIdentityResetRefusesOSMode(t *testing.T) {
	_, handler := newAccountTestServer(t)
	before := identityString(t, accountsIdentity(t, handler), "device_id")

	rec := accountRequest(t, handler, http.MethodPost, "/accounts/default/identity/reset", `{"mode":"os"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("mode=os = %d %s, want 400", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unsupported_identity_mode") {
		t.Fatalf("body = %s, want unsupported_identity_mode", rec.Body.String())
	}
	if got := identityString(t, accountsIdentity(t, handler), "device_id"); got != before {
		t.Fatalf("a refused os reset still rotated the identity: %q -> %q", before, got)
	}
}
