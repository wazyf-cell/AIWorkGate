package gateway

import (
	"net/http"
	"testing"

	"aiworkgate/internal/model"
	"aiworkgate/internal/scheduler"
)

// D-31: requested / upstream / served are distinct columns. The upstream here
// silently answers with a cheaper model, which is exactly what the column is
// for.
func TestLogCapturesThreeModelColumns(t *testing.T) {
	srv, _ := newUpstreamServer(t, jsonResponder(http.StatusOK,
		`{"model":"cheap-served","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	logs := &fakeLogSink{}
	g := newTestGateway(t, Options{
		Plugins: reg, Log: logs,
		Schedule: scheduler.New(scheduler.Options{}),
		Snapshot: staticSnapshot(singlePoolSnapshot("p1", 2, "acc1")),
	})

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p1/m1","messages":[{"role":"user","content":"hi"}]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rr.Code, rr.Body.String())
	}
	rows := logs.wait(t, 1)
	row := rows[0]
	if row.ModelRequested != "p1/m1" || row.ModelUpstream != "m1" || row.ModelServed != "cheap-served" {
		t.Fatalf("columns = requested %q upstream %q served %q",
			row.ModelRequested, row.ModelUpstream, row.ModelServed)
	}
}

func TestLogServedModelFromSSE(t *testing.T) {
	srv, _ := newUpstreamServer(t, sseResponder([]string{
		`data: {"model":"served-sse","choices":[{"delta":{"content":"hi"}}]}` + "\n\n",
		"data: [DONE]\n\n",
	}))
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	logs := &fakeLogSink{}
	g := newTestGateway(t, Options{
		Plugins: reg, Log: logs,
		Schedule: scheduler.New(scheduler.Options{}),
		Snapshot: staticSnapshot(singlePoolSnapshot("p1", 2, "acc1")),
	})

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p1/m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rr.Code, rr.Body.String())
	}
	rows := logs.wait(t, 1)
	if rows[0].ModelServed != "served-sse" {
		t.Fatalf("served = %q, want served-sse", rows[0].ModelServed)
	}
}
