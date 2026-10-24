package gateway

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"aiworkgate/internal/model"
)

func TestAPILogNonStreamFieldsAndUsage(t *testing.T) {
	const respBody = `{"id":"x","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":22}}`
	srv, _ := newUpstreamServer(t, jsonResponder(http.StatusOK, respBody))
	reg := newFakeRegistry().add(
		pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "tok")
	logs := &fakeLogSink{}
	g := newTestGateway(t, Options{Plugins: reg, Log: logs})

	base := time.Now()
	g.now = fakeClock(base, 123*time.Millisecond)

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"p/chat","messages":[]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if got := rr.Body.String(); got != respBody {
		t.Fatalf("body = %q", got)
	}

	rows := logs.wait(t, 1)
	row := rows[0]
	if row.ReqID == "" {
		t.Fatal("req_id is empty")
	}
	if _, err := time.Parse(time.RFC3339, row.TS); err != nil {
		t.Fatalf("ts %q is not RFC3339: %v", row.TS, err)
	}
	if row.Source != "plugin:p" {
		t.Fatalf("source = %q, want plugin:p", row.Source)
	}
	if row.Model != "p/chat" {
		t.Fatalf("model = %q, want the prefixed client model", row.Model)
	}
	if row.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200", row.Status)
	}
	if row.Stream {
		t.Fatal("stream = true for a non-stream request")
	}
	if row.LatencyMS != 123 {
		t.Fatalf("latency = %dms, want 123ms", row.LatencyMS)
	}
	if row.TTFBMS < 0 {
		t.Fatalf("ttfb = %dms", row.TTFBMS)
	}
	if row.KeyID != "key-1" || row.KeyName != "default" {
		t.Fatalf("key fields = %q/%q", row.KeyID, row.KeyName)
	}
	if row.PromptTokens != 11 || row.CompletionTokens != 22 {
		t.Fatalf("usage = %d/%d, want 11/22", row.PromptTokens, row.CompletionTokens)
	}
	if row.Error != "" {
		t.Fatalf("error = %q, want empty", row.Error)
	}
	if row.BodyReq != "" || row.BodyResp != "" {
		t.Fatalf("debug bodies logged with DebugBody off: %q / %q", row.BodyReq, row.BodyResp)
	}
}

func TestAPILogDebugBodiesCapped(t *testing.T) {
	padding := strings.Repeat("y", 5000)
	respBody := fmt.Sprintf(`{"id":"x","usage":{"prompt_tokens":7,"completion_tokens":9},"pad":%q}`, padding)
	srv, calls := newUpstreamServer(t, jsonResponder(http.StatusOK, respBody))
	reg := newFakeRegistry().add(
		pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "tok")
	logs := &fakeLogSink{}
	g := newTestGateway(t, Options{
		Plugins:   reg,
		Log:       logs,
		DebugBody: true,
		BodyCap:   64,
	})

	reqBody := fmt.Sprintf(`{"model":"p/chat","messages":[{"role":"user","content":%q}],"pad":%q}`,
		"hello", strings.Repeat("x", 5000))
	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, reqBody))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if got := rr.Body.String(); got != respBody {
		t.Fatalf("debug capping must not truncate the client response (got %d bytes, want %d)",
			len(got), len(respBody))
	}

	rows := logs.wait(t, 1)
	row := rows[0]
	if len(row.BodyReq) != requestPreviewBytes {
		t.Fatalf("body_req length = %d, want %d", len(row.BodyReq), requestPreviewBytes)
	}
	if !strings.Contains(row.BodyReq, `"model":"p/chat"`) {
		t.Fatalf("body_req should capture the client body, got prefix %q", row.BodyReq[:min(80, len(row.BodyReq))])
	}
	if len(row.BodyResp) != 64 {
		t.Fatalf("body_resp length = %d, want 64 (BodyCap)", len(row.BodyResp))
	}
	if row.PromptTokens != 7 || row.CompletionTokens != 9 {
		t.Fatalf("usage = %d/%d, want 7/9 (tap must parse the full body)", row.PromptTokens, row.CompletionTokens)
	}

	got := calls.last(t)
	if len(got.Body) <= requestPreviewBytes {
		t.Fatalf("plugin received %d bytes, expected the full rewritten body", len(got.Body))
	}
	if !strings.Contains(string(got.Body), `"model":"chat"`) {
		t.Fatalf("plugin model was not rewritten: %s", got.Body[:min(120, len(got.Body))])
	}
}

func TestAPILogUpstreamErrorStatus(t *testing.T) {
	srv, _ := newUpstreamServer(t, jsonResponder(http.StatusInternalServerError,
		`{"error":{"message":"boom","type":"server_error"}}`))
	reg := newFakeRegistry().add(
		pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "tok")
	logs := &fakeLogSink{}
	g := newTestGateway(t, Options{Plugins: reg, Log: logs})

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"p/chat"}`))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	rows := logs.wait(t, 1)
	row := rows[0]
	if row.Status != http.StatusInternalServerError {
		t.Fatalf("logged status = %d, want 500", row.Status)
	}
	if row.Source != "plugin:p" || row.Model != "p/chat" {
		t.Fatalf("unexpected log row: %+v", row)
	}
	if !strings.Contains(row.Error, "boom") {
		t.Fatalf("error summary = %q, want upstream message", row.Error)
	}
	if row.PromptTokens != 0 || row.CompletionTokens != 0 {
		t.Fatalf("unexpected tokens on error: %+v", row)
	}
}

func TestAPILogStreamFlag(t *testing.T) {
	srv, _ := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	reg := newFakeRegistry().add(
		pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "tok")
	logs := &fakeLogSink{}
	g := newTestGateway(t, Options{Plugins: reg, Log: logs})

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p/chat","stream":true,"messages":[]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	rows := logs.wait(t, 1)
	if !rows[0].Stream {
		t.Fatalf("stream flag not logged: %+v", rows[0])
	}
	if rows[0].Status != http.StatusOK || rows[0].Source != "plugin:p" {
		t.Fatalf("unexpected log row: %+v", rows[0])
	}
}

func TestSetDebugTogglesCapture(t *testing.T) {
	g := newGateway(Options{})
	if g.debugBody.Load() {
		t.Fatal("debug capture must default to off")
	}
	g.SetDebug(true)
	if !g.debugBody.Load() {
		t.Fatal("SetDebug(true) did not enable capture")
	}
	g.SetDebug(false)
	if g.debugBody.Load() {
		t.Fatal("SetDebug(false) did not disable capture")
	}
}
