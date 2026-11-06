package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// The relay converter and the aggregation. These are the parts a wrong port
// breaks silently: a mis-shaped chunk is still valid JSON, and the client only
// notices as garbled text.

func collectConverter(t *testing.T, runID, session string, body []byte, frames []string) (*relayConverter, *listWriter) {
	t.Helper()
	lw := &listWriter{}
	cv := newRelayConverter(runID, session, body, lw)
	for _, f := range frames {
		cv.handleFrame(f)
	}
	if !cv.done {
		cv.finish()
	}
	return cv, lw
}

func TestRelayConverterEmitsTextDeltasOnly(t *testing.T) {
	body := []byte(`{"model":"zai_auto","messages":[{"role":"user","content":"hi"}]}`)
	frames := []string{
		`event: x\ndata: {"event":"agent:stream","payload":{"runId":"r1","type":"text","text":"Hel"}}`,
		`data: {"event":"agent:stream","payload":{"runId":"r1","type":"text","text":"Hello"}}`,
		`data: {"event":"agent:stream","payload":{"runId":"r1","type":"text","text":"Hello world"}}`,
		`data: {"event":"agent:stream","payload":{"runId":"r1","type":"done","usage":{"input":3,"output":2,"total":5}}}`,
	}
	// The frame helper expects real newlines, not escapes.
	fixed := make([]string, len(frames))
	for i, f := range frames {
		fixed[i] = strings.ReplaceAll(f, `\n`, "\n")
	}
	_, lw := collectConverter(t, "r1", "main", body, fixed)

	var content strings.Builder
	var sawDone, sawUsage bool
	var prompt, completion, total float64
	for _, line := range lw.snapshot() {
		line = strings.TrimRight(line, "\n")
		if line == "data: [DONE]" {
			sawDone = true
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var chunk struct {
			Object  string `json:"object"`
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason any `json:"finish_reason"`
			} `json:"choices"`
			Usage map[string]any `json:"usage"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			t.Fatalf("decode chunk %q: %v", line, err)
		}
		if chunk.Object != "chat.completion.chunk" {
			t.Fatalf("object = %q", chunk.Object)
		}
		for _, c := range chunk.Choices {
			content.WriteString(c.Delta.Content)
		}
		if chunk.Usage != nil {
			sawUsage = true
			prompt = chunk.Usage["prompt_tokens"].(float64)
			completion = chunk.Usage["completion_tokens"].(float64)
			total = chunk.Usage["total_tokens"].(float64)
		}
	}
	// Cumulative text must be reduced to the missing suffix, or the reply is
	// duplicated.
	if got := content.String(); got != "Hello world" {
		t.Fatalf("content = %q, want Hello world", got)
	}
	if !sawDone {
		t.Fatal("the stream must end with data: [DONE]")
	}
	if !sawUsage || prompt != 3 || completion != 2 || total != 5 {
		t.Fatalf("usage = %v/%v/%v, want 3/2/5", prompt, completion, total)
	}
}

func TestRelayConverterIgnoresOtherRuns(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	frames := []string{
		// A different run with a different session must not leak into this one.
		`data: {"event":"agent:stream","payload":{"runId":"other","type":"text","text":"nope"}}`,
		// A same-session event with no runId is accepted.
		`data: {"event":"agent:stream","payload":{"sessionKey":"main","type":"text","text":"yes"}}`,
	}
	_, lw := collectConverter(t, "r1", "main", body, frames)
	out := strings.Join(lw.snapshot(), "")
	if strings.Contains(out, "nope") {
		t.Fatal("another run's text leaked into the response")
	}
	if !strings.Contains(out, "yes") {
		t.Fatal("a same-session event was dropped")
	}
}

func TestRelayConverterEmitsToolCallsOnce(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	frames := []string{
		`data: {"event":"chat","payload":{"runId":"r1","message":{"toolCalls":[{"id":"t1","name":"get_time","arguments":{"tz":"utc"}}]}}}`,
		// The same tool id appearing again must not be emitted twice.
		`data: {"event":"chat","payload":{"runId":"r1","message":{"toolCalls":[{"id":"t1","name":"get_time","arguments":{"tz":"utc"}}]}}}`,
		`data: {"event":"chat","payload":{"runId":"r1","state":"final","usage":{"input":1,"output":1,"total":2}}}`,
	}
	_, lw := collectConverter(t, "r1", "main", body, frames)
	toolChunks := 0
	for _, line := range lw.snapshot() {
		if !strings.Contains(line, "tool_calls") {
			continue
		}
		toolChunks++
		if !strings.Contains(line, "get_time") {
			t.Fatalf("tool chunk lost the name: %s", line)
		}
	}
	if toolChunks != 1 {
		t.Fatalf("tool_calls chunks = %d, want 1 (deduped by id)", toolChunks)
	}
}

func TestRelayConverterSurfacesErrorAsContent(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	frames := []string{
		`data: {"event":"agent:stream","payload":{"runId":"r1","type":"error","message":"余额不足"}}`,
	}
	_, lw := collectConverter(t, "r1", "main", body, frames)
	out := strings.Join(lw.snapshot(), "")
	if !strings.Contains(out, "余额不足") {
		t.Fatalf("the upstream error message was lost: %s", out)
	}
	// An error must still terminate the stream.
	if !strings.Contains(out, "[DONE]") {
		t.Fatal("an error stream must still end with [DONE]")
	}
}

func TestAggregateSSEFoldsContentUsageAndTools(t *testing.T) {
	lines := []string{
		`data: {"id":"chatcmpl-1","model":"zai_auto","created":1700000000,"choices":[{"index":0,"delta":{"role":"assistant","content":"a"},"finish_reason":null}]}`,
		`data: {"choices":[{"index":0,"delta":{"content":"b"},"finish_reason":null}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"t1","type":"function","function":{"name":"f","arguments":"{\"a\":"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`,
		`data: [DONE]`,
	}
	got := aggregateSSE(lines)
	if got["object"] != "chat.completion" {
		t.Fatalf("object = %v", got["object"])
	}
	choices, _ := got["choices"].([]map[string]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %v", choices)
	}
	msg, _ := choices[0]["message"].(map[string]any)
	if msg["content"] != "ab" {
		t.Fatalf("content = %v, want ab", msg["content"])
	}
	if choices[0]["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v", choices[0]["finish_reason"])
	}
	tcs, _ := msg["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls = %v", tcs)
	}
	tc, _ := tcs[0].(map[string]any)
	fn, _ := tc["function"].(map[string]any)
	// Fragmentary arguments must be concatenated, not replaced.
	if fn["arguments"] != `{"a":1}` {
		t.Fatalf("arguments = %v, want the concatenation", fn["arguments"])
	}
	if got["usage"] == nil {
		t.Fatal("usage must survive aggregation")
	}
}

func TestAggregateSSEOfEmptyStreamStillYieldsACompletion(t *testing.T) {
	got := aggregateSSE([]string{`data: [DONE]`})
	if got["object"] != "chat.completion" {
		t.Fatalf("object = %v", got["object"])
	}
	choices, _ := got["choices"].([]map[string]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %v", choices)
	}
	msg, _ := choices[0]["message"].(map[string]any)
	if msg["role"] != "assistant" || msg["content"] != "" {
		t.Fatalf("message = %v", msg)
	}
}

func TestExtractUsageVariants(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want bool
	}{
		{"nested usage", map[string]any{"usage": map[string]any{"input": 1}}, true},
		{"flat", map[string]any{"prompt_tokens": 1}, true},
		{"result", map[string]any{"result": map[string]any{"output": 2}}, true},
		{"absent", map[string]any{"text": "x"}, false},
	}
	for _, tc := range cases {
		if got := extractUsage(tc.in) != nil; got != tc.want {
			t.Errorf("%s: extractUsage != nil = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestPayloadMsgPrefersReadableField(t *testing.T) {
	if got := payloadMsg(map[string]any{"message": "boom", "code": 5}); got != "boom" {
		t.Fatalf("got %q", got)
	}
	// With no readable field it must still say something actionable.
	got := payloadMsg(map[string]any{"type": "error", "code": "E1"})
	if !strings.Contains(got, "code=E1") {
		t.Fatalf("got %q, want the code included", got)
	}
	if got := payloadMsg(map[string]any{}); !strings.Contains(got, "no detail") {
		t.Fatalf("got %q", got)
	}
}

func TestSingleChunkSSEWrapsJSONCompletion(t *testing.T) {
	full := map[string]any{
		"id": "c1", "created": float64(1700000000), "model": "zai_auto",
		"choices": []any{map[string]any{
			"index":   0,
			"message": map[string]any{"role": "assistant", "content": "hi"},
		}},
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
	}
	lw := &listWriter{}
	singleChunkSSE(full, lw)
	snap := lw.snapshot()
	if len(snap) != 2 {
		t.Fatalf("frames = %d, want chunk + [DONE]: %v", len(snap), snap)
	}
	if !strings.Contains(snap[0], "chat.completion.chunk") {
		t.Fatalf("first frame = %s", snap[0])
	}
	if !strings.Contains(snap[0], `"content":"hi"`) {
		t.Fatalf("the message content was not moved into delta: %s", snap[0])
	}
	if !strings.Contains(snap[1], "[DONE]") {
		t.Fatalf("second frame = %s", snap[1])
	}
}
