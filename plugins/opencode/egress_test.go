package main

// Egress tests. They prove the host-written binding is enforced on the real
// request path, which is the only thing that matters here: the free tier makes
// its regional decision from the egress address, so a binding that silently
// fell back to the direct connection would look like a random upstream failure
// instead of a routing bug.

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

// newEgressServer builds a server whose upstream base points at a closed port,
// so any request that leaks past the proxy fails instead of answering.
func newEgressServer(t *testing.T, path string) *server {
	t.Helper()
	srv, err := newServer(serverConfig{
		ID: "opencode", Token: "tk", DataDir: t.TempDir(), EgressFile: path,
	})
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	srv.client.base = "http://127.0.0.1:1"
	return srv
}

func TestEgressProxyRoutesTheChatCall(t *testing.T) {
	var mu sync.Mutex
	var seenHost string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenHost = r.Host
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer proxy.Close()

	path := filepath.Join(t.TempDir(), "egress.json")
	writeEgressFile(t, path, map[string]any{
		"version": 1,
		"strict":  false,
		"default": map[string]any{"mode": "proxy", "url": proxy.URL},
	})
	srv := newEgressServer(t, path)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"big-pickle","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer tk")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)

	// Port 1 is the "direct internet" the proxy must replace. A leak around the
	// proxy would surface as a 502/503 here, not a 200.
	if rec.Code != http.StatusOK {
		t.Fatalf("chat through proxy = %d %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if seenHost != "127.0.0.1:1" {
		t.Fatalf("proxy saw host %q, want the upstream routed through it", seenHost)
	}
}

// The most important test: a bound but unreachable proxy must FAIL, never fall
// back to the direct route. A fallback would silently defeat the whole feature.
func TestEgressProxyFailureDoesNotFallBackToDirect(t *testing.T) {
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request reached the direct route; egress must fail closed")
		w.WriteHeader(http.StatusOK)
	}))
	defer direct.Close()

	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // now refuses connections

	path := filepath.Join(t.TempDir(), "egress.json")
	writeEgressFile(t, path, map[string]any{
		"version": 1,
		"strict":  false,
		"default": map[string]any{"mode": "proxy", "url": deadURL},
	})
	srv, err := newServer(serverConfig{ID: "opencode", Token: "tk", DataDir: t.TempDir(), EgressFile: path})
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	// If anything bypassed the proxy it would reach this live direct server.
	srv.client.base = direct.URL

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"big-pickle","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer tk")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("chat with a dead proxy answered 200; it fell back to direct: %s", rec.Body.String())
	}
}

func TestEgressStrictDirectIsBlockedAndReloadApplies(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer proxy.Close()

	path := filepath.Join(t.TempDir(), "egress.json")
	writeEgressFile(t, path, map[string]any{
		"version": 1, "strict": true,
		"default": map[string]any{"mode": "direct"},
	})
	srv := newEgressServer(t, path)
	handler := srv.routes()

	post := func(p, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, p, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer tk")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	rec := post("/v1/chat/completions", `{"model":"big-pickle","messages":[]}`)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "egress_missing") {
		t.Fatalf("strict direct chat = %d %s", rec.Code, rec.Body.String())
	}

	// A binding change must take effect on /reload, not only on restart.
	writeEgressFile(t, path, map[string]any{
		"version": 1, "strict": true,
		"default": map[string]any{"mode": "proxy", "url": proxy.URL},
	})
	if rec := post("/reload", ""); rec.Code != http.StatusOK {
		t.Fatalf("reload = %d %s", rec.Code, rec.Body.String())
	}
	if rec := post("/v1/chat/completions", `{"model":"big-pickle","messages":[]}`); rec.Code != http.StatusOK {
		t.Fatalf("chat after reload = %d %s", rec.Code, rec.Body.String())
	}
}

func TestEgressBlockedModeFailsChatAndModels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "egress.json")
	writeEgressFile(t, path, map[string]any{
		"version": 1,
		"default": map[string]any{"mode": "blocked"},
	})
	srv := newEgressServer(t, path)
	handler := srv.routes()

	for _, p := range []string{"/v1/chat/completions", "/v1/models"} {
		req := httptest.NewRequest(http.MethodPost, p, strings.NewReader(`{"model":"big-pickle","messages":[]}`))
		if p == "/v1/models" {
			req = httptest.NewRequest(http.MethodGet, p, nil)
		}
		req.Header.Set("Authorization", "Bearer tk")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "egress_missing") {
			t.Fatalf("%s under blocked = %d %s", p, rec.Code, rec.Body.String())
		}
	}
}

// A missing file is the normal "no binding" case and must keep working exactly
// as before egress existed.
func TestEgressMissingFileStaysDirect(t *testing.T) {
	srv, err := newServer(serverConfig{ID: "opencode", Token: "tk", DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	if err := srv.egressError(); err != nil {
		t.Fatalf("no egress file must allow the direct route, got %v", err)
	}
	if srv.client.proxyURL != "" || srv.client.egressErr != nil {
		t.Fatalf("client kept egress state without a binding: proxyURL=%q err=%v", srv.client.proxyURL, srv.client.egressErr)
	}
}

// A proxy binding whose url is empty degrades to blocked, per the contract.
func TestEgressProxyWithoutURLDegradesToBlocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "egress.json")
	writeEgressFile(t, path, map[string]any{
		"version": 1,
		"default": map[string]any{"mode": "proxy", "url": ""},
	})
	srv := newEgressServer(t, path)
	if err := srv.egressError(); err == nil {
		t.Fatal("proxy without url must be treated as blocked")
	}
}
