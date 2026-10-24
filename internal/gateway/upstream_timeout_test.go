package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"aiworkgate/internal/model"
)

// withShortTimeout overrides one upstream liveness bound for a test.
func withShortTimeout(t *testing.T, target *time.Duration, value time.Duration) {
	t.Helper()
	old := *target
	*target = value
	t.Cleanup(func() { *target = old })
}

// TestUpstreamHeaderStallTimesOut covers a plugin that accepts the connection
// and never sends response headers: ResponseHeaderTimeout must fail the request
// instead of pinning the concurrency slot forever.
func TestUpstreamHeaderStallTimesOut(t *testing.T) {
	withShortTimeout(t, &upstreamResponseHeaderTimeout, 150*time.Millisecond)

	srv, _ := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		<-r.Context().Done()
	})
	reg := newFakeRegistry().add(pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "tok")
	g := newTestGateway(t, Options{Plugins: reg})

	start := time.Now()
	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p/chat","messages":[]}`))
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("header stall took %v, want the %v bound", elapsed, 150*time.Millisecond)
	}
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body %s)", rr.Code, rr.Body.String())
	}
}

// TestUpstreamSilentStreamTimesOut covers a reply that stalls after the headers
// (a silent SSE stream): the idle-read watchdog must cancel the request and the
// handler must return within the configured bound.
func TestUpstreamSilentStreamTimesOut(t *testing.T) {
	withShortTimeout(t, &upstreamIdleReadTimeout, 150*time.Millisecond)

	srv, _ := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	})
	reg := newFakeRegistry().add(pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "tok")
	logs := &fakeLogSink{}
	g := newTestGateway(t, Options{Plugins: reg, Log: logs})

	start := time.Now()
	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p/chat","stream":true,"messages":[]}`))
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("silent stream took %v, want the %v idle bound", elapsed, 150*time.Millisecond)
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want the headers already sent (body %s)", rr.Code, rr.Body.String())
	}
	rows := logs.wait(t, 1)
	if !strings.Contains(rows[0].Error, "idle timeout") {
		t.Fatalf("log error = %q, want the idle-timeout cause", rows[0].Error)
	}

	// The slot must be free again: the same plugin can take a new request.
	release, err := g.lim.acquire(t.Context(), "p", g.plugins.Concurrency("p"), 100*time.Millisecond)
	if err != nil {
		t.Fatalf("concurrency slot leaked after the stall: %v", err)
	}
	release()
}
