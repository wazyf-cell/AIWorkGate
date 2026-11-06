package main

import (
	"net/http"
	"strings"
	"testing"
)

// TestChatBodyUpToHostCapAccepted: the host forwards bodies up to 32MiB, so a
// 17MiB prompt (over the old 16MiB plugin cap) must reach the upstream.
func TestChatBodyUpToHostCapAccepted(t *testing.T) {
	if maxChatBodyBytes < 32<<20 {
		t.Fatalf("maxChatBodyBytes = %d, below the host bound %d", maxChatBodyBytes, 32<<20)
	}
	maas := newFakeMaaS(t, successSSE)
	root := makeInstall(t, defaultSession(maas.URL()))
	_, h, _ := newTestPlugin(t, func(o *Options) { o.ForcedOfficeAceHome = installHome(t, root) })

	content := strings.Repeat("x", 17<<20)
	body := sampleChatBody("glm-5.3", content, nil)
	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{body: body})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a %d byte body the host allows; body %s",
			rec.Code, len(body), truncate(rec.Body.String(), 300))
	}
	if reqs := maas.snapshot(); len(reqs) != 1 {
		t.Fatalf("upstream requests = %d", len(reqs))
	}
}

// TestChatBodyOverHostCapRejected guards the upper boundary: the cap exists,
// it is just aligned with the host instead of being half of it.
func TestChatBodyOverHostCapRejected(t *testing.T) {
	maas := newFakeMaaS(t, successSSE)
	root := makeInstall(t, defaultSession(maas.URL()))
	_, h, _ := newTestPlugin(t, func(o *Options) { o.ForcedOfficeAceHome = installHome(t, root) })

	body := sampleChatBody("glm-5.3", strings.Repeat("x", maxChatBodyBytes), nil)
	if len(body) <= maxChatBodyBytes {
		t.Fatalf("test body = %d, want > %d", len(body), maxChatBodyBytes)
	}
	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{body: body})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an oversized body; body %s", rec.Code, truncate(rec.Body.String(), 300))
	}
	if reqs := maas.snapshot(); len(reqs) != 0 {
		t.Fatalf("oversized body must not reach upstream, requests = %d", len(reqs))
	}
}
