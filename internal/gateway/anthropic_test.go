package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"aiworkgate/internal/model"
)

// --------------------------------------------------------------- request map

func TestChatBodyFromAnthropic(t *testing.T) {
	t.Run("system blocks tools and tool exchange", func(t *testing.T) {
		src := `{"model":"p/chat","max_tokens":256,"temperature":0.4,"top_p":0.8,` +
			`"stop_sequences":["END"],"stream":true,` +
			`"thinking":{"type":"enabled","budget_tokens":1024},` +
			`"metadata":{"user_id":"u1","unknown":{"a":1}},` +
			`"system":[{"type":"text","text":"be "},{"type":"text","text":"brief"}],` +
			`"tools":[{"name":"get_weather","description":"Look up weather",` +
			`"input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],` +
			`"tool_choice":{"type":"any"},` +
			`"messages":[` +
			`{"role":"user","content":[{"type":"text","text":"weather?"},` +
			`{"type":"image","source":{"type":"file","file_id":"f1"}}]},` +
			`{"role":"assistant","content":[{"type":"text","text":"checking"},` +
			`{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Paris"}}]},` +
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","is_error":true,` +
			`"content":[{"type":"text","text":"boom"},{"type":"image","source":{}}]}]}]}`

		chat, images, err := chatBodyFromAnthropic([]byte(src))
		if err != nil {
			t.Fatalf("convert: %v", err)
		}
		if images != 2 {
			t.Fatalf("dropped images = %d, want 2", images)
		}
		req := decodeChatRequest(t, chat)
		if req.Model != "p/chat" {
			t.Fatalf("model = %q", req.Model)
		}
		roles := make([]string, 0, len(req.Messages))
		for _, msg := range req.Messages {
			roles = append(roles, msg.Role)
		}
		wantRoles := []string{"system", "user", "assistant", "tool"}
		if strings.Join(roles, ",") != strings.Join(wantRoles, ",") {
			t.Fatalf("roles = %v, want %v", roles, wantRoles)
		}
		if got := req.Messages[0].Content; got != "be brief" {
			t.Fatalf("system = %q", got)
		}
		if got := req.Messages[1].Content; got != "weather?" {
			t.Fatalf("user = %q", got)
		}
		if req.Messages[2].Content != "checking" || len(req.Messages[2].ToolCalls) != 1 {
			t.Fatalf("assistant = %+v", req.Messages[2])
		}
		call := req.Messages[2].ToolCalls[0]
		if call.ID != "toolu_1" || call.Type != "function" || call.Function.Name != "get_weather" {
			t.Fatalf("tool call = %+v", call)
		}
		if call.Function.Arguments != `{"city":"Paris"}` {
			t.Fatalf("arguments = %q", call.Function.Arguments)
		}
		if req.Messages[3].ToolCallID != "toolu_1" || req.Messages[3].Content != "Error: boom" {
			t.Fatalf("tool result = %+v", req.Messages[3])
		}
		for _, key := range []string{"max_tokens", "temperature", "top_p", "stream", "stop", "tools", "tool_choice"} {
			if _, ok := req.top[key]; !ok {
				t.Fatalf("field %s missing: %s", key, chat)
			}
		}
		if got := numberField(t, req.top, "max_tokens"); got != 256 {
			t.Fatalf("max_tokens = %v", got)
		}
		if got := numberField(t, req.top, "temperature"); got != 0.4 {
			t.Fatalf("temperature = %v", got)
		}
		if got := numberField(t, req.top, "top_p"); got != 0.8 {
			t.Fatalf("top_p = %v", got)
		}
		var stops []string
		decodeInto(t, req.top["stop"], &stops)
		if len(stops) != 1 || stops[0] != "END" {
			t.Fatalf("stop = %v", stops)
		}
		if got := stringField(t, req.top, "tool_choice"); got != "required" {
			t.Fatalf("tool_choice = %q, want required", got)
		}
		var tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"function"`
		}
		decodeInto(t, req.top["tools"], &tools)
		if len(tools) != 1 || tools[0].Type != "function" || tools[0].Function.Name != "get_weather" {
			t.Fatalf("tools = %+v", tools)
		}
		if tools[0].Function.Description != "Look up weather" {
			t.Fatalf("tool description = %q", tools[0].Function.Description)
		}
		var schema map[string]any
		decodeInto(t, tools[0].Function.Parameters, &schema)
		if schema["type"] != "object" {
			t.Fatalf("parameters = %v", schema)
		}
		for _, key := range []string{"thinking", "metadata"} {
			if _, ok := req.top[key]; ok {
				t.Fatalf("unmappable field %s leaked into the chat request", key)
			}
		}
	})

	t.Run("system string and missing optional fields", func(t *testing.T) {
		chat, images, err := chatBodyFromAnthropic([]byte(
			`{"model":"p/chat","system":"be brief","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatalf("convert: %v", err)
		}
		if images != 0 {
			t.Fatalf("images = %d", images)
		}
		req := decodeChatRequest(t, chat)
		if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[0].Content != "be brief" {
			t.Fatalf("messages = %+v", req.Messages)
		}
		if got := numberField(t, req.top, "max_tokens"); got != defaultAnthropicMaxTokens {
			t.Fatalf("default max_tokens = %v, want %d", got, defaultAnthropicMaxTokens)
		}
	})

	t.Run("tool choice forms", func(t *testing.T) {
		cases := []struct {
			name string
			raw  string
			want any
		}{
			{"any string", `"any"`, "required"},
			{"auto string", `"auto"`, "auto"},
			{"none string", `"none"`, "none"},
			{"any object", `{"type":"any","disable_parallel_tool_use":true}`, "required"},
			{"auto object", `{"type":"auto"}`, "auto"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				chat, _, err := chatBodyFromAnthropic([]byte(
					`{"model":"p/chat","messages":[{"role":"user","content":"x"}],"tool_choice":` + tc.raw + `}`))
				if err != nil {
					t.Fatalf("convert: %v", err)
				}
				req := decodeChatRequest(t, chat)
				var got any
				decodeInto(t, req.top["tool_choice"], &got)
				if got != tc.want {
					t.Fatalf("tool_choice = %v, want %v", got, tc.want)
				}
			})
		}
	})

	t.Run("tool choice names one function", func(t *testing.T) {
		chat, _, err := chatBodyFromAnthropic([]byte(
			`{"model":"p/chat","messages":[{"role":"user","content":"x"}],` +
				`"tool_choice":{"type":"tool","name":"get_weather"}}`))
		if err != nil {
			t.Fatalf("convert: %v", err)
		}
		req := decodeChatRequest(t, chat)
		var choice struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		decodeInto(t, req.top["tool_choice"], &choice)
		if choice.Type != "function" || choice.Function.Name != "get_weather" {
			t.Fatalf("tool_choice = %+v", choice)
		}
	})

	t.Run("default max_tokens and tool schema", func(t *testing.T) {
		chat, _, err := chatBodyFromAnthropic([]byte(
			`{"model":"p/chat","max_tokens":null,"messages":[{"role":"user","content":"x"}],` +
				`"tools":[{"name":"noop"}]}`))
		if err != nil {
			t.Fatalf("convert: %v", err)
		}
		req := decodeChatRequest(t, chat)
		if got := numberField(t, req.top, "max_tokens"); got != defaultAnthropicMaxTokens {
			t.Fatalf("max_tokens = %v", got)
		}
		var tools []struct {
			Function struct {
				Parameters map[string]any `json:"parameters"`
			} `json:"function"`
		}
		decodeInto(t, req.top["tools"], &tools)
		if len(tools) != 1 || tools[0].Function.Parameters["type"] != "object" {
			t.Fatalf("default schema = %+v", tools)
		}
	})
}

