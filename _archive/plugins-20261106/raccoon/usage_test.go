package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newUsageUpstream is a fake Raccoon chat endpoint that emits the final usage
// chunk exactly the way an OpenAI-shaped upstream does: only when the request
// carries stream_options.include_usage.
func newUsageUpstream(t *testing.T, gotBody *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if gotBody != nil {
			*gotBody = string(raw)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n"))
		if strings.Contains(string(raw), `"include_usage":true`) {
			_, _ = w.Write([]byte("data: {\"id\":\"c1\",\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n"))
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func postChat(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// TestNonStreamingChatAsksForAndReturnsUsage: a non-streaming client expects
// usage in the single chat.completion, but the plugin only sees SSE frames. It
// therefore has to ask upstream for the usage chunk it is going to aggregate,
// and aggregateSSE already knows how to carry it.
func TestNonStreamingChatAsksForAndReturnsUsage(t *testing.T) {
	var gotBody string
	upstream := newUsageUpstream(t, &gotBody)
	srv, h := newLoginTestServer(t, upstream.URL)
	if err := srv.store.set(credentials{AccessToken: "t", BaseURL: upstream.URL}); err != nil {
		t.Fatalf("set: %v", err)
	}

	rec := postChat(t, h, `{"model":"m","messages":[],"stream":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(gotBody, `"include_usage":true`) {
		t.Fatalf("aggregation request did not ask for usage: %s", gotBody)
	}
	var out struct {
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("aggregate is not JSON: %v", err)
	}
	if got, _ := out.Usage["total_tokens"].(float64); got != 5 {
		t.Fatalf("usage = %v, want total_tokens 5 from the upstream chunk: %s", out.Usage, rec.Body.String())
	}
}

// TestStreamingWithoutIncludeUsageStaysWithoutUsage mirrors OpenAI: a streaming
// client that did not request usage must not get a usage chunk, and the plugin
// must not silently add stream_options on its behalf.
func TestStreamingWithoutIncludeUsageStaysWithoutUsage(t *testing.T) {
	var gotBody string
	upstream := newUsageUpstream(t, &gotBody)
	srv, h := newLoginTestServer(t, upstream.URL)
	if err := srv.store.set(credentials{AccessToken: "t", BaseURL: upstream.URL}); err != nil {
		t.Fatalf("set: %v", err)
	}

	rec := postChat(t, h, `{"model":"m","messages":[],"stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat = %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(gotBody, "stream_options") {
		t.Fatalf("streaming request was rewritten with stream_options: %s", gotBody)
	}
	if strings.Contains(rec.Body.String(), `"usage"`) {
		t.Fatalf("relayed a usage chunk the client never asked for: %s", rec.Body.String())
	}
}

// TestStreamingUsageChunkIsRelayedWhenRequested: when the client asks for
// include_usage, the upstream's final usage chunk reaches it unchanged.
func TestStreamingUsageChunkIsRelayedWhenRequested(t *testing.T) {
	var gotBody string
	upstream := newUsageUpstream(t, &gotBody)
	srv, h := newLoginTestServer(t, upstream.URL)
	if err := srv.store.set(credentials{AccessToken: "t", BaseURL: upstream.URL}); err != nil {
		t.Fatalf("set: %v", err)
	}

	rec := postChat(t, h, `{"model":"m","messages":[],"stream":true,"stream_options":{"include_usage":true}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(gotBody, `"include_usage":true`) {
		t.Fatalf("client's include_usage was dropped: %s", gotBody)
	}
	if !strings.Contains(rec.Body.String(), `"total_tokens":5`) {
		t.Fatalf("usage chunk was not relayed: %s", rec.Body.String())
	}
}
