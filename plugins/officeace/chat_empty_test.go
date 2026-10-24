package main

import (
	"io"
	"net/http"
	"testing"
)

func setupChat(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, body string)) http.Handler {
	t.Helper()
	maas := newFakeMaaS(t, handler)
	root := makeInstall(t, defaultSession(maas.URL()))
	_, h, _ := newTestPlugin(t, func(o *Options) { o.ForcedOfficeAceHome = installHome(t, root) })
	return h
}

// TestEmptyUpstreamSSENonStreamIsUpstreamError: an upstream 200 with no SSE
// events must not become a fabricated empty chat.completion.
func TestEmptyUpstreamSSENonStreamIsUpstreamError(t *testing.T) {
	h := setupChat(t, func(w http.ResponseWriter, r *http.Request, body string) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	})
	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{body: sampleChatBody("glm-5.3", "hi", nil)})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body %s", rec.Code, rec.Body.String())
	}
	if env := decodeAs[errorEnvelope](t, rec); env.Error.Type != "upstream_error" {
		t.Fatalf("error = %+v", env.Error)
	}
}

// TestEmptyUpstreamSSEStreamIsUpstreamError: streaming must fail before the
// first response byte; a 200 + empty SSE body looks like a successful answer.
func TestEmptyUpstreamSSEStreamIsUpstreamError(t *testing.T) {
	h := setupChat(t, func(w http.ResponseWriter, r *http.Request, body string) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	})
	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions",
		reqOpts{body: sampleChatBody("glm-5.3", "hi", map[string]any{"stream": true})})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("content-type = %q, want the JSON error", ct)
	}
	if env := decodeAs[errorEnvelope](t, rec); env.Error.Type != "upstream_error" {
		t.Fatalf("error = %+v", env.Error)
	}
}

// TestDoneOnlyUpstreamAggregateIsUpstreamError covers a stream that only sends
// the terminator: still no completion content.
func TestDoneOnlyUpstreamAggregateIsUpstreamError(t *testing.T) {
	h := setupChat(t, func(w http.ResponseWriter, r *http.Request, body string) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{body: sampleChatBody("glm-5.3", "hi", nil)})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body %s", rec.Code, rec.Body.String())
	}
	if env := decodeAs[errorEnvelope](t, rec); env.Error.Type != "upstream_error" {
		t.Fatalf("error = %+v", env.Error)
	}
}

// TestJSONUpstreamWithoutChoicesIsUpstreamError: an OpenAI-shaped JSON body
// with no choices is not a completion either.
func TestJSONUpstreamWithoutChoicesIsUpstreamError(t *testing.T) {
	h := setupChat(t, func(w http.ResponseWriter, r *http.Request, body string) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-empty","choices":[]}`)
	})
	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{body: sampleChatBody("glm-5.3", "hi", nil)})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body %s", rec.Code, rec.Body.String())
	}
	if env := decodeAs[errorEnvelope](t, rec); env.Error.Type != "upstream_error" {
		t.Fatalf("error = %+v", env.Error)
	}
}
