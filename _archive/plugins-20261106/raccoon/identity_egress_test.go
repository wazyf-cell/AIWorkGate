package main

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

// TestIdentityIsExposedAndRotated covers P1: the identity block is readable in
// /accounts and POST .../identity/reset really changes the stored identifiers.
func TestIdentityIsExposedAndRotated(t *testing.T) {
	srv, h := newLoginTestServer(t, "")
	creds := credentials{AccessToken: "tok-a", Identity: newIdentity()}
	if err := srv.store.set(creds); err != nil {
		t.Fatalf("set: %v", err)
	}
	before, _ := srv.store.get()
	if before.Identity == nil || len(before.Identity.DeviceID) != 32 {
		t.Fatalf("stored identity = %+v, want a 32-hex device id", before.Identity)
	}

	body := doReq(t, h, "/accounts").Body.String()
	if !strings.Contains(body, `"machine_id":"`+before.Identity.MachineID+`"`) ||
		!strings.Contains(body, `"session_id":"`+before.Identity.SessionID+`"`) {
		t.Fatalf("/accounts does not expose the stored identity: %s", body)
	}

	post := func(payload string) (*httptest.ResponseRecorder, map[string]any) {
		r := httptest.NewRequest(http.MethodPost, "/accounts/default/identity/reset", strings.NewReader(payload))
		r.Header.Set("Authorization", "Bearer tok")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec, out
	}

	rec, out := post(`{"mode":"request"}`)
	if rec.Code != http.StatusOK || out["ok"] != true {
		t.Fatalf("request reset = %d %v", rec.Code, out)
	}
	after, _ := srv.store.get()
	if after.Identity.DeviceID == before.Identity.DeviceID || after.Identity.SessionID == before.Identity.SessionID {
		t.Fatal("request reset did not rotate device_id/session_id")
	}
	if after.Identity.MachineID != before.Identity.MachineID {
		t.Fatal("request reset must not touch the machine-level id")
	}
	if after.AccessToken != "tok-a" {
		t.Fatal("identity reset must not drop the token")
	}

	rec, _ = post(`{"mode":"os"}`)
	// The plugin declares identity_request only (permissions.os_identity=false):
	// it cannot switch a machine-level identity, so mode=os must be refused
	// instead of reporting a success it did not perform.
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("os reset = %d, want 501 (no identity_os_switch capability)", rec.Code)
	}
	var osOut map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &osOut)
	if code, _ := osOut["error"].(map[string]any)["code"].(string); code != "identity_os_not_supported" {
		t.Fatalf("os reset code = %v, want identity_os_not_supported", osOut["error"])
	}
	after2, _ := srv.store.get()
	if after2.Identity.MachineID != after.Identity.MachineID {
		t.Fatal("mode=os must not touch machine_id when the plugin cannot switch OS identity")
	}

	if rec, _ := post(`{"mode":"weird"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad mode = %d, want 400", rec.Code)
	}
	r := httptest.NewRequest(http.MethodPost, "/accounts/other/identity/reset", strings.NewReader(`{"mode":"request"}`))
	r.Header.Set("Authorization", "Bearer tok")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown account = %d, want 404", rec.Code)
	}
}

// TestIdentityResetReportsOnlyWhatChanged pins the answer's honesty: a request
// reset rotates two LOCAL identifiers, and no Raccoon request carries
// session_id/machine_id at all. Chat and model_catalog never send a device id;
// user_info sends X-Client-Device-ID, but only during a scan login, which mints
// a fresh identity anyway. So the response must list the changed fields, state
// that the upstream effect is none, and must not claim the change applies to the
// next request.
func TestIdentityResetReportsOnlyWhatChanged(t *testing.T) {
	srv, h := newLoginTestServer(t, "")
	if err := srv.store.set(credentials{AccessToken: "tok-a", Identity: newIdentity()}); err != nil {
		t.Fatalf("set: %v", err)
	}
	before, _ := srv.store.get()

	r := httptest.NewRequest(http.MethodPost, "/accounts/default/identity/reset", strings.NewReader(`{"mode":"request"}`))
	r.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset = %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		OK             bool           `json:"ok"`
		Mode           string         `json:"mode"`
		Changed        []string       `json:"changed"`
		Unchanged      []string       `json:"unchanged"`
		UpstreamEffect string         `json:"upstream_effect"`
		Note           string         `json:"note"`
		Identity       map[string]any `json:"identity"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if !out.OK || out.Mode != "request" {
		t.Fatalf("ok/mode = %v/%q, want true/request", out.OK, out.Mode)
	}
	if strings.Join(out.Changed, ",") != "device_id,session_id" {
		t.Fatalf("changed = %v, want [device_id session_id]", out.Changed)
	}
	if strings.Join(out.Unchanged, ",") != "machine_id" {
		t.Fatalf("unchanged = %v, want [machine_id]", out.Unchanged)
	}
	if out.UpstreamEffect != "none" {
		t.Fatalf("upstream_effect = %q, want %q: the reset changes local ids only", out.UpstreamEffect, "none")
	}
	if !strings.Contains(out.Note, "user_info") || !strings.Contains(out.Note, "扫码登录") {
		t.Fatalf("note must say when device_id is sent upstream (user_info at the next scan login): %q", out.Note)
	}
	if strings.Contains(out.Note, "下一次请求起生效") {
		t.Fatalf("note still claims the change applies to the next request: %q", out.Note)
	}

	after, _ := srv.store.get()
	if out.Identity["device_id"] != after.Identity.DeviceID || out.Identity["session_id"] != after.Identity.SessionID {
		t.Fatalf("response identity does not match the stored one: %v", out.Identity)
	}
	if after.Identity.DeviceID == before.Identity.DeviceID || after.Identity.SessionID == before.Identity.SessionID {
		t.Fatal("request reset did not rotate the local device_id/session_id")
	}
}

