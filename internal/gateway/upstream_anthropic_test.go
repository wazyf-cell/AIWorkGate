package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestChatToAnthropicRequestTextToolsImage(t *testing.T) {
	body := `{"model":"ignored","max_tokens":20000,"temperature":0.2,"top_p":0.9,
		"messages":[
			{"role":"system","content":"be brief"},
			{"role":"user","content":[
				{"type":"text","text":"look"},
				{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"result"}
		],
		"tools":[{"type":"function","function":{"name":"lookup","description":"d","parameters":{"type":"object"}}}],
		"tool_choice":{"type":"function","function":{"name":"lookup"}},
		"stop":["END"],"reasoning_effort":"high"}`

	raw, err := chatToAnthropicRequest([]byte(body), "claude-x", "high")
	if err != nil {
		t.Fatalf("chatToAnthropicRequest: %v", err)
	}
	var req struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		System    string `json:"system"`
		Messages  []struct {
			Role    string `json:"role"`
			Content []struct {
				Type    string         `json:"type"`
				Text    string         `json:"text"`
				Content string         `json:"content"`
				ID      string         `json:"id"`
				Name    string         `json:"name"`
				Input   map[string]any `json:"input"`
				Source  map[string]any `json:"source"`
			} `json:"content"`
		} `json:"messages"`
		Tools         []map[string]any `json:"tools"`
		ToolChoice    map[string]any   `json:"tool_choice"`
		StopSequences []string         `json:"stop_sequences"`
		Thinking      map[string]any   `json:"thinking"`
		Temperature   float64          `json:"temperature"`
		TopP          float64          `json:"top_p"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if req.Model != "claude-x" || req.MaxTokens != 20000 || req.System != "be brief" {
		t.Fatalf("identity = %+v", req)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %+v, want user/assistant/user", req.Messages)
	}
	if req.Messages[0].Role != "user" || len(req.Messages[0].Content) != 2 ||
		req.Messages[0].Content[1].Type != "image" ||
		req.Messages[0].Content[1].Source["type"] != "base64" {
		t.Fatalf("user message = %+v", req.Messages[0])
	}
	assistant := req.Messages[1]
	if assistant.Role != "assistant" || len(assistant.Content) != 1 ||
		assistant.Content[0].Type != "tool_use" || assistant.Content[0].ID != "call_1" ||
		assistant.Content[0].Input["q"] != "x" {
		t.Fatalf("assistant message = %+v", assistant)
	}
	toolTurn := req.Messages[2]
	if toolTurn.Role != "user" || len(toolTurn.Content) != 1 ||
		toolTurn.Content[0].Type != "tool_result" || toolTurn.Content[0].Content != "result" {
		t.Fatalf("tool turn = %+v", toolTurn)
	}
	if len(req.Tools) != 1 || req.Tools[0]["name"] != "lookup" {
		t.Fatalf("tools = %+v", req.Tools)
	}
	if req.ToolChoice["type"] != "tool" || req.ToolChoice["name"] != "lookup" {
		t.Fatalf("tool_choice = %+v", req.ToolChoice)
	}
	if len(req.StopSequences) != 1 || req.StopSequences[0] != "END" {
		t.Fatalf("stop = %+v", req.StopSequences)
	}
	if req.Thinking["budget_tokens"] != float64(8192) || req.Temperature != 1 {
		t.Fatalf("thinking = %+v temp = %v", req.Thinking, req.Temperature)
	}
	if req.TopP != 0 {
		t.Fatalf("top_p must be omitted with thinking, got %v", req.TopP)
	}
}

func TestChatToAnthropicThinkingBudgetClamp(t *testing.T) {
	raw, err := chatToAnthropicRequest([]byte(`{"max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`), "m", "max")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	var req struct {
		Thinking map[string]any `json:"thinking"`
	}
	_ = json.Unmarshal(raw, &req)
	if req.Thinking["budget_tokens"] != float64(4095) {
		t.Fatalf("budget = %v, want max_tokens-1", req.Thinking["budget_tokens"])
	}
}

func TestAnthropicResponseToChat(t *testing.T) {
	body := `{"id":"msg_1","content":[
		{"type":"thinking","thinking":"plan"},
		{"type":"text","text":"hello"},
		{"type":"tool_use","id":"toolu_1","name":"f","input":{"a":1}}],
		"stop_reason":"tool_use",
		"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":3}}`
	raw, err := anthropicResponseToChat([]byte(body), "my-model")
	if err != nil {
		t.Fatalf("response: %v", err)
	}
	var reply struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content          string           `json:"content"`
				ReasoningContent string           `json:"reasoning_content"`
				ToolCalls        []map[string]any `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			Details          struct {
				Cached int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if reply.Model != "my-model" || len(reply.Choices) != 1 {
		t.Fatalf("reply = %+v", reply)
	}
	choice := reply.Choices[0]
	if choice.Message.Content != "hello" || choice.Message.ReasoningContent != "plan" ||
		choice.FinishReason != "tool_calls" || len(choice.Message.ToolCalls) != 1 {
		t.Fatalf("choice = %+v", choice)
	}
	if reply.Usage.PromptTokens != 10 || reply.Usage.CompletionTokens != 5 || reply.Usage.Details.Cached != 3 {
		t.Fatalf("usage = %+v", reply.Usage)
	}
}

func TestAnthropicStreamToChat(t *testing.T) {
	stream := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_2","usage":{"input_tokens":7,"cache_read_input_tokens":2}}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"", "",
	}, "\n")
	var out strings.Builder
	if err := anthropicStreamToChat(strings.NewReader(stream), &out, "my-model"); err != nil {
		t.Fatalf("stream: %v", err)
	}
	text := out.String()
	for _, want := range []string{`"role":"assistant"`, `"content":"Hi"`, `"finish_reason":"stop"`,
		`"prompt_tokens":7`, `"completion_tokens":4`, `"cached_tokens":2`, "data: [DONE]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("stream missing %q: %s", want, text)
		}
	}
	if !strings.HasSuffix(text, "data: [DONE]\n\n") {
		t.Fatalf("stream does not end with [DONE]: %q", text)
	}
}

func TestAnthropicStreamToolUseFragments(t *testing.T) {
	stream := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_3","usage":{"input_tokens":2}}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_2","name":"f","input":{}}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"a\":"}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"1}"}}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":3}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"", "",
	}, "\n")
	var out strings.Builder
	if err := anthropicStreamToChat(strings.NewReader(stream), &out, "m"); err != nil {
		t.Fatalf("stream: %v", err)
	}
	text := out.String()
	for _, want := range []string{`"tool_calls"`, `"id":"toolu_2"`, `"name":"f"`,
		`"arguments":"{\"a\":"`, `"arguments":"1}"`, `"finish_reason":"tool_calls"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("stream missing %q: %s", want, text)
		}
	}
}

func TestAnthropicStreamErrorEvent(t *testing.T) {
	stream := "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n"
	var out strings.Builder
	err := anthropicStreamToChat(strings.NewReader(stream), &out, "m")
	if err == nil {
		t.Fatal("error event returned nil")
	}
	if !strings.Contains(out.String(), "busy") || !strings.Contains(out.String(), "[DONE]") {
		t.Fatalf("output = %s", out.String())
	}
}
