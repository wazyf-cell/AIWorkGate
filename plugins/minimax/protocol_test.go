package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// The protocol layer is the part most likely to break silently: a wrong mapping
// still produces valid JSON, so only assertions catch it.

func TestOpenAIToAnthropicHoistsSystemAsString(t *testing.T) {
	in := []byte(`{
		"model": "MiniMax-M3",
		"stream": false,
		"messages": [
			{"role":"system","content":"you are terse"},
			{"role":"user","content":"hi"}
		]
	}`)
	out, err := openAIToAnthropicWith(in, reasoningParams{})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// MiniMax takes system as a plain string, not the Anthropic block array.
	sys, ok := got["system"].(string)
	if !ok {
		t.Fatalf("system must be a string, got %T (%s)", got["system"], out)
	}
	if sys != "you are terse" {
		t.Fatalf("system = %q", sys)
	}
	// An absent max_tokens must stay absent: MiniMax does not require it, and
	// inventing one would silently cap the answer.
	if _, present := got["max_tokens"]; present {
		t.Fatalf("max_tokens must not be invented: %s", out)
	}
	// The upstream only speaks streaming, so stream must be forced on.
	if got["stream"] != true {
		t.Fatalf("stream = %v, want true", got["stream"])
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v, want only the user turn", msgs)
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "user" {
		t.Fatalf("role = %v, want user", first["role"])
	}
}

func TestOpenAIToAnthropicJoinsAndDedupesSystem(t *testing.T) {
	in := []byte(`{
		"model": "m",
		"messages": [
			{"role":"system","content":"a"},
			{"role":"system","content":"a"},
			{"role":"developer","content":"b"},
			{"role":"user","content":"x"}
		]
	}`)
	out, _ := openAIToAnthropicWith(in, reasoningParams{})
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	// dedupeStrings drops the repeat, and the join is a blank line.
	if got["system"] != "a\n\nb" {
		t.Fatalf("system = %q, want %q", got["system"], "a\n\nb")
	}
}

func TestOpenAIToAnthropicKeepsExplicitMaxTokensAndStop(t *testing.T) {
	in := []byte(`{"model":"m","max_tokens":512,"stop":["END"],"messages":[{"role":"user","content":"x"}]}`)
	out, _ := openAIToAnthropicWith(in, reasoningParams{})
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	if got["max_tokens"] != float64(512) {
		t.Fatalf("max_tokens = %v, want 512", got["max_tokens"])
	}
	stops, _ := got["stop_sequences"].([]any)
	if len(stops) != 1 || stops[0] != "END" {
		t.Fatalf("stop_sequences = %v, want [END]", got["stop_sequences"])
	}
}

func TestOpenAIToAnthropicMapsToolsWithoutTypeWrapper(t *testing.T) {
	in := []byte(`{
		"model":"m",
		"messages":[{"role":"user","content":"go"}],
		"tools":[{"type":"function","function":{"name":"f","description":"d","parameters":{"type":"object","properties":{"a":{"type":"string"}}}}}]
	}`)
	out, _ := openAIToAnthropicWith(in, reasoningParams{})
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	tools, _ := got["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v, want 1", tools)
	}
	tool, _ := tools[0].(map[string]any)
	// OpenAI calls it parameters, MiniMax calls it input_schema.
	if _, ok := tool["input_schema"]; !ok {
		t.Fatalf("input_schema missing: %v", tool)
	}
	if _, ok := tool["parameters"]; ok {
		t.Fatalf("parameters must not be sent upstream: %v", tool)
	}
	// The reference sends exactly name/description/input_schema; a "type" key
	// at this level is not part of the shape.
	if _, ok := tool["type"]; ok {
		t.Fatalf("tools must not carry a type wrapper: %v", tool)
	}
	if tool["name"] != "f" {
		t.Fatalf("tool name = %v", tool["name"])
	}
}