func TestChatBodyFromAnthropicRejects(t *testing.T) {
	cases := []struct{ name, body string }{
		{"not an object", `["model"]`},
		{"missing model", `{"messages":[{"role":"user","content":"x"}]}`},
		{"blank model", `{"model":"  ","messages":[]}`},
		{"missing messages", `{"model":"p/chat"}`},
		{"messages not an array", `{"model":"p/chat","messages":"nope"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := chatBodyFromAnthropic([]byte(tc.body)); err == nil {
				t.Fatalf("want rejection for %s", tc.body)
			}
		})
	}
}

// -------------------------------------------------------------- response body

func TestAnthropicMessageBody(t *testing.T) {
	var reply chatReply
	decodeInto(t, []byte(chatJSONReply), &reply)
	body := anthropicMessageBody(reply, "p/chat", "msg_test")

	if body["id"] != "msg_test" || body["type"] != "message" || body["role"] != "assistant" {
		t.Fatalf("envelope = %v", body)
	}
	if body["model"] != "p/chat" {
		t.Fatalf("model = %v, want the client-facing name", body["model"])
	}
	if body["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason = %v", body["stop_reason"])
	}
	if body["stop_sequence"] != nil {
		t.Fatalf("stop_sequence = %v", body["stop_sequence"])
	}
	content, ok := body["content"].([]any)
	if !ok || len(content) != 3 {
		t.Fatalf("content = %v", body["content"])
	}
	first := content[0].(map[string]any)
	if first["type"] != "thinking" || first["thinking"] != "thinking hard" {
		t.Fatalf("thinking block = %v", first)
	}
	if sig, _ := first["signature"].(string); sig == "" || sig != thinkingSignature("thinking hard") {
		t.Fatalf("thinking signature = %q, want a signature over the thinking text", sig)
	}
	second := content[1].(map[string]any)
	if second["type"] != "text" || second["text"] != "Hello world" {
		t.Fatalf("text block = %v", second)
	}
	third := content[2].(map[string]any)
	if third["type"] != "tool_use" || third["id"] != "call_1" || third["name"] != "get_weather" {
		t.Fatalf("tool block = %v", third)
	}
	input, ok := third["input"].(map[string]any)
	if !ok || input["city"] != "Paris" {
		t.Fatalf("tool input = %v", third["input"])
	}
	usage, ok := body["usage"].(map[string]any)
	if !ok || usage["input_tokens"] != 11 || usage["output_tokens"] != 7 {
		t.Fatalf("usage = %v", body["usage"])
	}
}

func TestAnthropicMessageEmptyContent(t *testing.T) {
	var reply chatReply
	decodeInto(t, []byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":""}}]}`), &reply)
	body := anthropicMessageBody(reply, "p/chat", "msg_1")
	content := body["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["type"] != "text" {
		t.Fatalf("content = %v, want one empty text block", content)
	}
	if body["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason = %v", body["stop_reason"])
	}
}

