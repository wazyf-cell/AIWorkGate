package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

const toolStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":10,"cache_read_input_tokens":5,"cache_creation_input_tokens":2,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"calling"}}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu1","name":"f","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"a\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"1}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":20}}

event: message_stop
data: {"type":"message_stop"}

`

// A streamed tool use arrives as content_block_start(type=tool_use) plus a run
// of input_json_delta fragments; both have to become OpenAI tool_calls deltas or
// the call disappears.
func TestStreamOpenAITranslatesToolCalls(t *testing.T) {
	rec := httptest.NewRecorder()
	srv := &server{}
	srv.streamOpenAI(rec, strings.NewReader(toolStream), "glm-5.3")
	chunks := sseChunks(t, rec.Body.String())

	var start, finish, usage map[string]any
	var args strings.Builder
	var content strings.Builder
	for _, chunk := range chunks {
		if u, ok := chunk["usage"].(map[string]any); ok {
			usage = u
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		if fr, _ := choice["finish_reason"].(string); fr != "" {
			finish = choice
		}
		delta, _ := choice["delta"].(map[string]any)
		if text, _ := delta["content"].(string); text != "" {
			content.WriteString(text)
		}
		calls, _ := delta["tool_calls"].([]any)
		for _, raw := range calls {
			call, _ := raw.(map[string]any)
			fn, _ := call["function"].(map[string]any)
			if name, _ := fn["name"].(string); name != "" {
				start = call
			}
			if fragment, _ := fn["arguments"].(string); fragment != "" {
				args.WriteString(fragment)
			}
		}
	}
	if start == nil || start["id"] != "tu1" {
		t.Fatalf("no tool call start delta: %s", rec.Body.String())
	}
	if start["index"] != float64(0) || start["type"] != "function" {
		t.Fatalf("tool call start = %v", start)
	}
	if got := args.String(); got != `{"a":1}` {
		t.Fatalf("arguments = %q, want {\"a\":1}", got)
	}
	if content.String() != "calling" {
		t.Fatalf("content = %q, want calling", content.String())
	}
	if finish == nil || finish["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v, want tool_calls", finish)
	}
	if usage == nil || usage["prompt_tokens"] != float64(17) ||
		usage["completion_tokens"] != float64(20) || usage["total_tokens"] != float64(37) {
		t.Fatalf("usage = %v, want prompt 17 completion 20 total 37", usage)
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("stream is missing the DONE marker: %s", rec.Body.String())
	}
}

// A tool that takes no arguments still needs a complete "{}" payload, otherwise
// the client cannot parse the call.
func TestStreamOpenAIToolCallWithoutArguments(t *testing.T) {
	stream := `event: message_start
data: {"type":"message_start","message":{"id":"m","usage":{"input_tokens":1}}}
event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tu2","name":"g"}}
event: content_block_stop
data: {"type":"content_block_stop","index":0}
event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}
event: message_stop
data: {"type":"message_stop"}

`
	rec := httptest.NewRecorder()
	srv := &server{}
	srv.streamOpenAI(rec, strings.NewReader(stream), "m")

	var args string
	for _, chunk := range sseChunks(t, rec.Body.String()) {
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
		calls, _ := delta["tool_calls"].([]any)
		for _, raw := range calls {
			fn, _ := raw.(map[string]any)["function"].(map[string]any)
			if fragment, _ := fn["arguments"].(string); fragment != "" {
				args = fragment
			}
		}
	}
	if args != "{}" {
		t.Fatalf("arguments = %q, want {}", args)
	}
}

func sseChunks(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("chunk is not JSON: %q", payload)
		}
		out = append(out, chunk)
	}
	return out
}
