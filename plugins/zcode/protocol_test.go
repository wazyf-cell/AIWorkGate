package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// The protocol layer is the part most likely to break silently: a wrong mapping
// still produces valid JSON, so only assertions catch it.

func TestOpenAIToAnthropicHoistsSystemAndDefaultsMaxTokens(t *testing.T) {
	in := []byte(`{
		"model": "GLM-5.3",
		"stream": false,
		"messages": [
			{"role":"system","content":"you are terse"},
			{"role":"user","content":"hi"}
		]
	}`)
	out, err := openAIToAnthropic(in)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Anthropic rejects a system role inside messages, so it must be hoisted.
	if _, ok := got["system"]; !ok {
		t.Fatalf("system block missing: %s", out)
	}
	// Anthropic requires max_tokens; OpenAI omits it.
	if got["max_tokens"] != float64(4096) {
		t.Fatalf("max_tokens = %v, want the 4096 default", got["max_tokens"])
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

func TestOpenAIToAnthropicKeepsExplicitMaxTokensAndStop(t *testing.T) {
	in := []byte(`{"model":"m","max_tokens":512,"stop":["END"],"messages":[{"role":"user","content":"x"}]}`)
	out, _ := openAIToAnthropic(in)
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

func TestOpenAIToAnthropicMapsTools(t *testing.T) {
	in := []byte(`{
		"model":"m",
		"messages":[{"role":"user","content":"go"}],
		"tools":[{"type":"function","function":{"name":"f","description":"d","parameters":{"type":"object","properties":{"a":{"type":"string"}}}}}]
	}`)
	out, _ := openAIToAnthropic(in)
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	tools, _ := got["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v, want 1", tools)
	}
	tool, _ := tools[0].(map[string]any)
	// OpenAI calls it parameters, Anthropic calls it input_schema.
	if _, ok := tool["input_schema"]; !ok {
		t.Fatalf("input_schema missing: %v", tool)
	}
	if _, ok := tool["parameters"]; ok {
		t.Fatalf("parameters must not be sent to Anthropic: %v", tool)
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
	out, _ := openAIToAnthropic(in)
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
	// The tool role must become a user turn carrying tool_result.
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

func TestOpenAIToAnthropicConvertsDataURLImage(t *testing.T) {
	in := []byte(`{
		"model":"m",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"look"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}
		]}]
	}`)
	out, _ := openAIToAnthropic(in)
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

func TestAnthropicToOpenAIMapsTextUsageAndStop(t *testing.T) {
	body := []byte(`{
		"id":"msg_1",
		"stop_reason":"end_turn",
		"content":[{"type":"text","text":"hello"}],
		"usage":{"input_tokens":7,"output_tokens":3}
	}`)
	out, err := anthropicToOpenAI(body, "GLM-5.3")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var got struct {
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
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Object != "chat.completion" {
		t.Fatalf("object = %q", got.Object)
	}
	if got.Choices[0].Message.Content != "hello" {
		t.Fatalf("content = %q", got.Choices[0].Message.Content)
	}
	if got.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q, want stop", got.Choices[0].FinishReason)
	}
	if got.Usage.Prompt != 7 || got.Usage.Completion != 3 || got.Usage.Total != 10 {
		t.Fatalf("usage = %+v", got.Usage)
	}
}

func TestAnthropicToOpenAIMapsToolUseToToolCalls(t *testing.T) {
	body := []byte(`{
		"id":"msg_2","stop_reason":"tool_use",
		"content":[{"type":"tool_use","id":"tu1","name":"f","input":{"a":1}}],
		"usage":{"input_tokens":1,"output_tokens":1}
	}`)
	out, _ := anthropicToOpenAI(body, "m")
	var got struct {
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
	_ = json.Unmarshal(out, &got)
	tc := got.Choices[0].Message.ToolCalls
	if len(tc) != 1 || tc[0].ID != "tu1" || tc[0].Function.Name != "f" {
		t.Fatalf("tool_calls = %+v", tc)
	}
	if !strings.Contains(tc[0].Function.Arguments, `"a":1`) {
		t.Fatalf("arguments = %q, want the JSON input", tc[0].Function.Arguments)
	}
	if got.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", got.Choices[0].FinishReason)
	}
}

// A malformed body must be rejected, not silently converted into an empty
// request that would bill the operator for nothing.
func TestOpenAIToAnthropicRejectsMalformedJSON(t *testing.T) {
	if _, err := openAIToAnthropic([]byte(`{"model":`)); err == nil {
		t.Fatal("expected an error for truncated JSON")
	}
}