func TestAnthropicToolResultImageIsPreserved(t *testing.T) {
	src := `{"model":"p/chat","max_tokens":32,"messages":[` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"take_shot","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[` +
		`{"type":"text","text":"screenshot"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}}]}]}]}`

	chat, images, err := chatBodyFromAnthropic([]byte(src))
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if images != 0 {
		t.Fatalf("dropped images = %d, want 0", images)
	}
	if !strings.Contains(string(chat), "data:image/png;base64,aGk=") {
		t.Fatalf("tool_result image was dropped: %s", chat)
	}
	if !strings.Contains(string(chat), `"content":"screenshot"`) {
		t.Fatalf("tool_result text was lost: %s", chat)
	}
}

func TestAnthropicStopReason(t *testing.T) {
	cases := map[string]string{
		"stop": "end_turn", "length": "max_tokens", "content_filter": "refusal",
		"tool_calls": "tool_use", "function_call": "tool_use", "": "end_turn",
	}
	for finish, want := range cases {
		if got := anthropicStopReason(finish, false); got != want {
			t.Fatalf("stop_reason(%q) = %q, want %q", finish, got, want)
		}
	}
	if got := anthropicStopReason("stop", true); got != "tool_use" {
		t.Fatalf("tool calls must force tool_use, got %q", got)
	}
	if got := anthropicStopReason("length", true); got != "max_tokens" {
		t.Fatalf("truncated tool turn must report max_tokens, got %q", got)
	}
}

// -------------------------------------------------------------------- stream