// newProxyRecorder is a plain HTTP forward proxy for http:// targets: it
// records every forwarded request and replays it to the real upstream.
func newProxyRecorder(t *testing.T, hits *int) *httptest.Server {
	t.Helper()
	direct := &http.Client{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		if r.URL.Scheme == "" {
			t.Errorf("proxied request has no absolute URL: %s", r.URL.String())
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		out, err := http.NewRequest(r.Method, r.URL.String(), r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		out.Header = r.Header.Clone()
		resp, err := direct.Do(out)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestEgressProxyReallyRoutesTraffic is the "declare egress only when it works"
// guard: with mode proxy the chat request must arrive at the proxy, not go direct.
func TestEgressProxyReallyRoutesTraffic(t *testing.T) {
	sse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	defer sse.Close()

	hits := 0
	proxy := newProxyRecorder(t, &hits)

	srv, h := newLoginTestServer(t, sse.URL)
	if err := srv.store.set(credentials{AccessToken: "t", BaseURL: sse.URL}); err != nil {
		t.Fatalf("set: %v", err)
	}
	srv.egress.Accounts["default"] = egressEntry{Mode: egressProxy, URL: proxy.URL}

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
	r.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat through proxy = %d %s", rec.Code, rec.Body.String())
	}
	if hits != 1 {
		t.Fatalf("proxy saw %d requests, want 1 (traffic did not use the egress binding)", hits)
	}
	if !strings.Contains(rec.Body.String(), `"content":"ok"`) {
		t.Fatalf("body was not relayed: %s", rec.Body.String())
	}
}

// TestEgressStrictRefusesDirect: strict mode must reject a direct account
// instead of silently leaking it out of the default route.
func TestEgressStrictRefusesDirect(t *testing.T) {
	srv, h := newLoginTestServer(t, "")
	if err := srv.store.set(credentials{AccessToken: "t"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	srv.egress.Strict = true

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
	r.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "egress_missing") {
		t.Fatalf("strict direct chat = %d %s, want 403 egress_missing", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, h, "/v1/models"); rec.Code != http.StatusForbidden {
		t.Fatalf("strict direct models = %d, want 403", rec.Code)
	}
	if rec := doReq(t, h, "/accounts"); rec.Code != http.StatusOK {
		t.Fatalf("/accounts must stay readable in strict mode, got %d", rec.Code)
	}
}

// TestReloadPicksUpEgressFile proves the file is actually the source of truth.
func TestReloadPicksUpEgressFile(t *testing.T) {
	srv, h := newLoginTestServer(t, "")
	path := filepath.Join(t.TempDir(), "egress.json")
	srv.egressPath = path
	raw := `{"version":1,"strict":true,"default":{"mode":"blocked"},"accounts":{"default":{"mode":"proxy","url":"http://127.0.0.1:9"}}}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("write egress: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/reload", nil)
	r.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("reload = %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Egress int  `json:"egress"`
		Strict bool `json:"strict"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Egress != 1 || !out.Strict {
		t.Fatalf("reload reported %+v, want one binding and strict", out)
	}
	entry, err := srv.resolveEgress("default")
	if err != nil || entry.Mode != egressProxy {
		t.Fatalf("resolved = %+v err=%v, want the proxy binding", entry, err)
	}
	// An account with no binding falls back to default, which is "blocked" here.
	if _, err := srv.resolveEgress("other"); err == nil {
		t.Fatal("a blocked default must surface an egress error, not a direct route")
	}
}
