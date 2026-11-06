package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// capturedRequest is handed over a channel: the handler goroutine and the test
// goroutine would otherwise share the header map without synchronisation.
type capturedRequest struct {
	header http.Header
	query  string
}

func captureServer(t *testing.T) (*httptest.Server, <-chan capturedRequest) {
	t.Helper()
	ch := make(chan capturedRequest, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ch <- capturedRequest{header: r.Header.Clone(), query: r.URL.RawQuery}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, ch
}

// Billing is the electron desktop, not the cli that chats: the reference
// (upstream/client.go billingHeaders) sends a different identity set on
// /billing/balance, where X-Device-Mid is mandatory.
func TestBalanceSendsBillingIdentityHeaders(t *testing.T) {
	srv, ch := captureServer(t)

	c := newUpstreamClient()
	if _, err := c.balanceAt(context.Background(), srv.URL, "jwt-tok", "mid-123", ""); err != nil {
		t.Fatalf("balance: %v", err)
	}
	got := <-ch

	want := [][2]string{
		{"Authorization", "Bearer jwt-tok"},
		{"Accept", "*/*"},
		{"Accept-Language", "*"},
		{"User-Agent", "ZCode/" + billingVersion},
		{"HTTP-Referer", "https://zcode.z.ai"},
		{"X-Title", "Z Code@electron"},
		{"X-ZCode-App-Version", billingVersion},
		{"X-Os-Category", "windows"},
		{"X-Os-Version", "10.0.26200"},
		{"X-Platform", "win32-x64"},
		{"X-Release-Channel", "stable"},
		{"X-Client-Language", "en-US"},
		{"X-Client-Timezone", "America/New_York"},
		{"X-Device-Mid", "mid-123"},
		{"sec-fetch-mode", "cors"},
	}
	for _, kv := range want {
		if v := got.header.Get(kv[0]); v != kv[1] {
			t.Errorf("%s = %q, want %q", kv[0], v, kv[1])
		}
	}
	if id := got.header.Get("x-request-id"); !isUUIDv4(id) {
		t.Errorf("x-request-id = %q, want a v4 uuid", id)
	}
	// X-ZCode-Agent is a chat-only marker; billing must not claim it.
	if v := got.header.Get("X-ZCode-Agent"); v != "" {
		t.Errorf("X-ZCode-Agent = %q, want it absent on billing", v)
	}
	if got.query != "app_version="+billingVersion {
		t.Errorf("query = %q, want app_version=%s", got.query, billingVersion)
	}
}

// Chat keeps its own identity set; aligning billing must not leak into it.
func TestChatKeepsCLIIdentityHeaders(t *testing.T) {
	srv, ch := captureServer(t)

	c := newUpstreamClient()
	resp, err := c.messagesTo(context.Background(), srv.URL, "jwt-tok", "mid-123", []byte(`{}`), nil)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	_ = resp.Body.Close()
	got := <-ch

	want := [][2]string{
		{"Authorization", "Bearer jwt-tok"},
		{"HTTP-Referer", "https://zcode.z.ai/"},
		{"X-Title", "Z Code@cli"},
		{"X-ZCode-Agent", "glm"},
		{"X-Release-Channel", "production"},
		{"X-Client-Language", "en-001"},
		{"X-Client-Timezone", "Europe/Bucharest"},
		{"X-Device-Mid", "mid-123"},
	}
	for _, kv := range want {
		if v := got.header.Get(kv[0]); v != kv[1] {
			t.Errorf("%s = %q, want %q", kv[0], v, kv[1])
		}
	}
}

// isUUIDv4 reports the RFC-4122 v4 shape newUUID generates.
func isUUIDv4(s string) bool {
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' || s[14] != '4' {
		return false
	}
	for i, r := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}