func feedAnthropic(t *testing.T, wantStream bool, status int, contentType string, chunks []string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	aw := newAnthropicWriter(rec, "p/chat", wantStream)
	if contentType != "" {
		aw.Header().Set("Content-Type", contentType)
	}
	aw.WriteHeader(status)
	for _, chunk := range chunks {
		if _, err := aw.Write([]byte(chunk)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	aw.finish()
	return rec
}

func decodeEvent(t *testing.T, ev sseEvent) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal([]byte(ev.data), &payload); err != nil {
		t.Fatalf("event %q data is not JSON: %v (%q)", ev.event, err, ev.data)
	}
	return payload
}

func TestAnthropicWriterStream(t *testing.T) {
	rec := feedAnthropic(t, true, http.StatusOK, "text/event-stream", streamFrames(true))
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}
	body := rec.Body.String()
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("the chat terminator leaked into the Anthropic stream: %q", body)
	}
	events := parseSSE(t, body)
	want := []string{
		"message_start",
		"content_block_start", // thinking
		"content_block_delta", // thinking_delta
		"content_block_delta", // signature_delta
		"content_block_stop",
		"content_block_start", // text
		"content_block_delta", // Hel
		"content_block_delta", // lo
		"content_block_delta", // world
		"content_block_stop",
		"content_block_start", // tool_use
		"content_block_delta", // input_json_delta
		"content_block_stop",
		"message_delta",
		"message_stop",
	}
	got := eventNames(events)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events =\n %v\nwant\n %v", got, want)
	}

	start := decodeEvent(t, events[0])
	message := start["message"].(map[string]any)
	if start["type"] != "message_start" || message["type"] != "message" || message["role"] != "assistant" {
		t.Fatalf("message_start = %v", start)
	}
	if message["model"] != "p/chat" {
		t.Fatalf("message_start model = %v", message["model"])
	}
	id, _ := message["id"].(string)
	if !strings.HasPrefix(id, "msg_") || id == "msg_" {
		t.Fatalf("message id = %q, want msg_ prefix", id)
	}
	usage := message["usage"].(map[string]any)
	if usage["input_tokens"] != float64(0) || usage["output_tokens"] != float64(0) {
		t.Fatalf("message_start usage = %v, want zeros", usage)
	}
	if message["stop_reason"] != nil || message["stop_sequence"] != nil {
		t.Fatalf("message_start must not carry a stop reason: %v", message)
	}

	thinkingStart := decodeEvent(t, events[1])
	if thinkingStart["index"] != float64(0) {
		t.Fatalf("thinking index = %v", thinkingStart["index"])
	}
	block := thinkingStart["content_block"].(map[string]any)
	if block["type"] != "thinking" {
		t.Fatalf("first block = %v, thinking must come first", block)
	}
	if delta := decodeEvent(t, events[2])["delta"].(map[string]any); delta["type"] != "thinking_delta" || delta["thinking"] != "because" {
		t.Fatalf("thinking delta = %v", delta)
	}
	sigDelta := decodeEvent(t, events[3])["delta"].(map[string]any)
	if sigDelta["type"] != "signature_delta" {
		t.Fatalf("thinking signature delta = %v", sigDelta)
	}
	if sig, _ := sigDelta["signature"].(string); sig == "" || sig != thinkingSignature("because") {
		t.Fatalf("signature = %q, want the signature of the full thinking text", sig)
	}
	if decodeEvent(t, events[4])["index"] != float64(0) {
		t.Fatalf("thinking stop index = %v", decodeEvent(t, events[4])["index"])
	}

	textStart := decodeEvent(t, events[5])
	if textStart["index"] != float64(1) {
		t.Fatalf("text index = %v", textStart["index"])
	}
	if block := textStart["content_block"].(map[string]any); block["type"] != "text" {
		t.Fatalf("text block = %v", block)
	}
	var text string
	for _, ev := range events[6:9] {
		delta := decodeEvent(t, ev)["delta"].(map[string]any)
		if delta["type"] != "text_delta" {
			t.Fatalf("text delta = %v", delta)
		}
		text += delta["text"].(string)
	}
	if text != "Hello world" {
		t.Fatalf("streamed text = %q", text)
	}

	toolStart := decodeEvent(t, events[10])
	if toolStart["index"] != float64(2) {
		t.Fatalf("tool index = %v", toolStart["index"])
	}
	tool := toolStart["content_block"].(map[string]any)
	if tool["type"] != "tool_use" || tool["id"] != "call_1" || tool["name"] != "get_weather" {
		t.Fatalf("tool block = %v", tool)
	}
	if input, ok := tool["input"].(map[string]any); !ok || len(input) != 0 {
		t.Fatalf("tool input must start empty: %v", tool["input"])
	}
	toolDelta := decodeEvent(t, events[11])["delta"].(map[string]any)
	if toolDelta["type"] != "input_json_delta" {
		t.Fatalf("tool delta = %v", toolDelta)
	}
	partial := toolDelta["partial_json"].(string)
	if !json.Valid([]byte(partial)) {
		t.Fatalf("partial_json is not valid JSON: %q", partial)
	}
	var input map[string]any
	decodeInto(t, []byte(partial), &input)
	if input["city"] != "Paris" {
		t.Fatalf("tool input = %v", input)
	}

	final := decodeEvent(t, events[13])
	delta := final["delta"].(map[string]any)
	if final["type"] != "message_delta" || delta["stop_reason"] != "tool_use" || delta["stop_sequence"] != nil {
		t.Fatalf("message_delta = %v", final)
	}
	finalUsage := final["usage"].(map[string]any)
	if finalUsage["input_tokens"] != float64(11) || finalUsage["output_tokens"] != float64(7) {
		t.Fatalf("final usage = %v", finalUsage)
	}
	if decodeEvent(t, events[14])["type"] != "message_stop" {
		t.Fatalf("last event = %v", events[14])
	}
}

