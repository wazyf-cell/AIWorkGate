package main

// Egress tests. They prove the host-written binding is enforced on the real
// request path: a proxy entry routes the login call through the forwarder, a
// strict direct entry fails closed, and POST /reload applies a new binding
// without a restart.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func writeEgressFile(t *testing.T, path string, doc map[string]any) {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal egress: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write egress: %v", err)
	}
}

func TestEgressProxyRoutesTheLoginCall(t *testing.T) {
	var mu sync.Mutex
	var seenHosts, seenPaths []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenHosts = append(seenHosts, r.Host)
		seenPaths = append(seenPaths, r.URL.Path)
		mu.Unlock()
		_, _ = io.WriteString(w, deviceAuthBody)
	}))
	defer proxy.Close()

	path := filepath.Join(t.TempDir(), "egress.json")
	writeEgressFile(t, path, map[string]any{
		"version": 1,
		"strict":  false,
		"default": map[string]any{"mode": "proxy", "url": proxy.URL},
	})
	srv, err := newServer(serverConfig{ID: "cline", Token: "tk", DataDir: t.TempDir(), EgressFile: path})
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	// Port 1 is the "direct internet" the proxy must replace. If the request
	// leaked around the proxy it would fail instead of answering 200.
	srv.client.workOs = "http://127.0.0.1:1"

	req := httptest.NewRequest(http.MethodPost, "/accounts/login", strings.NewReader(`{"method":"oauth"}`))
	req.Header.Set("Authorization", "Bearer tk")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login through proxy = %d %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seenHosts) != 1 || seenHosts[0] != "127.0.0.1:1" {
		t.Fatalf("proxy saw %v, want the direct host routed through it", seenHosts)
	}
	if seenPaths[0] != deviceAuthorizationPath {
		t.Fatalf("proxy path = %q", seenPaths[0])
	}
}

func TestEgressStrictDirectIsBlockedAndReloadApplies(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, deviceAuthBody)
	}))
	defer proxy.Close()

	path := filepath.Join(t.TempDir(), "egress.json")
	writeEgressFile(t, path, map[string]any{
		"version": 1, "strict": true,
		"default": map[string]any{"mode": "direct"},
	})
	srv, err := newServer(serverConfig{ID: "cline", Token: "tk", DataDir: t.TempDir(), EgressFile: path})
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	srv.client.workOs = "http://127.0.0.1:1"
	handler := srv.routes()

	post := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer tk")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	rec := post("/accounts/login", `{"method":"oauth"}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "egress_missing") {
		t.Fatalf("strict direct login = %d %s", rec.Code, rec.Body.String())
	}
	if srv.store.configured() {
		t.Fatal("a blocked login must not store anything")
	}

	// A binding change must take effect on /reload, not only on restart.
	writeEgressFile(t, path, map[string]any{
		"version": 1, "strict": true,
		"default": map[string]any{"mode": "proxy", "url": proxy.URL},
	})
	if rec := post("/reload", ""); rec.Code != http.StatusOK {
		t.Fatalf("reload = %d %s", rec.Code, rec.Body.String())
	}
	rec = post("/accounts/login", `{"method":"oauth"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("login after reload = %d %s", rec.Code, rec.Body.String())
	}
}

func TestEgressBlockedModeFailsChat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "egress.json")
	writeEgressFile(t, path, map[string]any{
		"version": 1,
		"default": map[string]any{"mode": "blocked"},
	})
	srv, err := newServer(serverConfig{ID: "cline", Token: "tk", DataDir: t.TempDir(), EgressFile: path})
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	if err := srv.store.set(&clineCredential{AccessToken: "tok"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"x"}]}`))
	req.Header.Set("Authorization", "Bearer tk")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "egress_missing") {
		t.Fatalf("chat = %d %s", rec.Code, rec.Body.String())
	}
}