func TestOpenAIToAnthropicMapsToolResults(t *testing.T) {
	in := []byte(`{
		"model":"m",
		"messages":[
			{"role":"user","content":"q"},
			{"role":"assistant","content":"","tool_calls":[{"id":"t1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]},
			{"role":"tool","tool_call_id":"t1","content":"done"}
		]
	}`)
	out, _ := openAIToAnthropicWith(in, reasoningParams{})
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3", len(msgs))
	}
	assistant, _ := msgs[1].(map[string]any)
	blocks, _ := assistant["content"].([]any)
	found := false
	for _, b := range blocks {
		blk, _ := b.(map[string]any)
		if blk["type"] == "tool_use" {
			found = true
			if blk["id"] != "t1" || blk["name"] != "f" {
				t.Fatalf("tool_use block = %v", blk)
			}
			if _, ok := blk["input"].(map[string]any); !ok {
				t.Fatalf("tool_use input must be an object, got %T", blk["input"])
			}
		}
	}
	if !found {
		t.Fatalf("no tool_use block in %v", blocks)
	}
	toolMsg, _ := msgs[2].(map[string]any)
	if toolMsg["role"] != "user" {
		t.Fatalf("tool role = %v, want user", toolMsg["role"])
	}
	tblocks, _ := toolMsg["content"].([]any)
	tb, _ := tblocks[0].(map[string]any)
	if tb["type"] != "tool_result" || tb["tool_use_id"] != "t1" {
		t.Fatalf("tool_result block = %v", tb)
	}
}

// A malformed arguments string must degrade to {} and never invent parameters.
func TestOpenAIToAnthropicDegradesMalformedToolArguments(t *testing.T) {
	in := []byte(`{
		"model":"m",
		"messages":[
			{"role":"user","content":"q"},
			{"role":"assistant","content":"","tool_calls":[{"id":"t1","type":"function","function":{"name":"f","arguments":"{not json"}}]}
		]
	}`)
	out, _ := openAIToAnthropicWith(in, reasoningParams{})
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	msgs, _ := got["messages"].([]any)
	blocks, _ := msgs[1].(map[string]any)["content"].([]any)
	for _, b := range blocks {
		blk, _ := b.(map[string]any)
		if blk["type"] == "tool_use" {
			input, ok := blk["input"].(map[string]any)
			if !ok || len(input) != 0 {
				t.Fatalf("input = %v, want an empty object", blk["input"])
			}
			return
		}
	}
	t.Fatalf("no tool_use block in %v", blocks)
}

func TestOpenAIToAnthropicConvertsDataURLImage(t *testing.T) {
	in := []byte(`{
		"model":"m",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"look"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}
		]}]
	}`)
	out, _ := openAIToAnthropicWith(in, reasoningParams{})
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	msgs, _ := got["messages"].([]any)
	blocks, _ := msgs[0].(map[string]any)["content"].([]any)
	var img map[string]any
	for _, b := range blocks {
		blk, _ := b.(map[string]any)
		if blk["type"] == "image" {
			img, _ = blk["source"].(map[string]any)
		}
	}
	if img == nil {
		t.Fatalf("no image block in %v", blocks)
	}
	if img["type"] != "base64" || img["media_type"] != "image/png" || img["data"] != "QUJD" {
		t.Fatalf("image source = %v", img)
	}
}

// MiniMax reasons by mode (thinking) and, optionally, by tier (output_config).
func TestApplyThinkingModes(t *testing.T) {
	base := []byte(`{"model":"m","messages":[{"role":"user","content":"x"}]}`)

	t.Run("explicit disabled passthrough", func(t *testing.T) {
		out, _ := openAIToAnthropicWith(base, reasoningParams{
			Thinking: json.RawMessage(`{"type":"disabled"}`),
		})
		var got map[string]any
		_ = json.Unmarshal(out, &got)
		th, _ := got["thinking"].(map[string]any)
		if th["type"] != "disabled" {
			t.Fatalf("thinking = %v, want disabled", got["thinking"])
		}
	})
	t.Run("effort becomes adaptive plus output_config", func(t *testing.T) {
		out, _ := openAIToAnthropicWith(base, reasoningParams{Effort: "high"})
		var got map[string]any
		_ = json.Unmarshal(out, &got)
		th, _ := got["thinking"].(map[string]any)
		if th["type"] != "adaptive" {
			t.Fatalf("thinking = %v, want adaptive", got["thinking"])
		}
		oc, _ := got["output_config"].(map[string]any)
		if oc["effort"] != "high" {
			t.Fatalf("output_config = %v, want effort high", got["output_config"])
		}
	})
	t.Run("no reasoning means no keys", func(t *testing.T) {
		out, _ := openAIToAnthropicWith(base, reasoningParams{})
		var got map[string]any
		_ = json.Unmarshal(out, &got)
		if _, ok := got["thinking"]; ok {
			t.Fatalf("thinking must be absent: %s", out)
		}
		if _, ok := got["output_config"]; ok {
			t.Fatalf("output_config must be absent: %s", out)
		}
	})
}