func TestAnthropicWriterEmptyStream(t *testing.T) {
	for name, chunks := range map[string][]string{
		"nothing":     nil,
		"only [DONE]": {"data: [DONE]\n\n"},
		"empty chunk": {`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}` + "\n\n", "data: [DONE]\n\n"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := feedAnthropic(t, true, http.StatusOK, "text/event-stream", chunks)
			events := parseSSE(t, rec.Body.String())
			if len(events) != 1 || events[0].event != "error" {
				t.Fatalf("events = %v, want a single error event", eventNames(events))
			}
			payload := decodeEvent(t, events[0])
			if payload["type"] != "error" {
				t.Fatalf("payload = %v", payload)
			}
			body := payload["error"].(map[string]any)
			if body["type"] != "api_error" || body["message"] == "" {
				t.Fatalf("error body = %v", body)
			}
			if strings.Contains(rec.Body.String(), "message_stop") {
				t.Fatal("an empty stream must not pretend to succeed")
			}
		})
	}
}

// -------------------------------------------------------------------- errors

func decodeAnthropicError(t *testing.T, rec *httptest.ResponseRecorder) anthropicErrorEnvelope {
	t.Helper()
	var payload anthropicErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("error body is not JSON: %v (%q)", err, rec.Body.String())
	}
	if payload.Type != "error" || payload.Error.Type == "" || payload.Error.Message == "" {
		t.Fatalf("incomplete Anthropic error body: %+v", payload)
	}
	return payload
}

func TestAnthropicWriterErrorBody(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		upstream string
		wantType string
		wantMsg  string
	}{
		{"rate limit", http.StatusTooManyRequests,
			`{"error":{"message":"slow down","type":"rate_limit_exceeded","code":"rate_limit"}}`,
			"rate_limit_error", "slow down"},
		{"server error", http.StatusInternalServerError,
			`{"error":{"message":"boom","type":"server_error","code":"server_error"}}`,
			"api_error", "boom"},
		{"gateway unknown model", http.StatusNotFound,
			`{"error":{"message":"unknown model","type":"invalid_request_error","code":"model_not_found"}}`,
			"not_found_error", "unknown model"},
		{"plain text", http.StatusBadGateway, `upstream exploded`,
			"api_error", "upstream exploded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := feedAnthropic(t, false, tc.status, "application/json", []string{tc.upstream})
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Fatalf("Content-Type = %q", ct)
			}
			payload := decodeAnthropicError(t, rec)
			if payload.Error.Type != tc.wantType || payload.Error.Message != tc.wantMsg {
				t.Fatalf("error = %+v, want %s/%q", payload.Error, tc.wantType, tc.wantMsg)
			}
		})
	}
}

// -------------------------------------------------------------------- routes

const anthropicRequest = `{"model":"p/chat","max_tokens":64,"system":"be brief",` +
	`"messages":[{"role":"user","content":"hi"}]}`

func TestMessagesRouteToPlugin(t *testing.T) {
	srv, calls := newUpstreamServer(t, jsonResponder(http.StatusOK, chatJSONReply))
	reg := newFakeRegistry().add(
		pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "plug-token")
	logs := &fakeLogSink{}
	g := newTestGateway(t, Options{Plugins: reg, Log: logs})

	rr := serve(g, newChatRequest(t, "/v1/messages", clientKey, anthropicRequest))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get(sourceHeader); got != "plugin:p" {
		t.Fatalf("source header = %q", got)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if calls.count() != 1 {
		t.Fatalf("plugin calls = %d, want 1", calls.count())
	}
	up := calls.last(t)
	if up.Path != "/v1/chat/completions" || up.Authorization != "Bearer plug-token" {
		t.Fatalf("plugin request = %s auth %q", up.Path, up.Authorization)
	}
	chat := decodeChatRequest(t, up.Body)
	if chat.Model != "chat" {
		t.Fatalf("upstream model = %q, want the physical name", chat.Model)
	}
	if got := numberField(t, chat.top, "max_tokens"); got != 64 {
		t.Fatalf("max_tokens = %v, want 64", got)
	}
	if len(chat.Messages) != 2 || chat.Messages[0].Role != "system" || chat.Messages[1].Role != "user" {
		t.Fatalf("messages = %+v", chat.Messages)
	}

	var body map[string]any
	decodeInto(t, rr.Body.Bytes(), &body)
	if body["type"] != "message" || body["model"] != "p/chat" || body["role"] != "assistant" {
		t.Fatalf("response = %v", body)
	}
	if body["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason = %v", body["stop_reason"])
	}
	content := body["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("content = %v", content)
	}
	if content[0].(map[string]any)["type"] != "thinking" ||
		content[1].(map[string]any)["type"] != "text" ||
		content[2].(map[string]any)["type"] != "tool_use" {
		t.Fatalf("block order = %v", content)
	}
	usage := body["usage"].(map[string]any)
	if usage["input_tokens"] != float64(11) || usage["output_tokens"] != float64(7) {
		t.Fatalf("usage = %v", usage)
	}

	row := logs.wait(t, 1)[0]
	if row.Source != "plugin:p" || row.Model != "p/chat" || row.Status != http.StatusOK {
		t.Fatalf("log row = %+v", row)
	}
	if row.Stream {
		t.Fatal("non-stream request was logged as a stream")
	}
}

