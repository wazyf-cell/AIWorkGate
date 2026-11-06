package main

// Egress bindings: the host-written egress.json is the only source of account
// routes. These tests prove the file is installed at startup, re-read on
// /reload, and actually used by clientFor — and that strict mode refuses a
// direct route instead of leaking the request out of the unproxied interface.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// recordingProxy is a local forwarder that counts the requests it receives.
func recordingProxy(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("proxied:" + r.URL.Path))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeEgress(t *testing.T, path, doc string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write egress file: %v", err)
	}
}

// TestEgressFileRoutesAccountsThroughTheirProxy: the account binding wins for an
// account, while login handshakes (accountID "") follow the default entry.
func TestEgressFileRoutesAccountsThroughTheirProxy(t *testing.T) {
	var defaultHits, accountHits atomic.Int32
	def := recordingProxy(t, &defaultHits)
	acct := recordingProxy(t, &accountHits)
	path := filepath.Join(t.TempDir(), "egress.json")
	writeEgress(t, path, `{"version":1,"strict":false,
		"default":{"mode":"proxy","url":"`+def.URL+`"},
		"accounts":{"cn/u1":{"mode":"proxy","url":"`+acct.URL+`"},"cn/u2":{"mode":"direct"}}}`)

	up := newUpstream("")
	if _, err := up.applyEgress(path); err != nil {
		t.Fatalf("applyEgress: %v", err)
	}

	// The account's own binding must be used, not the default one.
	c, err := up.clientFor("cn/u1")
	if err != nil {
		t.Fatalf("clientFor(cn/u1): %v", err)
	}
	resp, err := c.Get("http://upstream.invalid/userapi/v1/agent-card")
	if err != nil {
		t.Fatalf("request through account proxy: %v", err)
	}
	resp.Body.Close()
	if accountHits.Load() != 1 || defaultHits.Load() != 0 {
		t.Fatalf("account hits = %d, default hits = %d; want 1/0", accountHits.Load(), defaultHits.Load())
	}

	// A login handshake carries no account yet; it must use the default entry.
	c, err = up.clientFor("")
	if err != nil {
		t.Fatalf("clientFor(default): %v", err)
	}
	resp, err = c.Get("http://upstream.invalid/userapi/v1/agent-send-code")
	if err != nil {
		t.Fatalf("request through default proxy: %v", err)
	}
	resp.Body.Close()
	if defaultHits.Load() != 1 {
		t.Fatalf("default hits = %d, want 1 (login handshakes must follow the default route)", defaultHits.Load())
	}

	// An explicit direct binding must not carry a Proxy at all.
	c, err = up.clientFor("cn/u2")
	if err != nil {
		t.Fatalf("clientFor(cn/u2): %v", err)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("direct transport is %T, want *http.Transport", c.Transport)
	}
	if tr.Proxy != nil {
		t.Fatal("a direct binding must not send traffic through a proxy")
	}
}

// TestEgressStrictRefusesDirectAccount: under strict mode a direct entry is an
// error, never a silent fallback to the unproxied interface.
func TestEgressStrictRefusesDirectAccount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "egress.json")
	writeEgress(t, path, `{"version":1,"strict":true,
		"default":{"mode":"direct"},
		"accounts":{"cn/u1":{"mode":"direct"}}}`)

	up := newUpstream("")
	if _, err := up.applyEgress(path); err != nil {
		t.Fatalf("applyEgress: %v", err)
	}
	if _, err := up.clientFor("cn/u1"); err == nil || !strings.Contains(err.Error(), "egress_missing") {
		t.Fatalf("strict direct clientFor = %v, want an egress_missing error", err)
	}
}

// TestReloadRereadsEgressFile: /reload applies a rewritten file and reports the
// account count plus the strict flag, matching the other plugins' shape.
func TestReloadRereadsEgressFile(t *testing.T) {
	var hits atomic.Int32
	proxy := recordingProxy(t, &hits)
	s := newTestServer(t)
	path := filepath.Join(t.TempDir(), "egress.json")
	s.cfg.EgressFile = path
	writeEgress(t, path, `{"version":1,"strict":false,
		"default":{"mode":"direct"},
		"accounts":{"cn/u1":{"mode":"proxy","url":"`+proxy.URL+`"}}}`)

	rec := do(t, s, http.MethodPost, "/reload", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("reload = %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Egress int  `json:"egress"`
		Strict bool `json:"strict"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode reload: %v (%s)", err, rec.Body.String())
	}
	if out.Egress != 1 || out.Strict {
		t.Fatalf("reload reported %+v, want one binding and non-strict", out)
	}
	c, err := s.up.clientFor("cn/u1")
	if err != nil {
		t.Fatalf("clientFor after reload: %v", err)
	}
	resp, err := c.Get("http://upstream.invalid/userapi/v1/agent-card")
	if err != nil {
		t.Fatalf("request through reloaded proxy: %v", err)
	}
	resp.Body.Close()
	if hits.Load() != 1 {
		t.Fatalf("proxy hits = %d, want 1 (reload did not install the route)", hits.Load())
	}

	// A second rewrite must replace the route, not merge with it.
	writeEgress(t, path, `{"version":1,"strict":true,"default":{"mode":"direct"},"accounts":{}}`)
	rec = do(t, s, http.MethodPost, "/reload", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("second reload = %d %s", rec.Code, rec.Body.String())
	}
	out = struct {
		Egress int  `json:"egress"`
		Strict bool `json:"strict"`
	}{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode second reload: %v", err)
	}
	if out.Egress != 0 || !out.Strict {
		t.Fatalf("second reload reported %+v, want no bindings and strict", out)
	}
	if _, err := s.up.clientFor("cn/u1"); err == nil || !strings.Contains(err.Error(), "egress_missing") {
		t.Fatalf("clientFor after strict reload = %v, want egress_missing", err)
	}
}

// TestStartupMissingEgressFileStaysDirect: the env var is honoured at startup
// and a file that does not exist degrades to direct routing instead of failing.
func TestStartupMissingEgressFileStaysDirect(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.json")
	t.Setenv("AIWORKGATE_EGRESS_FILE", path)
	cfg := loadConfig()
	if cfg.EgressFile != path {
		t.Fatalf("cfg.EgressFile = %q, want %q", cfg.EgressFile, path)
	}
	up := newUpstream("")
	if _, err := up.applyEgress(cfg.EgressFile); err != nil {
		t.Fatalf("a missing egress file must degrade to direct, got %v", err)
	}
	c, err := up.clientFor("cn/u1")
	if err != nil {
		t.Fatalf("clientFor: %v", err)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil {
		t.Fatalf("missing file must leave routing direct, transport = %#v", c.Transport)
	}
}