// A truncated body must be rejected, not silently converted into an empty
// request that would bill the operator for nothing.
func TestOpenAIToAnthropicRejectsMalformedJSON(t *testing.T) {
	if _, err := openAIToAnthropicWith([]byte(`{"model":`), reasoningParams{}); err == nil {
		t.Fatal("expected an error for truncated JSON")
	}
}

// buildSSE renders frames the way the upstream does: named events are optional,
// every payload rides a data: line.
func buildSSE(frames ...string) string {
	var b strings.Builder
	for _, f := range frames {
		b.WriteString("data: " + f + "\n\n")
	}
	return b.String()
}

func TestPumpTranslatesTextStream(t *testing.T) {
	body := buildSSE(
		`{"type":"message_start","message":{"usage":{"input_tokens":11}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hel"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
		`{"type":"message_stop"}`,
	)
	var got []map[string]any
	tl := newTranslator(func(delta map[string]any, finish any) {
		got = append(got, map[string]any{"delta": delta, "finish": finish})
	})
	stop, err := pump(strings.NewReader(body), tl)
	if err != nil {
		t.Fatalf("pump: %v", err)
	}
	if stop != "end_turn" {
		t.Fatalf("stop = %q, want end_turn", stop)
	}
	completion := completionFromState(tl, "MiniMax-M3")
	var decoded struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			Prompt     int `json:"prompt_tokens"`
			Completion int `json:"completion_tokens"`
			Total      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(completion, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Object != "chat.completion" {
		t.Fatalf("object = %q", decoded.Object)
	}
	if decoded.Choices[0].Message.Content != "hello" {
		t.Fatalf("content = %q, want hello", decoded.Choices[0].Message.Content)
	}
	if decoded.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q, want stop", decoded.Choices[0].FinishReason)
	}
	if decoded.Usage.Prompt != 11 || decoded.Usage.Completion != 5 || decoded.Usage.Total != 16 {
		t.Fatalf("usage = %+v", decoded.Usage)
	}
}

func TestPumpAssemblesToolUseFromJsonDeltas(t *testing.T) {
	body := buildSSE(
		`{"type":"message_start","message":{"usage":{"input_tokens":3}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tu1","name":"f"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a\":"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"1}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		`{"type":"message_stop"}`,
	)
	tl := newTranslator(func(map[string]any, any) {})
	if _, err := pump(strings.NewReader(body), tl); err != nil {
		t.Fatalf("pump: %v", err)
	}
	var decoded struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(completionFromState(tl, "m"), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	tc := decoded.Choices[0].Message.ToolCalls
	if len(tc) != 1 || tc[0].ID != "tu1" || tc[0].Function.Name != "f" {
		t.Fatalf("tool_calls = %+v", tc)
	}
	// The fragments must be re-assembled into one JSON object.
	if tc[0].Function.Arguments != `{"a":1}` {
		t.Fatalf("arguments = %q", tc[0].Function.Arguments)
	}
	if decoded.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", decoded.Choices[0].FinishReason)
	}
}

// An in-stream error frame must surface as an error, not as an empty answer.
func TestPumpSurfacesInStreamError(t *testing.T) {
	body := buildSSE(`{"type":"error","error":{"message":"boom"}}`)
	tl := newTranslator(func(map[string]any, any) {})
	_, err := pump(strings.NewReader(body), tl)
	if err == nil {
		t.Fatal("expected an error for an error frame")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error = %v, want it to carry the upstream message", err)
	}
}

func TestMapStopReason(t *testing.T) {
	cases := map[string]string{
		"max_tokens": "length",
		"tool_use":   "tool_calls",
		"end_turn":   "stop",
		"refusal":    "stop",
		"":           "stop",
	}
	for in, want := range cases {
		if got := mapStopReason(in); got != want {
			t.Fatalf("mapStopReason(%q) = %q, want %q", in, got, want)
		}
	}
}