func TestMessagesBareModelKeepsPassthrough(t *testing.T) {
	aSrv, aCalls := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"a":"original"}`))
	const body = `{"model":"claude-3-5-sonnet","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	g := newTestGateway(t, Options{
		Plugins: newFakeRegistry().add(pluginStatus("p", true, model.ModelInfo{ID: "chat"}), "", ""),
		A:       &fakeA{base: aSrv.URL, key: "sk-a"},
	})

	rr := serve(g, newChatRequest(t, "/v1/messages", clientKey, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	if got := rr.Body.String(); got != `{"a":"original"}` {
		t.Fatalf("body = %q, want A's answer verbatim", got)
	}
	up := aCalls.last(t)
	if up.Path != "/v1/messages" || string(up.Body) != body || up.Authorization != "Bearer sk-a" {
		t.Fatalf("A request = %s auth %q body %q", up.Path, up.Authorization, up.Body)
	}
	if got := rr.Header().Get(sourceHeader); got != "a" {
		t.Fatalf("source header = %q", got)
	}
}

func TestMessagesRouteStreamsSSE(t *testing.T) {
	frames := streamFrames(true)
	srv, calls := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, frame := range frames {
			_, _ = io.WriteString(w, frame)
			w.(http.Flusher).Flush()
		}
	})
	reg := newFakeRegistry().add(
		pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "tok")
	logs := &fakeLogSink{}
	g := newTestGateway(t, Options{Plugins: reg, Log: logs})

	rr := serve(g, newChatRequest(t, "/v1/messages", clientKey,
		`{"model":"p/chat","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}
	events := parseSSE(t, rr.Body.String())
	if len(events) == 0 || events[0].event != "message_start" ||
		events[len(events)-1].event != "message_stop" {
		t.Fatalf("sequence = %v", eventNames(events))
	}
	chat := decodeChatRequest(t, calls.last(t).Body)
	if got := string(chat.top["stream"]); got != "true" {
		t.Fatalf("stream flag was not forwarded: %s", calls.last(t).Body)
	}
	if got := numberField(t, chat.top, "max_tokens"); got != 32 {
		t.Fatalf("max_tokens = %v", got)
	}
	row := logs.wait(t, 1)[0]
	if !row.Stream || row.Status != http.StatusOK {
		t.Fatalf("log row = %+v", row)
	}
}

func TestMessagesRouteUsesScheduler(t *testing.T) {
	srv, calls := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(accountHeader, r.Header.Get(accountHeader))
		_, _ = io.WriteString(w, chatJSONReply)
	})
	reg := newFakeRegistry().add(
		pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	logs := &fakeLogSink{}
	g := scheduledGateway(t, reg, logs, singlePoolSnapshot("p1", 2, "acc1"))

	rr := serve(g, newChatRequest(t, "/v1/messages", clientKey,
		`{"model":"p1/m1","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	if got := calls.last(t).Header.Get(accountHeader); got != "acc1" {
		t.Fatalf("pinned account = %q, want acc1", got)
	}
	if got := rr.Header().Get(sourceHeader); got != "plugin:p1" {
		t.Fatalf("source header = %q", got)
	}
	var body map[string]any
	decodeInto(t, rr.Body.Bytes(), &body)
	if body["type"] != "message" || body["model"] != "p1/m1" {
		t.Fatalf("response = %v", body)
	}
	row := logs.wait(t, 1)[0]
	if row.Account != "acc1" || row.Source != "plugin:p1" || row.Model != "p1/m1" {
		t.Fatalf("log row = %+v", row)
	}
}

func TestMessagesRouteErrorsUseAnthropicShape(t *testing.T) {
	t.Run("upstream rate limit", func(t *testing.T) {
		const errBody = `{"error":{"message":"upstream says no","type":"server_error","code":"boom"}}`
		srv, _ := newUpstreamServer(t, jsonResponder(http.StatusTooManyRequests, errBody))
		reg := newFakeRegistry().add(
			pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "tok")
		g := newTestGateway(t, Options{Plugins: reg, Log: &fakeLogSink{}})

		rr := serve(g, newChatRequest(t, "/v1/messages", clientKey, anthropicRequest))
		if rr.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429 (body %s)", rr.Code, rr.Body.String())
		}
		payload := decodeAnthropicError(t, rr)
		if payload.Error.Type != "rate_limit_error" || payload.Error.Message != "upstream says no" {
			t.Fatalf("error = %+v", payload.Error)
		}
	})

	t.Run("plugin unavailable", func(t *testing.T) {
		reg := pluginRegistry()
		reg.setEnsureErr("p", io.ErrClosedPipe)
		g := newTestGateway(t, Options{Plugins: reg, Log: &fakeLogSink{}})

		rr := serve(g, newChatRequest(t, "/v1/messages", clientKey, anthropicRequest))
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 (body %s)", rr.Code, rr.Body.String())
		}
		payload := decodeAnthropicError(t, rr)
		if payload.Error.Type != "api_error" || !strings.Contains(payload.Error.Message, "无法启动") {
			t.Fatalf("error = %+v", payload.Error)
		}
		if got := rr.Header().Get(sourceHeader); got != "plugin:p" {
			t.Fatalf("source header = %q", got)
		}
	})

	t.Run("unknown plugin model", func(t *testing.T) {
		srv, calls := newUpstreamServer(t, jsonResponder(http.StatusOK, chatJSONReply))
		reg := newFakeRegistry().add(
			pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "tok")
		g := newTestGateway(t, Options{Plugins: reg, Log: &fakeLogSink{}})

		rr := serve(g, newChatRequest(t, "/v1/messages", clientKey, `{"model":"p/","messages":[]}`))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body %s)", rr.Code, rr.Body.String())
		}
		payload := decodeAnthropicError(t, rr)
		if payload.Error.Type != "not_found_error" {
			t.Fatalf("error = %+v", payload.Error)
		}
		if calls.count() != 0 {
			t.Fatalf("plugin calls = %d", calls.count())
		}
	})

	t.Run("unmappable request", func(t *testing.T) {
		srv, calls := newUpstreamServer(t, jsonResponder(http.StatusOK, chatJSONReply))
		reg := newFakeRegistry().add(
			pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "tok")
		g := newTestGateway(t, Options{Plugins: reg, Log: &fakeLogSink{}})

		rr := serve(g, newChatRequest(t, "/v1/messages", clientKey,
			`{"model":"p/chat","messages":"nope"}`))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", rr.Code, rr.Body.String())
		}
		payload := decodeAnthropicError(t, rr)
		if payload.Error.Type != "invalid_request_error" {
			t.Fatalf("error = %+v", payload.Error)
		}
		if calls.count() != 0 {
			t.Fatalf("plugin calls = %d", calls.count())
		}
	})
}

