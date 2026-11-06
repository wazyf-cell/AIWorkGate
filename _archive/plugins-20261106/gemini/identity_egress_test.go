package main

// Identity and egress tests: the reset endpoint must rotate identifiers that
// are actually sent upstream, and the egress file must decide whether a chat
// request leaves direct or through the bound proxy.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIdentityResetRotatesStoredIdentifiers(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// GET /accounts reads quota; answer an empty summary rather than failing.
		io.WriteString(w, `{}`)
	}))
	defer upstream.Close()
	srv, handler := newTestServer(t, upstream.URL)

	// The seed credential has no identity yet; the reset issues one and rotates.
	rec := doAccountPost(t, handler, "/accounts/default/identity/reset", `{"mode":"request"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset: %d %s", rec.Code, rec.Body.String())
	}
	first, _ := srv.store.get()
	if first.Identity == nil || first.Identity.DeviceID == "" || first.Identity.SessionID == "" {
		t.Fatalf("identity not stored: %+v", first.Identity)
	}
	genBefore := sessionGeneration(first.Identity)

	rec = doAccountPost(t, handler, "/accounts/default/identity/reset", `{"mode":"request"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("second reset: %d %s", rec.Code, rec.Body.String())
	}
	second, _ := srv.store.get()
	if second.Identity.DeviceID == first.Identity.DeviceID || second.Identity.SessionID == first.Identity.SessionID {
		t.Fatalf("identifiers did not rotate: %+v vs %+v", first.Identity, second.Identity)
	}
	if sessionGeneration(second.Identity) == genBefore {
		t.Fatal("session generation did not change; the wire sessionId would be identical")
	}

	// GET /accounts must report the new identity for the details dialog.
	list := doGet(t, handler, "/accounts")
	var out struct {
		Accounts []struct {
			Identity map[string]any `json:"identity"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &out); err != nil || len(out.Accounts) != 1 {
		t.Fatalf("decode accounts: %v %s", err, list.Body.String())
	}
	id := out.Accounts[0].Identity
	if id["device_id"] != second.Identity.DeviceID {
		t.Fatalf("accounts identity = %v, want device_id %s", id, second.Identity.DeviceID)
	}
	if id["machine_id"] != machineTag || id["user_agent"] != identityUserAgent {
		t.Fatalf("identity must expose the real protocol values: %v", id)
	}
}

func TestIdentityResetRefusesOSMode(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	_, handler := newTestServer(t, upstream.URL)
	rec := doAccountPost(t, handler, "/accounts/default/identity/reset", `{"mode":"os"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d (%s), want 400", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unsupported_identity_mode") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestSessionIDDependsOnTheIdentityGeneration(t *testing.T) {
	spec, err := modelSpecFor("gemini-3.8-flash", "medium")
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	body := []byte(`{"model":"gemini-3.8-flash","messages":[{"role":"user","content":"hi"}]}`)
	a, err := translateRequest(body, requestOptions{Spec: spec, Identity: &identityState{DeviceID: "d1", SessionID: "s1"}})
	if err != nil {
		t.Fatalf("translate a: %v", err)
	}
	b, err := translateRequest(body, requestOptions{Spec: spec, Identity: &identityState{DeviceID: "d2", SessionID: "s2"}})
	if err != nil {
		t.Fatalf("translate b: %v", err)
	}
	sessionOf := func(raw []byte) string {
		var env struct {
			Request struct {
				SessionID string `json:"sessionId"`
			} `json:"request"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return env.Request.SessionID
	}
	if sessionOf(a) == sessionOf(b) {
		t.Fatalf("sessionId did not change with the identity: %s", sessionOf(a))
	}
}

func TestEgressFileRoutesChatThroughTheProxy(t *testing.T) {
	var gotHost, gotPath string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost, gotPath = r.Host, r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"proxied\"}]},\"finishReason\":\"STOP\"}]}\n\n")
	}))
	defer proxy.Close()

	st, err := newStore(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := st.set(credentials{
		AccessToken: "ya29.test", Expiry: "2099-01-01T00:00:00Z",
		CloudaicompanionProject: "proj",
		// Plain http target so the proxy receives an absolute-form request.
		EndpointBase: "http://gemini-upstream.test",
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "egress.json")
	doc := `{"version":1,"strict":false,"default":{"mode":"direct"},
		"accounts":{"default":{"mode":"proxy","url":"` + proxy.URL + `"}}}`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write egress: %v", err)
	}
	srv := newServer(pluginConfig{ID: "gemini", Token: "tk", EgressFile: path}, st)
	handler := srv.routes()

	rec := doPost(t, handler, `{"model":"gemini-3.8-flash","messages":[{"role":"user","content":"ping"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat: %d %s", rec.Code, rec.Body.String())
	}
	if gotHost != "gemini-upstream.test" || gotPath != "/v1internal:streamGenerateContent" {
		t.Fatalf("proxy saw host=%q path=%q", gotHost, gotPath)
	}
	if !strings.Contains(rec.Body.String(), "proxied") {
		t.Fatalf("response = %s", rec.Body.String())
	}
}

func TestEgressStrictModeRefusesDirect(t *testing.T) {
	st, err := newStore(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := st.set(credentials{AccessToken: "ya29.test", Expiry: "2099-01-01T00:00:00Z", CloudaicompanionProject: "p"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "egress.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"strict":true,"default":{"mode":"direct"}}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	srv := newServer(pluginConfig{ID: "gemini", Token: "tk", EgressFile: path}, st)
	rec := doPost(t, srv.routes(), `{"model":"gemini-3.8-flash","messages":[{"role":"user","content":"x"}]}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d (%s), want 403 egress_missing", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "egress_missing") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

// doAccountPost sends an authenticated POST to an account sub-resource.
func doAccountPost(t *testing.T, handler http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tk")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}
