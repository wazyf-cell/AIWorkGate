package gateway

// task-60 单测：cached_tokens 落库链路——tap.Usage.cachedPrompt → rec.cached →
// emit 的 row.CachedTokens，非流式与 SSE 两路。这是路由面板「单条 cache 命中率」
// 的数据源头（usage 侧透出断言在 internal/usage/cached_tokens_test.go）。

import (
	"io"
	"net/http"
	"testing"

	"aiworkgate/internal/model"
	"aiworkgate/internal/scheduler"
)

func cachedTestGateway(t *testing.T, upstreamURL string, rows *[]model.APIRequestLog) *Gateway {
	t.Helper()
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), upstreamURL, "tok")
	g := newTestGateway(t, Options{
		Plugins:  reg,
		Log:      &fakeLogSink{},
		Schedule: scheduler.New(scheduler.Options{}),
		Snapshot: staticSnapshot(singlePoolSnapshot("p1", 2, "acc1")),
	})
	g.logs = sinkFunc(func(row model.APIRequestLog) { *rows = append(*rows, row) })
	return g
}

func TestCachedTokensRecordedNonStream(t *testing.T) {
	srv, calls := newUpstreamServer(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}],
			"usage":{"prompt_tokens":100,"completion_tokens":10,
			"prompt_tokens_details":{"cached_tokens":90}}}`)
	})
	var rows []model.APIRequestLog
	g := cachedTestGateway(t, srv.URL, &rows)

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"p1/m1","messages":[{"role":"user","content":"hi"}]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if calls.count() != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls.count())
	}
	if len(rows) != 1 {
		t.Fatalf("log rows = %d, want 1", len(rows))
	}
	if rows[0].CachedTokens != 90 || rows[0].PromptTokens != 100 || !rows[0].UsageKnown {
		t.Fatalf("非流式 cached_tokens 落库错误: cached=%d prompt=%d known=%v",
			rows[0].CachedTokens, rows[0].PromptTokens, rows[0].UsageKnown)
	}
}

func TestCachedTokensRecordedStream(t *testing.T) {
	// usage 挂在收尾帧（chatChunk.usage，tap 按 data: 行解析）——SSE 的 data
	// 行内不许有裸换行，usage 必须与 choices 同一行。
	srv, calls := newUpstreamServer(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+`{"choices":[{"index":0,"delta":{"content":"ok"}}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: "+`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":200,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":150}}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	var rows []model.APIRequestLog
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	g := newTestGateway(t, Options{
		Plugins:  reg,
		Log:      &fakeLogSink{},
		Schedule: scheduler.New(scheduler.Options{}),
		Snapshot: staticSnapshot(singlePoolSnapshot("p1", 2, "acc1")),
	})
	g.logs = sinkFunc(func(row model.APIRequestLog) { rows = append(rows, row) })

	body := `{"model":"p1/m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if calls.count() != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls.count())
	}
	if len(rows) != 1 {
		t.Fatalf("log rows = %d, want 1", len(rows))
	}
	if rows[0].CachedTokens != 150 || rows[0].PromptTokens != 200 || !rows[0].UsageKnown || !rows[0].Stream {
		t.Fatalf("SSE cached_tokens 落库错误: cached=%d prompt=%d known=%v stream=%v",
			rows[0].CachedTokens, rows[0].PromptTokens, rows[0].UsageKnown, rows[0].Stream)
	}
}