// ------------------------------------------------ native Anthropic headers

// TestMessagesDropsNativeAnthropicHeaders covers the interoperability failure
// where a standard anthropic-version header survived the translation into a chat
// body and the chat upstream rejected the whole call because of it. The headers
// only make sense for a native Anthropic upstream, so they must not reach the
// chat endpoint; the A passthrough keeps forwarding them.
func TestMessagesDropsNativeAnthropicHeaders(t *testing.T) {
	const (
		version = "2023-06-01"
		beta    = "prompt-caching-2024-07-31"
	)
	requireChatBodyWithout := func(t *testing.T, up capturedRequest) {
		t.Helper()
		if got := up.Header.Get("anthropic-version"); got != "" {
			t.Fatalf("anthropic-version leaked to the chat upstream: %q", got)
		}
		if got := up.Header.Get("anthropic-beta"); got != "" {
			t.Fatalf("anthropic-beta leaked to the chat upstream: %q", got)
		}
		if up.Path != "/v1/chat/completions" {
			t.Fatalf("upstream path = %s, want the chat endpoint", up.Path)
		}
		chat := decodeChatRequest(t, up.Body)
		if chat.Model == "" || len(chat.Messages) == 0 {
			t.Fatalf("upstream body is not a chat body: %s", up.Body)
		}
	}

	t.Run("plugin", func(t *testing.T) {
		srv, calls := newUpstreamServer(t, jsonResponder(http.StatusOK, chatJSONReply))
		reg := newFakeRegistry().add(
			pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "tok")
		g := newTestGateway(t, Options{Plugins: reg, Log: &fakeLogSink{}})

		req := newChatRequest(t, "/v1/messages", clientKey, anthropicRequest)
		req.Header.Set("anthropic-version", version)
		req.Header.Set("anthropic-beta", beta)
		rr := serve(g, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
		}
		requireChatBodyWithout(t, calls.last(t))
	})

	t.Run("scheduled", func(t *testing.T) {
		srv, calls := newUpstreamServer(t, jsonResponder(http.StatusOK, chatJSONReply))
		reg := newFakeRegistry().add(
			pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
		g := scheduledGateway(t, reg, &fakeLogSink{}, singlePoolSnapshot("p1", 2, "acc1"))

		req := newChatRequest(t, "/v1/messages", clientKey,
			`{"model":"p1/m1","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
		req.Header.Set("anthropic-version", version)
		req.Header.Set("anthropic-beta", beta)
		rr := serve(g, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
		}
		requireChatBodyWithout(t, calls.last(t))
	})

	t.Run("bare model keeps the passthrough headers", func(t *testing.T) {
		aSrv, aCalls := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"a":"original"}`))
		g := newTestGateway(t, Options{
			Plugins: newFakeRegistry(),
			A:       &fakeA{base: aSrv.URL, key: "sk-a"},
		})

		req := newChatRequest(t, "/v1/messages", clientKey,
			`{"model":"claude-3-5-sonnet","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
		req.Header.Set("anthropic-version", version)
		req.Header.Set("anthropic-beta", beta)
		rr := serve(g, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
		}
		up := aCalls.last(t)
		if got := up.Header.Get("anthropic-version"); got != version {
			t.Fatalf("A must receive the client's anthropic-version, got %q", got)
		}
		if got := up.Header.Get("anthropic-beta"); got != beta {
			t.Fatalf("A must receive the client's anthropic-beta, got %q", got)
		}
		if string(up.Body) != `{"model":"claude-3-5-sonnet","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}` {
			t.Fatalf("A body = %q, want the native body untouched", up.Body)
		}
	})
}

// TestMessagesUnknownModelCarriesCode pins the error code parity between the
// OpenAI paths and the Anthropic envelope: both report model_not_found.
func TestMessagesUnknownModelCarriesCode(t *testing.T) {
	srv, calls := newUpstreamServer(t, jsonResponder(http.StatusOK, chatJSONReply))
	reg := newFakeRegistry().add(
		pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "tok")
	g := newTestGateway(t, Options{Plugins: reg, Log: &fakeLogSink{}})

	for _, model := range []string{"p/nope", "p/"} {
		rr := serve(g, newChatRequest(t, "/v1/messages", clientKey,
			`{"model":"`+model+`","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404 (body %s)", model, rr.Code, rr.Body.String())
		}
		// Decoded into a test-local shape so the assertion reads the wire body,
		// not the production envelope type.
		var payload struct {
			Type  string `json:"type"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
				Code    string `json:"code"`
			} `json:"error"`
		}
		decodeInto(t, rr.Body.Bytes(), &payload)
		if payload.Error.Type != "not_found_error" {
			t.Fatalf("%s error type = %q, want not_found_error", model, payload.Error.Type)
		}
		if payload.Error.Code != "model_not_found" {
			t.Fatalf("%s error code = %q, want model_not_found (body %s)",
				model, payload.Error.Code, rr.Body.String())
		}
		if !strings.Contains(payload.Error.Message, "unknown model") {
			t.Fatalf("%s message = %q", model, payload.Error.Message)
		}
	}
	if calls.count() != 0 {
		t.Fatalf("plugin calls = %d, want an unknown model to stop before the upstream", calls.count())
	}
}

type fakeSchedLog struct {
	mu   sync.Mutex
	msgs []string
}

func (l *fakeSchedLog) Info(string, string, map[string]any) {}

func (l *fakeSchedLog) Warn(module, msg string, _ map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.msgs = append(l.msgs, module+": "+msg)
}

func TestMessagesRouteLogsDroppedImages(t *testing.T) {
	srv, _ := newUpstreamServer(t, jsonResponder(http.StatusOK, chatJSONReply))
	reg := newFakeRegistry().add(
		pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "tok")
	logger := &fakeSchedLog{}
	g := newTestGateway(t, Options{Plugins: reg, Log: &fakeLogSink{}, SchedLog: logger})

	rr := serve(g, newChatRequest(t, "/v1/messages", clientKey,
		`{"model":"p/chat","messages":[{"role":"user","content":[`+
			`{"type":"text","text":"look"},{"type":"image","source":{"type":"file","file_id":"f1"}}]}]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	logger.mu.Lock()
	defer logger.mu.Unlock()
	if len(logger.msgs) != 1 || !strings.Contains(logger.msgs[0], "image") {
		t.Fatalf("warnings = %v, want one image warning", logger.msgs)
	}
}
