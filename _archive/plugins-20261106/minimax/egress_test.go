package main

// Egress tests: the binding in the host-written file must be enforced on the
// real request path (proxy routing), must fail closed in strict/blocked mode, and
// must be re-read on POST /reload.

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

func writeMMEgress(t *testing.T, path string, doc map[string]any) {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal egress: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write egress: %v", err)
	}
}

func TestMiniMaxEgressProxyRoutesTheLoginCall(t *testing.T) {
	var mu sync.Mutex
	var hosts, paths []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hosts = append(hosts, r.Host)
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		_, _ = io.WriteString(w, mmDeviceBody)
	}))
	defer proxy.Close()

	path := filepath.Join(t.TempDir(), "egress.json")
	writeMMEgress(t, path, map[string]any{
		"version": 1, "strict": false,
		"default": map[string]any{"mode": "proxy", "url": proxy.URL},
	})
	srv, err := newServer(pluginConfig{ID: "minimax", Token: "tk", DataDir: t.TempDir(), EgressFile: path})
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	// Port 1 is the "direct internet" the proxy must replace.
	srv.client.accountBase = "http://127.0.0.1:1"

	req := httptest.NewRequest(http.MethodPost, "/accounts/login", strings.NewReader(`{"method":"oauth"}`))
	req.Header.Set("Authorization", "Bearer tk")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login through proxy = %d %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hosts) == 0 || hosts[0] != "127.0.0.1:1" || paths[0] != deviceCodePath {
		t.Fatalf("proxy saw %v %v", hosts, paths)
	}
}

func TestMiniMaxEgressStrictDirectIsBlockedAndReloadApplies(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, mmDeviceBody)
	}))
	defer proxy.Close()

	path := filepath.Join(t.TempDir(), "egress.json")
	writeMMEgress(t, path, map[string]any{
		"version": 1, "strict": true,
		"default": map[string]any{"mode": "direct"},
	})
	srv, err := newServer(pluginConfig{ID: "minimax", Token: "tk", DataDir: t.TempDir(), EgressFile: path})
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	srv.client.accountBase = "http://127.0.0.1:1"
	handler := srv.routes()

	post := func(target, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer tk")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	rec := post("/accounts/login", `{"method":"oauth"}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "egress_missing") {
		t.Fatalf("strict direct login = %d %s", rec.Code, rec.Body.String())
	}

	writeMMEgress(t, path, map[string]any{
		"version": 1, "strict": true,
		"default": map[string]any{"mode": "proxy", "url": proxy.URL},
	})
	if rec := post("/reload", ""); rec.Code != http.StatusOK {
		t.Fatalf("reload = %d %s", rec.Code, rec.Body.String())
	}
	if rec := post("/accounts/login", `{"method":"oauth"}`); rec.Code != http.StatusOK {
		t.Fatalf("login after reload = %d %s", rec.Code, rec.Body.String())
	}
}

func TestMiniMaxEgressBlockedModeFailsChat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "egress.json")
	writeMMEgress(t, path, map[string]any{
		"version": 1,
		"default": map[string]any{"mode": "blocked"},
	})
	srv, err := newServer(pluginConfig{ID: "minimax", Token: "tk", DataDir: t.TempDir(), EgressFile: path})
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	if err := srv.store.set(credentials{AccessToken: "tok"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"MiniMax-M3","messages":[{"role":"user","content":"x"}]}`))
	req.Header.Set("Authorization", "Bearer tk")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "egress_missing") {
		t.Fatalf("chat = %d %s", rec.Code, rec.Body.String())
	}
}
