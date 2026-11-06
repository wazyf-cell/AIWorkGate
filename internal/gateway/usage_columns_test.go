package gateway

import (
	"net/http"
	"testing"
)

func TestTapParsesCacheWriteAndReasoning(t *testing.T) {
	tap := newBodyTap(1024, false)
	tap.feed([]byte(`{"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":100,` +
		`"cache_creation_input_tokens":3000,"cache_creation":{"ephemeral_5m_input_tokens":1000,"ephemeral_1h_input_tokens":2000},` +
		`"magpie_cache_unknown":true,"completion_tokens_details":{"reasoning_tokens":77}}}`))
	tap.finish()
	if tap.cacheWrite != 3000 || tap.cacheWrite1h != 2000 || !tap.cacheUnknown || tap.reasoning != 77 {
		t.Fatalf("tap = write %d 1h %d unknown %v reasoning %d",
			tap.cacheWrite, tap.cacheWrite1h, tap.cacheUnknown, tap.reasoning)
	}
}

func TestLogRecordsCacheReasoningAndPurpose(t *testing.T) {
	srv, _ := newUpstreamServer(t, jsonResponder(http.StatusOK,
		`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":10,"completion_tokens":5,"cache_creation_input_tokens":300,`+
			`"cache_creation":{"ephemeral_1h_input_tokens":120},"completion_tokens_details":{"reasoning_tokens":9}}}`))
	logs := &fakeLogSink{}
	g := newTestGateway(t, Options{A: &fakeA{base: srv.URL, key: "sk-a"}, Log: logs})

	req := newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	req.Header.Set("X-Aiworkgate-Purpose", "batch")
	serve(g, req)
	row := logs.wait(t, 1)[0]
	if row.CacheWrite != 300 || row.CacheWrite1h != 120 || row.ReasoningTokens != 9 || row.Purpose != "batch" {
		t.Fatalf("row = write %d 1h %d reasoning %d purpose %q",
			row.CacheWrite, row.CacheWrite1h, row.ReasoningTokens, row.Purpose)
	}

	// No header: the endpoint name is the purpose.
	serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	rows := logs.wait(t, 2)
	if rows[1].Purpose != "chat" {
		t.Fatalf("purpose = %q, want chat", rows[1].Purpose)
	}
}
