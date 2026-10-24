package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGatewayCredentialHeadersDoNotReachTheAssistant: the host authenticates the
// client's gateway key with Authorization, X-API-Key or api-key
// (internal/gateway/gateway.go clientToken) and forwards the latter two
// unchanged. The plugin replaces Authorization with the assistant's own key, so
// it must also drop the gateway's key spellings instead of leaking a client
// credential to the assistant (or to any configured upstream host).
func TestGatewayCredentialHeadersDoNotReachTheAssistant(t *testing.T) {
	var got http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	s := newTestServer(t, upstream.URL, "sk-assistant-key")
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[]}`))
	r.Header.Set("Authorization", "Bearer "+testToken) // host -> plugin hop only
	r.Header.Set("X-API-Key", "gw-client-key-1")
	r.Header.Set("api-key", "gw-client-key-2")
	r.Header.Set("X-Client-Key", "gw-client-key-3")
	r.Header.Set("X-Conversation-Id", "conv-keep") // ordinary header, forwarded
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("proxy = %d %s", rec.Code, rec.Body.String())
	}
	for _, h := range []string{"X-API-Key", "Api-Key", "X-Client-Key"} {
		if v := got.Get(h); v != "" {
			t.Fatalf("gateway credential header %s leaked upstream: %q", h, v)
		}
	}
	if v := got.Get("Authorization"); v != "Bearer sk-assistant-key" {
		t.Fatalf("upstream Authorization = %q, want the assistant's stored key", v)
	}
	if v := got.Get("X-Conversation-Id"); v != "conv-keep" {
		t.Fatalf("ordinary client header was dropped: %q", v)
	}
}
