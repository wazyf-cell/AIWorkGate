package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"aiworkgate/internal/model"
	"aiworkgate/internal/scheduler"
)

// chatJSONReply is a canned upstream chat answer carrying reasoning, text and
// a tool call so every envelope field is exercised at once.
const chatJSONReply = `{"id":"chatcmpl-77","object":"chat.completion","created":1700000001,"model":"chat",` +
	`"choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"Hello world",` +
	`"reasoning_content":"thinking hard","tool_calls":[{"id":"call_1","type":"function","function":` +
	`{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]}}],` +
	`"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`

// ------------------------------------------------------------------ requests

type decodedChat struct {
	Model    string           `json:"model"`
	Messages []decodedChatMsg `json:"messages"`
	top      map[string]json.RawMessage
}

type decodedChatMsg struct {
	Role       string `json:"role"`
	Content    string `json:"content"`
	Name       string `json:"name"`
	ToolCallID string `json:"tool_call_id"`
	ToolCalls  []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

func decodeChatRequest(t *testing.T, chat []byte) decodedChat {
	t.Helper()
	var out decodedChat
	if err := json.Unmarshal(chat, &out); err != nil {
		t.Fatalf("converted chat body is not valid: %v (%s)", err, chat)
	}
	if err := json.Unmarshal(chat, &out.top); err != nil {
		t.Fatalf("converted chat body is not an object: %v", err)
	}
	return out
}

func requireRejected(t *testing.T, body string, convert func([]byte) ([]byte, error)) {
	t.Helper()
	chat, err := convert([]byte(body))
	if err == nil {
		t.Fatalf("want rejection for %s, got %s", body, chat)
	}
	var envErr *envelopeError
	if !errors.As(err, &envErr) {
		t.Fatalf("want *envelopeError for %s, got %T: %v", body, err, err)
	}
	if envErr.Error() == "" {
		t.Fatalf("rejection for %s carries no message", body)
	}
}

func TestChatBodyFromCompletions(t *testing.T) {
	t.Run("string prompt", func(t *testing.T) {
		chat, err := chatBodyFromCompletions([]byte(
			`{"model":"p/chat","prompt":"hello","max_tokens":16,"temperature":0.5,"stop":"\n","user":"u1","stream":true}`))
		if err != nil {
			t.Fatalf("convert: %v", err)
		}
		req := decodeChatRequest(t, chat)
		if req.Model != "p/chat" {
			t.Fatalf("model = %q", req.Model)
		}
		if len(req.Messages) != 1 || req.Messages[0].Role != "user" || req.Messages[0].Content != "hello" {
			t.Fatalf("messages = %+v, want one user message with the prompt", req.Messages)
		}
		for _, key := range []string{"max_tokens", "temperature", "stop", "user", "stream"} {
			if _, ok := req.top[key]; !ok {
				t.Fatalf("field %s was dropped: %s", key, chat)
			}
		}
	})

	t.Run("single element prompt array", func(t *testing.T) {
		chat, err := chatBodyFromCompletions([]byte(`{"model":"p/chat","prompt":["hello"]}`))
		if err != nil {
			t.Fatalf("convert: %v", err)
		}
		req := decodeChatRequest(t, chat)
		if len(req.Messages) != 1 || req.Messages[0].Content != "hello" {
			t.Fatalf("messages = %+v", req.Messages)
		}
	})

	rejections := []struct{ name, body string }{
		{"multi element prompt", `{"model":"p/chat","prompt":["a","b"]}`},
		{"non string prompt element", `{"model":"p/chat","prompt":[{"type":"text"}]}`},
		{"object prompt", `{"model":"p/chat","prompt":{"a":1}}`},
		{"missing prompt", `{"model":"p/chat"}`},
		{"n greater than one", `{"model":"p/chat","prompt":"x","n":2}`},
		{"echo", `{"model":"p/chat","prompt":"x","echo":true}`},
		{"suffix", `{"model":"p/chat","prompt":"x","suffix":"y"}`},
		{"logit_bias", `{"model":"p/chat","prompt":"x","logit_bias":{"7":1}}`},
		{"best_of", `{"model":"p/chat","prompt":"x","best_of":2}`},
		{"missing model", `{"prompt":"x"}`},
		{"blank model", `{"model":"  ","prompt":"x"}`},
		{"invalid json", `{"model":`},
		{"not an object", `["prompt"]`},
	}
	for _, tc := range rejections {
		t.Run("reject "+tc.name, func(t *testing.T) {
			requireRejected(t, tc.body, chatBodyFromCompletions)
		})
	}

	t.Run("n one and null optional are accepted", func(t *testing.T) {
		if _, err := chatBodyFromCompletions([]byte(`{"model":"p/chat","prompt":"x","n":1,"echo":null}`)); err != nil {
			t.Fatalf("convert: %v", err)
		}
	})

	// An explicit false / empty value is the OpenAI default, not a request for
	// the unsupported behaviour: SDKs send it on every call.
	t.Run("falsy parameters are accepted", func(t *testing.T) {
		cases := []string{
			`{"model":"p/chat","prompt":"x","echo":false}`,
			`{"model":"p/chat","prompt":"x","suffix":""}`,
			`{"model":"p/chat","prompt":"x","logit_bias":{}}`,
			`{"model":"p/chat","prompt":"x","best_of":1}`,
			`{"model":"p/chat","prompt":"x","echo":false,"suffix":"","logit_bias":{},"best_of":0}`,
		}
		for _, body := range cases {
			chat, err := chatBodyFromCompletions([]byte(body))
			if err != nil {
				t.Fatalf("convert %s: %v", body, err)
			}
			req := decodeChatRequest(t, chat)
			for _, name := range []string{"echo", "suffix", "logit_bias", "best_of"} {
				if _, ok := req.top[name]; ok {
					t.Fatalf("%s with a falsy value leaked into the chat request: %s", name, chat)
				}
			}
		}
	})
}

func TestChatBodyFromResponses(t *testing.T) {
	t.Run("string input and instructions", func(t *testing.T) {
		chat, err := chatBodyFromResponses([]byte(
			`{"model":"p/chat","instructions":"be brief","input":"hi","max_output_tokens":32,"stream":true}`))
		if err != nil {
			t.Fatalf("convert: %v", err)
		}
		req := decodeChatRequest(t, chat)
		if len(req.Messages) != 2 {
			t.Fatalf("messages = %+v, want system then user", req.Messages)
		}
		if req.Messages[0].Role != "system" || req.Messages[0].Content != "be brief" {
			t.Fatalf("instructions were not mapped to a leading system message: %+v", req.Messages[0])
		}
		if req.Messages[1].Role != "user" || req.Messages[1].Content != "hi" {
			t.Fatalf("input string was not mapped to a user message: %+v", req.Messages[1])
		}
		if got := numberField(t, req.top, "max_tokens"); got != 32 {
			t.Fatalf("max_tokens = %v, want 32 (max_output_tokens was not renamed)", got)
		}
		if _, ok := req.top["max_output_tokens"]; ok {
			t.Fatal("max_output_tokens leaked into the chat request")
		}
		if _, ok := req.top["stream"]; !ok {
			t.Fatalf("stream was dropped: %s", chat)
		}
	})

	t.Run("item array input", func(t *testing.T) {
		body := `{"model":"p/chat","input":[` +
			`{"type":"reasoning","summary":[{"type":"summary_text","text":"old"}]},` +
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},` +
			`{"type":"function_call","call_id":"call_9","name":"lookup","arguments":"{\"q\":\"x\"}"},` +
			`{"type":"function_call_output","call_id":"call_9","output":"42"}]}`
		chat, err := chatBodyFromResponses([]byte(body))
		if err != nil {
			t.Fatalf("convert: %v", err)
		}
		req := decodeChatRequest(t, chat)
		if len(req.Messages) != 3 {
			t.Fatalf("messages = %+v, want user/assistant/tool (reasoning replay is dropped)", req.Messages)
		}
		if req.Messages[0].Role != "user" || req.Messages[0].Content != "hi" {
			t.Fatalf("content parts were not flattened: %+v", req.Messages[0])
		}
		calls := req.Messages[1].ToolCalls
		if req.Messages[1].Role != "assistant" || len(calls) != 1 {
			t.Fatalf("function_call item did not become an assistant tool call: %+v", req.Messages[1])
		}
		if calls[0].ID != "call_9" || calls[0].Type != "function" ||
			calls[0].Function.Name != "lookup" || calls[0].Function.Arguments != `{"q":"x"}` {
			t.Fatalf("tool call payload = %+v", calls[0])
		}
		if req.Messages[2].Role != "tool" || req.Messages[2].ToolCallID != "call_9" || req.Messages[2].Content != "42" {
			t.Fatalf("function_call_output item = %+v", req.Messages[2])
		}
	})

	t.Run("parallel function calls share one assistant message", func(t *testing.T) {
		// chat pairs every tool reply with the assistant message carrying the
		// call, so one turn must not split into several messages.
		body := `{"model":"p/chat","input":[` +
			`{"type":"message","role":"user","content":"go"},` +
			`{"type":"function_call","call_id":"c1","name":"one","arguments":"{}"},` +
			`{"type":"function_call","call_id":"c2","name":"two","arguments":{"q":1}},` +
			`{"type":"function_call_output","call_id":"c1","output":"A"},` +
			`{"type":"function_call_output","call_id":"c2","output":"B"}]}`
		chat, err := chatBodyFromResponses([]byte(body))
		if err != nil {
			t.Fatalf("convert: %v", err)
		}
		req := decodeChatRequest(t, chat)
		if len(req.Messages) != 4 {
			t.Fatalf("messages = %+v, want user, one assistant turn, two tool replies", req.Messages)
		}
		calls := req.Messages[1].ToolCalls
		if req.Messages[1].Role != "assistant" || len(calls) != 2 {
			t.Fatalf("parallel calls were not merged: %+v", req.Messages[1])
		}
		if calls[0].ID != "c1" || calls[0].Function.Name != "one" || calls[0].Function.Arguments != "{}" {
			t.Fatalf("first call = %+v", calls[0])
		}
		if calls[1].ID != "c2" || calls[1].Function.Name != "two" ||
			calls[1].Function.Arguments != `{"q":1}` {
			t.Fatalf("second call = %+v (object arguments must stay JSON)", calls[1])
		}
		if req.Messages[2].Role != "tool" || req.Messages[2].ToolCallID != "c1" ||
			req.Messages[3].ToolCallID != "c2" {
			t.Fatalf("tool replies = %+v", req.Messages[2:])
		}
	})

	t.Run("a separate item closes the assistant turn", func(t *testing.T) {
		body := `{"model":"p/chat","input":[` +
			`{"type":"function_call","call_id":"c1","name":"one","arguments":"{}"},` +
			`{"type":"message","role":"user","content":"again"},` +
			`{"type":"function_call","call_id":"c2","name":"two","arguments":"{}"}]}`
		chat, err := chatBodyFromResponses([]byte(body))
		if err != nil {
			t.Fatalf("convert: %v", err)
		}
		req := decodeChatRequest(t, chat)
		if len(req.Messages) != 3 {
			t.Fatalf("messages = %+v, want two separate assistant turns around the user message", req.Messages)
		}
		if len(req.Messages[0].ToolCalls) != 1 || len(req.Messages[2].ToolCalls) != 1 {
			t.Fatalf("tool calls split across turns = %+v", req.Messages)
		}
	})

	t.Run("reasoning effort and tools", func(t *testing.T) {
		chat, err := chatBodyFromResponses([]byte(`{"model":"p/chat","input":"hi",` +
			`"reasoning":{"effort":"high"},"tool_choice":"auto",` +
			`"tools":[{"type":"function","name":"get_weather","description":"d","parameters":{"type":"object"},"strict":false}]}`))
		if err != nil {
			t.Fatalf("convert: %v", err)
		}
		req := decodeChatRequest(t, chat)
		if got := stringField(t, req.top, "reasoning_effort"); got != "high" {
			t.Fatalf("reasoning_effort = %q, want high", got)
		}
		if _, ok := req.top["reasoning"]; ok {
			t.Fatal("the reasoning object leaked into the chat request")
		}
		if got := stringField(t, req.top, "tool_choice"); got != "auto" {
			t.Fatalf("tool_choice = %q", got)
		}
		var tools []map[string]any
		decodeInto(t, req.top["tools"], &tools)
		if len(tools) != 1 {
			t.Fatalf("tools = %v", tools)
		}
		if tools[0]["type"] != "function" {
			t.Fatalf("tool type = %v", tools[0]["type"])
		}
		fn, ok := tools[0]["function"].(map[string]any)
		if !ok {
			t.Fatalf("flat tool was not nested: %v", tools[0])
		}
		if fn["name"] != "get_weather" || fn["description"] != "d" || fn["strict"] != false {
			t.Fatalf("nested tool = %v", fn)
		}
		if _, ok := tools[0]["name"]; ok {
			t.Fatalf("flat tool fields were not moved under function: %v", tools[0])
		}
	})

	t.Run("already nested tools pass through", func(t *testing.T) {
		const tools = `[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]`
		chat, err := chatBodyFromResponses([]byte(`{"model":"p/chat","input":"hi","tools":` + tools + `}`))
		if err != nil {
			t.Fatalf("convert: %v", err)
		}
		req := decodeChatRequest(t, chat)
		if got := string(req.top["tools"]); got != tools {
			t.Fatalf("nested tools were rewritten: %s", got)
		}
	})

	t.Run("non function tools pass through", func(t *testing.T) {
		const tools = `[{"type":"web_search"}]`
		chat, err := chatBodyFromResponses([]byte(`{"model":"p/chat","input":"hi","tools":` + tools + `}`))
		if err != nil {
			t.Fatalf("convert: %v", err)
		}
		req := decodeChatRequest(t, chat)
		if got := string(req.top["tools"]); got != tools {
			t.Fatalf("host tool was rewritten: %s", got)
		}
	})

	t.Run("mixed tool list converts only the flat entries", func(t *testing.T) {
		chat, err := chatBodyFromResponses([]byte(`{"model":"p/chat","input":"hi","tools":[` +
			`{"type":"web_search"},` +
			`{"type":"function","name":"f","parameters":{"type":"object"}},` +
			`{"type":"function","function":{"name":"already","parameters":{"type":"object"}}}]}`))
		if err != nil {
			t.Fatalf("convert: %v", err)
		}
		req := decodeChatRequest(t, chat)
		var tools []map[string]any
		decodeInto(t, req.top["tools"], &tools)
		if len(tools) != 3 {
			t.Fatalf("tools = %v", tools)
		}
		if tools[0]["type"] != "web_search" || tools[0]["function"] != nil {
			t.Fatalf("host tool was rewritten: %v", tools[0])
		}
		flat, ok := tools[1]["function"].(map[string]any)
		if !ok || flat["name"] != "f" || tools[1]["type"] != "function" {
			t.Fatalf("flat tool next to a host tool stayed unwrapped: %v", tools[1])
		}
		nested, ok := tools[2]["function"].(map[string]any)
		if !ok || nested["name"] != "already" {
			t.Fatalf("nested tool was rewritten: %v", tools[2])
		}
	})

	rejections := []struct{ name, body string }{
		{"background", `{"model":"p/chat","input":"hi","background":true}`},
		{"previous_response_id without input", `{"model":"p/chat","previous_response_id":"resp_1"}`},
		{"empty input", `{"model":"p/chat"}`},
		{"empty input array", `{"model":"p/chat","input":[]}`},
		{"input not string or array", `{"model":"p/chat","input":7}`},
		{"missing model", `{"input":"hi"}`},
		{"invalid json", `{"model":`},
	}
	for _, tc := range rejections {
		t.Run("reject "+tc.name, func(t *testing.T) {
			requireRejected(t, tc.body, chatBodyFromResponses)
		})
	}

	t.Run("blank optional fields are accepted", func(t *testing.T) {
		if _, err := chatBodyFromResponses([]byte(
			`{"model":"p/chat","input":"hi","background":false,"previous_response_id":null,"reasoning":{}}`)); err != nil {
			t.Fatalf("convert: %v", err)
		}
	})
}

func stringField(t *testing.T, top map[string]json.RawMessage, key string) string {
	t.Helper()
	raw, ok := top[key]
	if !ok {
		t.Fatalf("field %s missing in %v", key, keysOf(top))
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("field %s is not a string: %s", key, raw)
	}
	return s
}

func numberField(t *testing.T, top map[string]json.RawMessage, key string) float64 {
	t.Helper()
	raw, ok := top[key]
	if !ok {
		t.Fatalf("field %s missing in %v", key, keysOf(top))
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("field %s is not a number: %s", key, raw)
	}
	return f
}

func decodeInto(t *testing.T, raw json.RawMessage, target any) {
	t.Helper()
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
}

func keysOf(top map[string]json.RawMessage) []string {
	out := make([]string, 0, len(top))
	for k := range top {
		out = append(out, k)
	}
	return out
}

// ------------------------------------------------------------ non-stream out

func feedEnvelope(t *testing.T, kind envelopeKind, status int, contentType string, wantStream bool, chunks []string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	ew := newEnvelopeWriter(rec, kind, "p/chat", wantStream)
	if contentType != "" {
		ew.Header().Set("Content-Type", contentType)
	}
	ew.WriteHeader(status)
	for _, chunk := range chunks {
		if _, err := ew.Write([]byte(chunk)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	ew.finish()
	return rec
}

func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("envelope is not JSON: %v (body %q)", err, rec.Body.String())
	}
	return body
}

func TestEnvelopeWriterCompletionsNonStream(t *testing.T) {
	rec := feedEnvelope(t, envelopeCompletions, http.StatusOK, "application/json", false, []string{chatJSONReply})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q", ct)
	}
	body := decodeEnvelope(t, rec)
	if got := body["object"]; got != "text_completion" {
		t.Fatalf("object = %v, want text_completion", got)
	}
	if got := body["id"]; got != "chatcmpl-77" {
		t.Fatalf("id = %v, want the upstream chat id", got)
	}
	if got := body["model"]; got != "p/chat" {
		t.Fatalf("model = %v, want the client-facing name", got)
	}
	if got := body["created"]; got != float64(1700000001) {
		t.Fatalf("created = %v", got)
	}
	var choices []map[string]any
	decodeInto(t, marshalRaw(t, body["choices"]), &choices)
	if len(choices) != 1 {
		t.Fatalf("choices = %v", choices)
	}
	if choices[0]["text"] != "Hello world" {
		t.Fatalf("text = %v", choices[0]["text"])
	}
	if choices[0]["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v", choices[0]["finish_reason"])
	}
	if choices[0]["logprobs"] != nil {
		t.Fatalf("logprobs = %v, want null", choices[0]["logprobs"])
	}
	usage, ok := body["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage missing: %v", body)
	}
	if usage["prompt_tokens"] != float64(11) || usage["completion_tokens"] != float64(7) ||
		usage["total_tokens"] != float64(18) {
		t.Fatalf("usage = %v", usage)
	}
}

func TestEnvelopeWriterResponsesNonStream(t *testing.T) {
	rec := feedEnvelope(t, envelopeResponses, http.StatusOK, "application/json", false, []string{chatJSONReply})

	body := decodeEnvelope(t, rec)
	if got := body["object"]; got != "response" {
		t.Fatalf("object = %v, want response", got)
	}
	if got := body["status"]; got != "completed" {
		t.Fatalf("status = %v, want completed", got)
	}
	id, _ := body["id"].(string)
	if !strings.HasPrefix(id, "resp_") {
		t.Fatalf("id = %v, want a resp_ id (the upstream only speaks chatcmpl-)", body["id"])
	}
	if got := body["model"]; got != "p/chat" {
		t.Fatalf("model = %v", got)
	}
	if got := body["created_at"]; got != float64(1700000001) {
		t.Fatalf("created_at = %v", got)
	}
	if got := body["output_text"]; got != "Hello world" {
		t.Fatalf("output_text = %v", got)
	}

	var items []map[string]any
	decodeInto(t, marshalRaw(t, body["output"]), &items)
	wantTypes := []string{"reasoning", "function_call", "message"}
	if len(items) != len(wantTypes) {
		t.Fatalf("output items = %v, want %v", items, wantTypes)
	}
	for i, want := range wantTypes {
		if items[i]["type"] != want {
			t.Fatalf("output[%d].type = %v, want %s", i, items[i]["type"], want)
		}
	}
	call := items[1]
	if call["name"] != "get_weather" || call["call_id"] != "call_1" ||
		call["arguments"] != `{"city":"Paris"}` || call["status"] != "completed" {
		t.Fatalf("function_call item = %v", call)
	}
	reasoning, ok := items[0]["summary"].([]any)
	if !ok || len(reasoning) != 1 {
		t.Fatalf("reasoning item = %v", items[0])
	}
	part := reasoning[0].(map[string]any)
	if part["type"] != "summary_text" || part["text"] != "thinking hard" {
		t.Fatalf("summary part = %v", part)
	}
	var content []map[string]any
	decodeInto(t, marshalRaw(t, items[2]["content"]), &content)
	if len(content) != 1 || content[0]["type"] != "output_text" || content[0]["text"] != "Hello world" {
		t.Fatalf("message content = %v", content)
	}
	if items[2]["id"] == body["id"] {
		t.Fatal("the message item reused the response id")
	}

	usage, ok := body["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage missing: %v", body)
	}
	if usage["input_tokens"] != float64(11) || usage["output_tokens"] != float64(7) ||
		usage["total_tokens"] != float64(18) {
		t.Fatalf("usage = %v", usage)
	}
}

func TestEnvelopeWriterResponsesWithoutText(t *testing.T) {
	rec := feedEnvelope(t, envelopeResponses, http.StatusOK, "application/json", false,
		[]string{`{"id":"chatcmpl-2","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":""}}]}`})
	body := decodeEnvelope(t, rec)
	if items, ok := body["output"].([]any); !ok || len(items) != 0 {
		t.Fatalf("output = %v, want an empty list", body["output"])
	}
	if got := body["output_text"]; got != "" {
		t.Fatalf("output_text = %v", got)
	}
	if _, ok := body["usage"]; ok {
		t.Fatalf("usage fabricated: %v", body["usage"])
	}
}

func marshalRaw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

// -------------------------------------------------------------- error paths

func TestEnvelopeWriterPassesUpstreamErrorsThrough(t *testing.T) {
	const errBody = `{"error":{"message":"bad request","type":"invalid_request_error","code":"bad_request"}}`
	for _, status := range []int{http.StatusBadRequest, http.StatusInternalServerError} {
		for _, kind := range []envelopeKind{envelopeCompletions, envelopeResponses} {
			rec := feedEnvelope(t, kind, status, "application/json", false, []string{errBody})
			if rec.Code != status {
				t.Fatalf("kind %d status = %d, want %d", kind, rec.Code, status)
			}
			if got := rec.Body.String(); got != errBody {
				t.Fatalf("kind %d body = %q, want the upstream error verbatim", kind, got)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("kind %d Content-Type = %q, want the upstream media type kept", kind, got)
			}
		}
	}

	// Even a non-JSON error body is relayed with its own media type.
	rec := feedEnvelope(t, envelopeResponses, http.StatusBadGateway, "text/plain; charset=utf-8", false,
		[]string{"upstream died"})
	if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := rec.Body.String(); got != "upstream died" {
		t.Fatalf("body = %q", got)
	}
}

func TestEnvelopeWriterSurfacesErrorInsideSuccessfulBody(t *testing.T) {
	const inner = `{"message":"upstream exploded","type":"server_error","code":"internal"}`
	rec := feedEnvelope(t, envelopeResponses, http.StatusOK, "application/json", false,
		[]string{`{"id":"chatcmpl-3","error":` + inner + `}`})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want the upstream status preserved", rec.Code)
	}
	body := decodeEnvelope(t, rec)
	if _, ok := body["object"]; ok {
		t.Fatalf("an error reply was converted into an envelope: %v", body)
	}
	payload, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("error missing: %v", body)
	}
	if payload["message"] != "upstream exploded" || payload["code"] != "internal" {
		t.Fatalf("error = %v", payload)
	}
}

func TestEnvelopeWriterEmptyUpstreamBody(t *testing.T) {
	rec := feedEnvelope(t, envelopeCompletions, http.StatusOK, "application/json", false, []string{"   "})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body %s)", rec.Code, rec.Body.String())
	}
	if payload := decodeError(t, rec); payload.Error.Code != "empty_response" {
		t.Fatalf("error = %+v", payload.Error)
	}
}

func TestEnvelopeWriterUnparsableUpstreamBodyIsPassedThrough(t *testing.T) {
	rec := feedEnvelope(t, envelopeCompletions, http.StatusOK, "text/html; charset=utf-8", false,
		[]string{"<h1>not json</h1>"})
	if got := rec.Body.String(); got != "<h1>not json</h1>" {
		t.Fatalf("body = %q", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want the media type the upstream chose", got)
	}

	// With no media type from the upstream, the JSON default still applies.
	rec2 := feedEnvelope(t, envelopeCompletions, http.StatusOK, "", false, []string{`not json`})
	if got := rec2.Body.String(); got != "not json" {
		t.Fatalf("body = %q", got)
	}
	if got := rec2.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q", got)
	}
}

func TestEnvelopeWriterFinishIsIdempotent(t *testing.T) {
	rec := httptest.NewRecorder()
	ew := newEnvelopeWriter(rec, envelopeCompletions, "p/chat", false)
	ew.Header().Set("Content-Type", "application/json")
	ew.WriteHeader(http.StatusOK)
	_, _ = ew.Write([]byte(chatJSONReply))
	ew.finish()
	first := rec.Body.String()
	ew.finish()
	if got := rec.Body.String(); got != first {
		t.Fatalf("second finish changed the body:\n got %q\nwant %q", got, first)
	}
}

// ----------------------------------------------------------------- streaming

type sseEvent struct {
	event string
	data  string
}

// parseSSE splits an SSE body into (event name, data payload) pairs.
func parseSSE(t *testing.T, body string) []sseEvent {
	t.Helper()
	var out []sseEvent
	for _, block := range strings.Split(body, "\n\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		ev := sseEvent{}
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				if ev.data != "" {
					ev.data += "\n"
				}
				ev.data = ev.data + strings.TrimPrefix(line, "data: ")
			default:
				t.Fatalf("unexpected SSE line %q in block %q", line, block)
			}
		}
		out = append(out, ev)
	}
	return out
}

func streamFrames(withReasoning bool) []string {
	frames := []string{
		`data: {"id":"chatcmpl-9","object":"chat.completion.chunk","created":1700000009,"model":"chat",` +
			`"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"content":"Hel"}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"content":"lo "}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"content":"world"}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function",` +
			`"function":{"name":"get_weather","arguments":""}}]}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Paris\"}"}}]}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n",
		`data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}` + "\n\n",
		`data: [DONE]` + "\n\n",
	}
	if !withReasoning {
		return frames
	}
	reasoning := `data: {"choices":[{"index":0,"delta":{"reasoning_content":"because"}}]}` + "\n\n"
	return append([]string{frames[0], reasoning}, frames[1:]...)
}

func TestEnvelopeWriterCompletionsStream(t *testing.T) {
	rec := feedEnvelope(t, envelopeCompletions, http.StatusOK, "text/event-stream", true, streamFrames(false))

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}
	body := rec.Body.String()
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("stream does not end with the terminator: %q", body)
	}
	events := parseSSE(t, strings.TrimSuffix(body, "data: [DONE]\n\n"))
	if len(events) != 4 {
		t.Fatalf("chunk count = %d, want 3 text chunks and 1 final chunk (%q)", len(events), body)
	}
	var texts []string
	for i, ev := range events {
		if ev.event != "" {
			t.Fatalf("event %d carries a name %q; text completions are unnamed", i, ev.event)
		}
		var chunk struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Model   string `json:"model"`
			Created int64  `json:"created"`
			Choices []struct {
				Index        int     `json:"index"`
				Text         string  `json:"text"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *map[string]any `json:"usage"`
		}
		decodeInto(t, []byte(ev.data), &chunk)
		if chunk.Object != "text_completion" || chunk.ID != "chatcmpl-9" ||
			chunk.Model != "p/chat" || chunk.Created != 1700000009 {
			t.Fatalf("chunk %d = %s", i, ev.data)
		}
		if len(chunk.Choices) != 1 || chunk.Choices[0].Index != 0 {
			t.Fatalf("chunk %d choices = %+v", i, chunk.Choices)
		}
		texts = append(texts, chunk.Choices[0].Text)
		last := i == len(events)-1
		if last != (chunk.Choices[0].FinishReason != nil) {
			t.Fatalf("chunk %d finish_reason = %v, want it only on the last chunk", i, chunk.Choices[0].FinishReason)
		}
		if last {
			if got := *chunk.Choices[0].FinishReason; got != "tool_calls" {
				t.Fatalf("final finish_reason = %q", got)
			}
			if chunk.Usage == nil {
				t.Fatal("usage was not carried on the final chunk")
			}
			if (*chunk.Usage)["prompt_tokens"] != float64(11) {
				t.Fatalf("usage = %v", *chunk.Usage)
			}
		}
	}
	if got := strings.Join(texts[:3], ""); got != "Hello world" {
		t.Fatalf("streamed text = %q", got)
	}
	if strings.Count(body, "[DONE]") != 1 {
		t.Fatalf("the upstream terminator leaked: %q", body)
	}
}

func TestEnvelopeWriterResponsesStream(t *testing.T) {
	rec := feedEnvelope(t, envelopeResponses, http.StatusOK, "text/event-stream", true, streamFrames(false))

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}
	body := rec.Body.String()
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("stream does not end with the terminator: %q", body)
	}
	events := parseSSE(t, strings.TrimSuffix(body, "data: [DONE]\n\n"))

	// The Responses envelope owns its id; the upstream chat id (chatcmpl-77)
	// must not leak into any event.
	var created map[string]any
	decodeInto(t, []byte(events[0].data), &created)
	response, _ := created["response"].(map[string]any)
	if id, _ := response["id"].(string); !strings.HasPrefix(id, "resp_") {
		t.Fatalf("response.created id = %v, want a resp_ id", response["id"])
	}
	if strings.Contains(body, "chatcmpl-77") {
		t.Fatalf("stream leaked the upstream chat id: %q", body)
	}

	wantSequence := []string{
		"response.created", "response.in_progress",
		"response.output_item.added", "response.content_part.added",
		"response.output_text.delta", "response.output_text.delta", "response.output_text.delta",
		"response.output_text.done", "response.content_part.done", "response.output_item.done",
		"response.output_item.added", "response.function_call_arguments.delta",
		"response.function_call_arguments.done", "response.output_item.done",
		"response.completed",
	}
	if len(events) != len(wantSequence) {
		t.Fatalf("event sequence = %v\nwant %v", eventNames(events), wantSequence)
	}
	for i, want := range wantSequence {
		if events[i].event != want {
			t.Fatalf("event %d = %q, want %q (sequence %v)", i, events[i].event, want, eventNames(events))
		}
		var probe map[string]any
		decodeInto(t, []byte(events[i].data), &probe)
		if probe["type"] != want {
			t.Fatalf("event %d data.type = %v, want %q", i, probe["type"], want)
		}
		seq, ok := probe["sequence_number"].(float64)
		if !ok || int(seq) != i+1 {
			t.Fatalf("event %d sequence_number = %v, want %d", i, probe["sequence_number"], i+1)
		}
	}

	// The deltas must concatenate to the full text and match the done events.
	var streamed strings.Builder
	var doneText string
	for _, ev := range events {
		var payload map[string]any
		decodeInto(t, []byte(ev.data), &payload)
		switch ev.event {
		case "response.output_text.delta":
			streamed.WriteString(payload["delta"].(string))
		case "response.output_text.done":
			doneText = payload["text"].(string)
		}
	}
	if streamed.String() != "Hello world" {
		t.Fatalf("concatenated deltas = %q", streamed.String())
	}
	if doneText != "Hello world" {
		t.Fatalf("output_text.done = %q", doneText)
	}

	var completed struct {
		Response struct {
			ID         string `json:"id"`
			Object     string `json:"object"`
			Status     string `json:"status"`
			Model      string `json:"model"`
			CreatedAt  int64  `json:"created_at"`
			OutputText string `json:"output_text"`
			Output     []struct {
				Type      string `json:"type"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
				CallID    string `json:"call_id"`
			} `json:"output"`
			Usage map[string]any `json:"usage"`
		} `json:"response"`
	}
	decodeInto(t, []byte(events[len(events)-1].data), &completed)
	resp := completed.Response
	if !strings.HasPrefix(resp.ID, "resp_") || resp.Object != "response" || resp.Status != "completed" ||
		resp.Model != "p/chat" || resp.CreatedAt != 1700000009 {
		t.Fatalf("completed response = %+v", resp)
	}
	if resp.OutputText != "Hello world" {
		t.Fatalf("output_text = %q", resp.OutputText)
	}
	if len(resp.Output) != 2 || resp.Output[0].Type != "message" || resp.Output[1].Type != "function_call" {
		t.Fatalf("output items = %+v", resp.Output)
	}
	call := resp.Output[1]
	if call.Name != "get_weather" || call.CallID != "call_1" || call.Arguments != `{"city":"Paris"}` {
		t.Fatalf("function_call item = %+v (split arguments were not merged)", call)
	}
	if resp.Usage["input_tokens"] != float64(11) || resp.Usage["output_tokens"] != float64(7) ||
		resp.Usage["total_tokens"] != float64(18) {
		t.Fatalf("usage = %v", resp.Usage)
	}
}

func TestEnvelopeWriterResponsesStreamReasoningPrecedesMessage(t *testing.T) {
	rec := feedEnvelope(t, envelopeResponses, http.StatusOK, "text/event-stream", true, streamFrames(true))
	events := parseSSE(t, strings.TrimSuffix(rec.Body.String(), "data: [DONE]\n\n"))

	names := eventNames(events)
	if names[0] != "response.created" || names[1] != "response.in_progress" ||
		names[2] != "response.output_item.added" || names[3] != "response.output_item.done" {
		t.Fatalf("sequence = %v, want the reasoning item added and done before the message", names)
	}
	var firstItem struct {
		Item struct {
			Type    string `json:"type"`
			Summary []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"summary"`
		} `json:"item"`
	}
	decodeInto(t, []byte(events[2].data), &firstItem)
	if firstItem.Item.Type != "reasoning" || len(firstItem.Item.Summary) != 1 ||
		firstItem.Item.Summary[0].Text != "because" {
		t.Fatalf("reasoning item = %+v", firstItem.Item)
	}
	// output_index must increase with the items and the completed list must match.
	var completed struct {
		Response struct {
			Output []map[string]any `json:"output"`
		} `json:"response"`
	}
	decodeInto(t, []byte(events[len(events)-1].data), &completed)
	if len(completed.Response.Output) != 3 {
		t.Fatalf("output = %v", completed.Response.Output)
	}
	wantTypes := []string{"reasoning", "message", "function_call"}
	for i, want := range wantTypes {
		if got := completed.Response.Output[i]["type"]; got != want {
			t.Fatalf("output[%d].type = %v, want %s", i, got, want)
		}
	}
	indexes := []int{}
	for _, ev := range events {
		if ev.event != "response.output_item.added" && ev.event != "response.output_item.done" &&
			ev.event != "response.output_text.delta" {
			continue
		}
		var payload struct {
			OutputIndex int `json:"output_index"`
		}
		decodeInto(t, []byte(ev.data), &payload)
		indexes = append(indexes, payload.OutputIndex)
	}
	if !reflect.DeepEqual(indexes, []int{0, 0, 1, 1, 1, 1, 1, 2, 2}) {
		t.Fatalf("output_index sequence = %v, want the reasoning item at 0, the message at 1 and the call at 2", indexes)
	}
}

func TestEnvelopeWriterResponsesStreamLateReasoningStaysConsistent(t *testing.T) {
	// A plugin that yields reasoning after its text cannot be re-ordered once
	// the deltas went out; the gateway mirrors the upstream order in the events
	// and in response.completed so the two never disagree.
	frames := []string{
		`data: {"id":"chatcmpl-6","choices":[{"index":0,"delta":{"content":"Hi"}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"reasoning_content":"after"}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
		`data: [DONE]` + "\n\n",
	}
	rec := feedEnvelope(t, envelopeResponses, http.StatusOK, "text/event-stream", true, frames)
	events := parseSSE(t, strings.TrimSuffix(rec.Body.String(), "data: [DONE]\n\n"))

	var added []struct {
		OutputIndex int            `json:"output_index"`
		Item        map[string]any `json:"item"`
	}
	for _, ev := range events {
		if ev.event == "response.output_item.added" {
			var payload struct {
				OutputIndex int            `json:"output_index"`
				Item        map[string]any `json:"item"`
			}
			decodeInto(t, []byte(ev.data), &payload)
			added = append(added, payload)
		}
	}
	var completed struct {
		Response struct {
			Output []map[string]any `json:"output"`
		} `json:"response"`
	}
	decodeInto(t, []byte(events[len(events)-1].data), &completed)
	if len(added) != 2 || len(completed.Response.Output) != 2 {
		t.Fatalf("added = %+v output = %v", added, completed.Response.Output)
	}
	for i := range added {
		if added[i].OutputIndex != i {
			t.Fatalf("event output_index %d = %d", i, added[i].OutputIndex)
		}
		if added[i].Item["type"] != completed.Response.Output[i]["type"] {
			t.Fatalf("item %d: events say %v, response.completed says %v",
				i, added[i].Item["type"], completed.Response.Output[i]["type"])
		}
	}
}

func TestEnvelopeWriterResponsesStreamSplitsFrames(t *testing.T) {
	frames := streamFrames(false)
	rec := httptest.NewRecorder()
	ew := newEnvelopeWriter(rec, envelopeResponses, "p/chat", true)
	ew.Header().Set("Content-Type", "text/event-stream")
	ew.WriteHeader(http.StatusOK)
	// Feed the stream one byte at a time: partial lines must be buffered.
	for _, frame := range frames {
		for i := 0; i < len(frame); i++ {
			if _, err := ew.Write([]byte(frame[i : i+1])); err != nil {
				t.Fatalf("write byte: %v", err)
			}
		}
	}
	ew.finish()

	events := parseSSE(t, strings.TrimSuffix(rec.Body.String(), "data: [DONE]\n\n"))
	if len(events) != 15 {
		t.Fatalf("event count = %d, want 15 (%v)", len(events), eventNames(events))
	}
}

func TestEnvelopeWriterResponsesStreamToolCallOnly(t *testing.T) {
	frames := []string{
		`data: {"id":"chatcmpl-5","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_2",` +
			`"function":{"name":"lookup","arguments":"{}"}}],"finish_reason":"tool_calls"}}]}` + "\n\n",
		`data: [DONE]` + "\n\n",
	}
	rec := feedEnvelope(t, envelopeResponses, http.StatusOK, "text/event-stream", true, frames)
	events := parseSSE(t, strings.TrimSuffix(rec.Body.String(), "data: [DONE]\n\n"))
	names := eventNames(events)
	if names[0] != "response.created" || names[len(names)-1] != "response.completed" {
		t.Fatalf("sequence = %v, want created and completed around a tool-call-only stream", names)
	}
	for _, name := range names {
		if name == "response.output_text.done" || name == "response.content_part.added" {
			t.Fatalf("empty message events emitted for a tool-call-only stream: %v", names)
		}
	}
	var completed struct {
		Response struct {
			Output []map[string]any `json:"output"`
		} `json:"response"`
	}
	decodeInto(t, []byte(events[len(events)-1].data), &completed)
	if len(completed.Response.Output) != 1 || completed.Response.Output[0]["type"] != "function_call" {
		t.Fatalf("output = %v", completed.Response.Output)
	}
	if completed.Response.Output[0]["arguments"] != "{}" {
		t.Fatalf("arguments = %v", completed.Response.Output[0]["arguments"])
	}
}

func TestEnvelopeWriterStreamSplitsToolCallsWithoutIndex(t *testing.T) {
	frames := []string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"id":"c1","type":"function",` +
			`"function":{"name":"one","arguments":""}}]}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"{}"}}]}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"id":"c2","type":"function",` +
			`"function":{"name":"two","arguments":"[]"}}]}}]}` + "\n\n",
		`data: [DONE]` + "\n\n",
	}
	rec := feedEnvelope(t, envelopeResponses, http.StatusOK, "text/event-stream", true, frames)
	events := parseSSE(t, strings.TrimSuffix(rec.Body.String(), "data: [DONE]\n\n"))

	var completed struct {
		Response struct {
			Output []struct {
				Type      string `json:"type"`
				Name      string `json:"name"`
				CallID    string `json:"call_id"`
				Arguments string `json:"arguments"`
			} `json:"output"`
		} `json:"response"`
	}
	decodeInto(t, []byte(events[len(events)-1].data), &completed)
	want := []struct {
		name, callID, args string
	}{{"one", "c1", "{}"}, {"two", "c2", "[]"}}
	if len(completed.Response.Output) != len(want) {
		t.Fatalf("output = %+v, want two calls", completed.Response.Output)
	}
	for i, expect := range want {
		item := completed.Response.Output[i]
		if item.Type != "function_call" || item.Name != expect.name ||
			item.CallID != expect.callID || item.Arguments != expect.args {
			t.Fatalf("output[%d] = %+v, want %v", i, item, expect)
		}
	}
}

func TestEnvelopeWriterStreamWithoutContentTypeHeader(t *testing.T) {
	// wantStream is the only hint when the upstream omits the media type.
	rec := feedEnvelope(t, envelopeCompletions, http.StatusOK, "", true, streamFrames(false))
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want the stream to be detected from the request", ct)
	}
	rec2 := feedEnvelope(t, envelopeCompletions, http.StatusOK, "", false, []string{chatJSONReply})
	if got := rec2.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q, want the buffered path", got)
	}
	// An explicit JSON media type wins over the client's stream flag.
	rec3 := feedEnvelope(t, envelopeCompletions, http.StatusOK, "application/json", true, []string{chatJSONReply})
	if got := rec3.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q, want conversion of the buffered reply", got)
	}
	body3 := decodeEnvelope(t, rec3)
	if body3["object"] != "text_completion" {
		t.Fatalf("body = %v", body3)
	}
}

func eventNames(events []sseEvent) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.event)
	}
	return out
}

// ------------------------------------------------------------------- routes

func pluginRegistry() *fakeRegistry {
	return newFakeRegistry().add(pluginStatus("p", true, model.ModelInfo{ID: "chat"}), "", "")
}

func TestEnvelopeRoutesToPlugin(t *testing.T) {
	cases := []struct {
		name         string
		path         string
		body         string
		wantRoles    []string
		wantContents []string
		wantObject   string
	}{
		{
			name:         "completions",
			path:         "/v1/completions",
			body:         `{"model":"p/chat","prompt":"hello","max_tokens":8}`,
			wantObject:   "text_completion",
			wantRoles:    []string{"user"},
			wantContents: []string{"hello"},
		},
		{
			name:         "responses",
			path:         "/v1/responses",
			body:         `{"model":"p/chat","instructions":"be brief","input":"hello"}`,
			wantObject:   "response",
			wantRoles:    []string{"system", "user"},
			wantContents: []string{"be brief", "hello"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, calls := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Upstream", "plugin")
				_, _ = io.WriteString(w, chatJSONReply)
			})
			reg := newFakeRegistry().add(
				pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "plug-token")
			logs := &fakeLogSink{}
			g := newTestGateway(t, Options{Plugins: reg, Log: logs})

			rr := serve(g, newChatRequest(t, tc.path, clientKey, tc.body))
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
			}
			if calls.count() != 1 {
				t.Fatalf("plugin calls = %d, want 1", calls.count())
			}
			up := calls.last(t)
			if up.Path != "/v1/chat/completions" {
				t.Fatalf("plugin path = %s, want /v1/chat/completions", up.Path)
			}
			if up.Authorization != "Bearer plug-token" {
				t.Fatalf("plugin Authorization = %q", up.Authorization)
			}
			req := decodeChatRequest(t, up.Body)
			if req.Model != "chat" {
				t.Fatalf("plugin model = %q, want the physical name", req.Model)
			}
			if len(req.Messages) != len(tc.wantRoles) {
				t.Fatalf("plugin messages = %+v, want %d", req.Messages, len(tc.wantRoles))
			}
			for i, want := range tc.wantRoles {
				if req.Messages[i].Role != want {
					t.Fatalf("message %d role = %q, want %q", i, req.Messages[i].Role, want)
				}
				if got := req.Messages[i].Content; got != tc.wantContents[i] {
					t.Fatalf("message %d content = %q, want %q", i, got, tc.wantContents[i])
				}
			}
			if strings.Contains(string(up.Body), `"prompt"`) {
				t.Fatalf("prompt was not converted: %s", up.Body)
			}

			if got := rr.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
				t.Fatalf("Content-Type = %q", got)
			}
			if got := rr.Header().Get(sourceHeader); got != "plugin:p" {
				t.Fatalf("source header = %q, want plugin:p", got)
			}
			body := decodeEnvelope(t, rr)
			if body["object"] != tc.wantObject {
				t.Fatalf("object = %v, want %s", body["object"], tc.wantObject)
			}
			if body["model"] != "p/chat" {
				t.Fatalf("model = %v, want the client-facing name", body["model"])
			}

			row := logs.wait(t, 1)[0]
			if row.Source != "plugin:p" || row.Model != "p/chat" || row.Status != http.StatusOK {
				t.Fatalf("log row = %+v", row)
			}
			if row.PromptTokens != 11 || row.CompletionTokens != 7 {
				t.Fatalf("log usage = %d/%d", row.PromptTokens, row.CompletionTokens)
			}
		})
	}
}

func TestEnvelopeRouteStreamsSSE(t *testing.T) {
	frames := streamFrames(false)
	srv, calls := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, frame := range frames {
			_, _ = io.WriteString(w, frame)
			w.(http.Flusher).Flush()
		}
	})
	reg := newFakeRegistry().add(
		pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "plug-token")
	logs := &fakeLogSink{}
	g := newTestGateway(t, Options{Plugins: reg, Log: logs})

	rr := serve(g, newChatRequest(t, "/v1/responses", clientKey, `{"model":"p/chat","input":"hi","stream":true}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if got := rr.Header().Get(sourceHeader); got != "plugin:p" {
		t.Fatalf("source header = %q", got)
	}
	if calls.count() != 1 {
		t.Fatalf("plugin calls = %d, want 1", calls.count())
	}
	up := calls.last(t)
	if up.Path != "/v1/chat/completions" {
		t.Fatalf("plugin path = %s", up.Path)
	}
	req := decodeChatRequest(t, up.Body)
	if got := string(req.top["stream"]); got != "true" {
		t.Fatalf("stream flag was not forwarded: %s", up.Body)
	}

	events := parseSSE(t, strings.TrimSuffix(rr.Body.String(), "data: [DONE]\n\n"))
	if len(events) != 15 {
		t.Fatalf("event count = %d, want 15 (%v)", len(events), eventNames(events))
	}
	if events[0].event != "response.created" || events[len(events)-1].event != "response.completed" {
		t.Fatalf("sequence = %v", eventNames(events))
	}
	row := logs.wait(t, 1)[0]
	if !row.Stream || row.Status != http.StatusOK {
		t.Fatalf("log row = %+v", row)
	}
}

func TestEnvelopeRouteUsesScheduler(t *testing.T) {
	srv, calls := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(accountHeader, r.Header.Get(accountHeader))
		_, _ = io.WriteString(w, chatJSONReply)
	})
	reg := newFakeRegistry().add(
		pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	logs := &fakeLogSink{}
	g := scheduledGateway(t, reg, logs, singlePoolSnapshot("p1", 2, "acc1"))

	rr := serve(g, newChatRequest(t, "/v1/completions", clientKey, `{"model":"p1/m1","prompt":"ping"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	if got := calls.last(t).Header.Get(accountHeader); got != "acc1" {
		t.Fatalf("pinned account = %q, want acc1", got)
	}
	if got := rr.Header().Get(sourceHeader); got != "plugin:p1" {
		t.Fatalf("source header = %q, want plugin:p1", got)
	}
	body := decodeEnvelope(t, rr)
	if body["object"] != "text_completion" {
		t.Fatalf("body = %v", body)
	}
	row := logs.wait(t, 1)[0]
	if row.Account != "acc1" || row.Source != "plugin:p1" {
		t.Fatalf("log row = %+v", row)
	}
}

func TestEnvelopeBareModelKeepsPlainPassthrough(t *testing.T) {
	aSrv, aCalls := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"a":"original"}`))
	logs := &fakeLogSink{}
	g := newTestGateway(t, Options{
		Plugins: newFakeRegistry(),
		A:       &fakeA{base: aSrv.URL, key: "sk-a"},
		Log:     logs,
	})

	for _, path := range []string{"/v1/completions", "/v1/responses"} {
		body := `{"model":"gpt-4o","prompt":"hello","input":"hello","echo":true,"n":2}`
		rr := serve(g, newChatRequest(t, path, clientKey, body))
		if rr.Code != http.StatusOK {
			t.Fatalf("%s status = %d (body %s)", path, rr.Code, rr.Body.String())
		}
		if got := rr.Body.String(); got != `{"a":"original"}` {
			t.Fatalf("%s body = %q, want A's answer verbatim", path, got)
		}
		up := aCalls.last(t)
		if up.Path != path {
			t.Fatalf("%s A path = %s", path, up.Path)
		}
		if string(up.Body) != body {
			t.Fatalf("%s A body = %q, want the original %q", path, up.Body, body)
		}
		if got := rr.Header().Get(sourceHeader); got != "a" {
			t.Fatalf("%s source header = %q, want a", path, got)
		}
	}
}

// TestEnvelopeCompletionsRouteAcceptsFalsyParams pins the 400 regression: an SDK
// sending the OpenAI defaults (echo:false, suffix:"") gets a normal completion,
// while echo:true still reports unsupported_request.
func TestEnvelopeCompletionsRouteAcceptsFalsyParams(t *testing.T) {
	srv, calls := newUpstreamServer(t, jsonResponder(http.StatusOK, chatJSONReply))
	reg := newFakeRegistry().add(
		pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "tok")
	g := newTestGateway(t, Options{Plugins: reg, Log: &fakeLogSink{}})

	rr := serve(g, newChatRequest(t, "/v1/completions", clientKey,
		`{"model":"p/chat","prompt":"hi","echo":false,"suffix":""}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("echo:false status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	if calls.count() != 1 {
		t.Fatalf("plugin calls = %d, want 1", calls.count())
	}

	rr = serve(g, newChatRequest(t, "/v1/completions", clientKey,
		`{"model":"p/chat","prompt":"hi","echo":true}`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("echo:true status = %d, want 400 (body %s)", rr.Code, rr.Body.String())
	}
	if payload := decodeError(t, rr); payload.Error.Code != "unsupported_request" {
		t.Fatalf("echo:true error = %+v, want unsupported_request", payload.Error)
	}
	if calls.count() != 1 {
		t.Fatalf("plugin calls = %d, want the rejected request to stop before the upstream", calls.count())
	}
}

func TestEnvelopeCompletionsRouteStreamsSSE(t *testing.T) {
	frames := streamFrames(false)
	srv, calls := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, frame := range frames {
			_, _ = io.WriteString(w, frame)
		}
	})
	reg := newFakeRegistry().add(
		pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "plug-token")
	logs := &fakeLogSink{}
	g := newTestGateway(t, Options{Plugins: reg, Log: logs})

	rr := serve(g, newChatRequest(t, "/v1/completions", clientKey, `{"model":"p/chat","prompt":"hi","stream":true}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}
	body := rr.Body.String()
	if !strings.HasSuffix(body, "data: [DONE]\n\n") || strings.Count(body, "[DONE]") != 1 {
		t.Fatalf("terminator = %q", body)
	}
	events := parseSSE(t, strings.TrimSuffix(body, "data: [DONE]\n\n"))
	if len(events) != 4 {
		t.Fatalf("chunk count = %d (%v)", len(events), eventNames(events))
	}
	if calls.count() != 1 {
		t.Fatalf("plugin calls = %d", calls.count())
	}
	if got := string(decodeChatRequest(t, calls.last(t).Body).top["stream"]); got != "true" {
		t.Fatalf("stream flag was not forwarded: %s", calls.last(t).Body)
	}
}

// TestEnvelopeRoutePassesUpstreamErrorThrough covers both shapes of a failing
// plugin: a single non-retryable answer and the buffered fail-over that the
// scheduler writes once every candidate is exhausted.
func TestEnvelopeRoutePassesUpstreamErrorThrough(t *testing.T) {
	const errBody = `{"error":{"message":"upstream says no","type":"server_error","code":"boom"}}`

	t.Run("plugin answers the error", func(t *testing.T) {
		srv, calls := newUpstreamServer(t, jsonResponder(http.StatusInternalServerError, errBody))
		reg := newFakeRegistry().add(
			pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "tok")
		logs := &fakeLogSink{}
		g := newTestGateway(t, Options{Plugins: reg, Log: logs})

		rr := serve(g, newChatRequest(t, "/v1/responses", clientKey, `{"model":"p/chat","input":"hi"}`))
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d (body %s)", rr.Code, rr.Body.String())
		}
		if got := rr.Body.String(); got != errBody {
			t.Fatalf("body = %q, want the chat error verbatim", got)
		}
		if got := rr.Header().Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q", got)
		}
		if got := rr.Header().Get(sourceHeader); got != "plugin:p" {
			t.Fatalf("source header = %q", got)
		}
		if calls.count() != 1 {
			t.Fatalf("plugin calls = %d", calls.count())
		}
		row := logs.wait(t, 1)[0]
		if row.Status != http.StatusInternalServerError || row.Error == "" {
			t.Fatalf("log row = %+v", row)
		}
	})

	t.Run("scheduler exhausts its candidates", func(t *testing.T) {
		srv, calls := newUpstreamServer(t, jsonResponder(http.StatusTooManyRequests, errBody))
		reg := newFakeRegistry().add(
			pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
		g := scheduledGateway(t, reg, &fakeLogSink{}, singlePoolSnapshot("p1", 2, "acc1", "acc2"))

		rr := serve(g, newChatRequest(t, "/v1/completions", clientKey, `{"model":"p1/m1","prompt":"ping"}`))
		if rr.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d (body %s)", rr.Code, rr.Body.String())
		}
		if got := rr.Body.String(); got != errBody {
			t.Fatalf("body = %q, want the last upstream error verbatim", got)
		}
		if calls.count() < 2 {
			t.Fatalf("plugin calls = %d, want a fail-over attempt", calls.count())
		}
	})
}

// TestEnvelopeRouteUnknownPluginModelIsNotFound pins "<plugin>/" to 404. The
// scheduled path used to translate the request and then fall back to A, which
// handed A a chat body on the completions URL.
func TestEnvelopeRouteUnknownPluginModelIsNotFound(t *testing.T) {
	const errBody = `{"a":"answer"}`
	aSrv, aCalls := newUpstreamServer(t, jsonResponder(http.StatusOK, errBody))
	srv, pCalls := newUpstreamServer(t, jsonResponder(http.StatusOK, chatJSONReply))
	reg := newFakeRegistry().add(pluginStatus("p", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")

	gateways := map[string]*Gateway{
		"plain": newTestGateway(t, Options{Plugins: reg, A: &fakeA{base: aSrv.URL, key: "sk-a"}}),
		"scheduled": newTestGateway(t, Options{
			Plugins:  reg,
			A:        &fakeA{base: aSrv.URL, key: "sk-a"},
			Schedule: scheduler.New(scheduler.Options{}),
			Snapshot: staticSnapshot(singlePoolSnapshot("p", 2, "acc1")),
		}),
	}
	for name, g := range gateways {
		for _, tc := range []struct{ path, body string }{
			{"/v1/completions", `{"model":"p/","prompt":"hi"}`},
			{"/v1/responses", `{"model":"p/","input":"hi"}`},
		} {
			rr := serve(g, newChatRequest(t, tc.path, clientKey, tc.body))
			if rr.Code != http.StatusNotFound {
				t.Fatalf("%s %s status = %d (body %s)", name, tc.path, rr.Code, rr.Body.String())
			}
			if payload := decodeError(t, rr); payload.Error.Code != "model_not_found" {
				t.Fatalf("%s %s error = %+v", name, tc.path, payload.Error)
			}
		}
	}
	if aCalls.count() != 0 {
		t.Fatalf("A calls = %d, want an unnamed plugin model never to reach A", aCalls.count())
	}
	if pCalls.count() != 0 {
		t.Fatalf("plugin calls = %d", pCalls.count())
	}
}

func TestEnvelopeRouteAppliesDowngradeAndReasoningDefault(t *testing.T) {
	srv, calls := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(accountHeader, r.Header.Get(accountHeader))
		_, _ = io.WriteString(w, chatJSONReply)
	})
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	snap := singlePoolSnapshot("p1", 2, "acc1")
	snap.Pools["p1"].BackgroundDowngrade = true
	snap.Pools["p1"].DefaultReasoning = "low"
	snap.Pools["p1"].Models = map[string]model.ModelInfo{
		"m1":       {ID: "m1", Multiplier: 2},
		"m1-flash": {ID: "m1-flash", Multiplier: 0.5},
	}
	logs := &fakeLogSink{}
	g := scheduledGateway(t, reg, logs, snap)

	req := newChatRequest(t, "/v1/responses", clientKey, `{"model":"p1/m1","input":"ping","max_output_tokens":64}`)
	rr := serve(g, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", rr.Code, rr.Body.String())
	}
	// Scheduler headers must survive the conversion: they are set on the
	// envelope writer, whose buffered headers only reach the client on flush.
	if got := rr.Header().Get(downgradedHeader); got != scheduler.DowngradeBackground {
		t.Fatalf("downgraded header = %q, want %q", got, scheduler.DowngradeBackground)
	}
	if got := rr.Header().Get(accountHeader); got != "acc1" {
		t.Fatalf("account header = %q", got)
	}
	up := decodeChatRequest(t, calls.last(t).Body)
	if up.Model != "m1-flash" {
		t.Fatalf("upstream model = %q, want the downgraded physical name", up.Model)
	}
	if got := stringField(t, up.top, "reasoning_effort"); got != "low" {
		t.Fatalf("reasoning_effort = %q, want the pool default", got)
	}
	if got := numberField(t, up.top, "max_tokens"); got != 64 {
		t.Fatalf("max_tokens = %v, want max_output_tokens mapped", got)
	}
	body := decodeEnvelope(t, rr)
	if body["object"] != "response" || body["model"] != "p1/m1" {
		t.Fatalf("envelope = %v", body)
	}
	row := logs.wait(t, 1)[0]
	if row.Account != "acc1" || row.Model != "p1/m1" || row.Source != "plugin:p1" {
		t.Fatalf("log row = %+v", row)
	}
}

func TestEnvelopePluginRejectsUnsupportedRequest(t *testing.T) {
	srv, calls := newUpstreamServer(t, jsonResponder(http.StatusOK, chatJSONReply))
	reg := newFakeRegistry().add(
		pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "tok")
	logs := &fakeLogSink{}
	g := newTestGateway(t, Options{Plugins: reg, Log: logs})

	cases := []struct{ name, path, body string }{
		{"completions n", "/v1/completions", `{"model":"p/chat","prompt":"x","n":2}`},
		{"responses background", "/v1/responses", `{"model":"p/chat","input":"x","background":true}`},
		{"responses empty input", "/v1/responses", `{"model":"p/chat"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := calls.count()
			rr := serve(g, newChatRequest(t, tc.path, clientKey, tc.body))
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rr.Code, rr.Body.String())
			}
			payload := decodeError(t, rr)
			if payload.Error.Code != "unsupported_request" {
				t.Fatalf("error code = %q", payload.Error.Code)
			}
			if calls.count() != before {
				t.Fatal("rejected request must not reach the plugin")
			}
		})
	}
	rows := logs.wait(t, len(cases))
	for _, row := range rows {
		if row.Status != http.StatusBadRequest {
			t.Fatalf("log row = %+v", row)
		}
	}
}

func TestEnvelopePluginAcceptsPreviousResponseIDWithInput(t *testing.T) {
	srv, calls := newUpstreamServer(t, jsonResponder(http.StatusOK, chatJSONReply))
	reg := newFakeRegistry().add(
		pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "tok")
	g := newTestGateway(t, Options{Plugins: reg, Log: &fakeLogSink{}})

	rr := serve(g, newChatRequest(t, "/v1/responses", clientKey,
		`{"model":"p/chat","input":"x","previous_response_id":"r1"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	if calls.count() != 1 {
		t.Fatalf("plugin calls = %d, want 1", calls.count())
	}
}

func TestEnvelopePluginUnavailableKeepsErrorShape(t *testing.T) {
	reg := pluginRegistry()
	reg.setEnsureErr("p", errors.New("boom"))
	logs := &fakeLogSink{}
	g := newTestGateway(t, Options{Plugins: reg, Log: logs})

	for _, kind := range []struct {
		path string
		body string
	}{
		{"/v1/completions", `{"model":"p/chat","prompt":"x"}`},
		{"/v1/responses", `{"model":"p/chat","input":"x"}`},
	} {
		rr := serve(g, newChatRequest(t, kind.path, clientKey, kind.body))
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s status = %d (body %s)", kind.path, rr.Code, rr.Body.String())
		}
		if payload := decodeError(t, rr); payload.Error.Type != "plugin_unavailable" {
			t.Fatalf("%s error = %+v", kind.path, payload.Error)
		}
		if got := rr.Header().Get(sourceHeader); got != "plugin:p" {
			t.Fatalf("%s source header = %q", kind.path, got)
		}
	}
	rows := logs.wait(t, 2)
	for _, row := range rows {
		if row.Status != http.StatusServiceUnavailable {
			t.Fatalf("log row = %+v", row)
		}
	}
}

// ---------------------------------------------------------------------- auth

func TestClientApiKeyHeaders(t *testing.T) {
	srv, calls := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"ok":true}`))
	reg := newFakeRegistry().add(
		pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "plug-token")
	keys := newFakeKeys()
	keys.tokens["agk_second"] = keyIdentity{id: "key-2", name: "second"}
	logs := &fakeLogSink{}
	g := newTestGateway(t, Options{Plugins: reg, Keys: keys, Log: logs})

	const body = `{"model":"p/chat","messages":[]}`
	cases := []struct {
		name    string
		headers map[string]string
		want    int
		keyID   string
	}{
		{"bearer", map[string]string{"Authorization": "Bearer " + clientKey}, http.StatusOK, "key-1"},
		{"lowercase bearer", map[string]string{"Authorization": "bearer " + clientKey}, http.StatusOK, "key-1"},
		{"x-api-key", map[string]string{"x-api-key": clientKey}, http.StatusOK, "key-1"},
		{"canonical x-api-key", map[string]string{"X-Api-Key": clientKey}, http.StatusOK, "key-1"},
		{"api-key", map[string]string{"api-key": clientKey}, http.StatusOK, "key-1"},
		{"wrong x-api-key", map[string]string{"x-api-key": "nope"}, http.StatusUnauthorized, ""},
		{"wrong api-key", map[string]string{"api-key": "nope"}, http.StatusUnauthorized, ""},
		{"bearer wins over api-key", map[string]string{
			"Authorization": "Bearer agk_second", "api-key": clientKey}, http.StatusOK, "key-2"},
		{"no credential", nil, http.StatusUnauthorized, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := calls.count()
			req := newChatRequest(t, "/v1/chat/completions", "", body)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rr := serve(g, req)
			if rr.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", rr.Code, tc.want, rr.Body.String())
			}
			if tc.want == http.StatusUnauthorized {
				if payload := decodeError(t, rr); payload.Error.Code != "invalid_api_key" {
					t.Fatalf("error = %+v", payload.Error)
				}
			}
			if calls.count() != before+bool2int(tc.want == http.StatusOK) {
				t.Fatalf("plugin calls = %d", calls.count())
			}
		})
	}

	rows := logs.wait(t, len(cases))
	wantKeys := []string{"key-1", "key-1", "key-1", "key-1", "key-1", "", "", "key-2", ""}
	for i, want := range wantKeys {
		if rows[i].KeyID != want {
			t.Fatalf("row %d key id = %q, want %q", i, rows[i].KeyID, want)
		}
	}
	if got := keys.touchCount("key-1"); got != 5 {
		t.Fatalf("touches = %d, want 5", got)
	}
	if got := keys.touchCount("key-2"); got != 1 {
		t.Fatalf("touches for key-2 = %d, want 1", got)
	}
}

func bool2int(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------- CORS

func originRequest(method, path, origin string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	return req
}

func TestCorsIsClosedByDefault(t *testing.T) {
	srv, calls := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"ok":true}`))
	reg := newFakeRegistry().add(
		pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "plug-token")
	g := newTestGateway(t, Options{Plugins: reg})

	rr := serve(g, originRequest(http.MethodOptions, "/v1/chat/completions", "http://app.test"))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("preflight status = %d, want 405 (body %s)", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want no grant", got)
	}
	if got := rr.Header().Get("Allow"); got != "GET, POST" {
		t.Fatalf("Allow = %q", got)
	}
	payload := decodeError(t, rr)
	if payload.Error.Code != "cors_not_allowed" || payload.Error.Type != "invalid_request_error" {
		t.Fatalf("error = %+v", payload.Error)
	}
	if calls.count() != 0 {
		t.Fatal("preflight must not be proxied")
	}

	getReq := newGetRequest(t, "/v1/models", clientKey)
	getReq.Header.Set("Origin", "http://app.test")
	rr2 := serve(g, getReq)
	if rr2.Code != http.StatusOK {
		t.Fatalf("GET status = %d", rr2.Code)
	}
	if got := rr2.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("GET granted a non-listed origin: %q", got)
	}
}

func TestCorsAllowedOrigin(t *testing.T) {
	srv, calls := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, chatJSONReply)
	})
	reg := newFakeRegistry().add(
		pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "plug-token")
	g := newTestGateway(t, Options{Plugins: reg})
	g.SetCorsOrigins([]string{"http://app.test"})

	preflight := originRequest(http.MethodOptions, "/v1/responses", "http://app.test")
	preflight.Header.Set("Access-Control-Request-Method", "POST")
	rr := serve(g, preflight)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", rr.Code)
	}
	h := rr.Header()
	if got := h.Get("Access-Control-Allow-Origin"); got != "http://app.test" {
		t.Fatalf("Allow-Origin = %q", got)
	}
	if got := h.Get("Access-Control-Allow-Methods"); !strings.Contains(got, "POST") {
		t.Fatalf("Allow-Methods = %q", got)
	}
	for _, want := range []string{"Authorization", "X-API-Key", "api-key", "X-Conversation-Id"} {
		if got := h.Get("Access-Control-Allow-Headers"); !strings.Contains(got, want) {
			t.Fatalf("Allow-Headers = %q, want it to list %s", got, want)
		}
	}
	exposed := h.Get("Access-Control-Expose-Headers")
	for _, want := range []string{downgradedHeader, sourceHeader, "X-Aiworkgate-Credits"} {
		if !strings.Contains(exposed, want) {
			t.Fatalf("Expose-Headers = %q, want it to list %s", exposed, want)
		}
	}
	if h.Get("Access-Control-Max-Age") == "" {
		t.Fatal("Max-Age missing")
	}
	if got := h.Get("Vary"); !strings.Contains(got, "Origin") {
		t.Fatalf("Vary = %q", got)
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("204 carries a body: %q", rr.Body.String())
	}
	if calls.count() != 0 {
		t.Fatal("preflight must not be proxied")
	}

	getReq := newGetRequest(t, "/v1/models", clientKey)
	getReq.Header.Set("Origin", "http://app.test")
	if got := serve(g, getReq).Header().Get("Access-Control-Allow-Origin"); got != "http://app.test" {
		t.Fatalf("GET Allow-Origin = %q", got)
	}

	post := newChatRequest(t, "/v1/completions", clientKey, `{"model":"p/chat","prompt":"hi"}`)
	post.Header.Set("Origin", "http://app.test")
	rrPost := serve(g, post)
	if rrPost.Code != http.StatusOK {
		t.Fatalf("POST status = %d (body %s)", rrPost.Code, rrPost.Body.String())
	}
	if got := rrPost.Header().Get("Access-Control-Allow-Origin"); got != "http://app.test" {
		t.Fatalf("POST Allow-Origin = %q", got)
	}
	if got := rrPost.Header().Get(sourceHeader); got != "plugin:p" {
		t.Fatalf("POST source header = %q", got)
	}
	if body := decodeEnvelope(t, rrPost); body["object"] != "text_completion" {
		t.Fatalf("POST body = %v", body)
	}

	// An origin outside the list keeps the closed behaviour.
	other := originRequest(http.MethodOptions, "/v1/chat/completions", "http://evil.test")
	if rr2 := serve(g, other); rr2.Code != http.StatusMethodNotAllowed ||
		rr2.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("non-listed preflight = %d %q", rr2.Code, rr2.Header().Get("Access-Control-Allow-Origin"))
	}

	// Clearing the list closes the gateway again.
	g.SetCorsOrigins(nil)
	if rr3 := serve(g, originRequest(http.MethodOptions, "/v1/chat/completions", "http://app.test")); rr3.Code != http.StatusMethodNotAllowed {
		t.Fatalf("cleared preflight status = %d, want 405", rr3.Code)
	}
}

func TestCorsWildcardOrigin(t *testing.T) {
	g := newTestGateway(t, Options{Plugins: newFakeRegistry()})
	g.SetCorsOrigins([]string{" * "})

	for _, origin := range []string{"http://anywhere.test", "https://elsewhere.test"} {
		rr := serve(g, originRequest(http.MethodOptions, "/v1/chat/completions", origin))
		if rr.Code != http.StatusNoContent {
			t.Fatalf("%s status = %d, want 204", origin, rr.Code)
		}
		if got := rr.Header().Get("Access-Control-Allow-Origin"); got != origin {
			t.Fatalf("Allow-Origin = %q, want the request origin %q echoed", got, origin)
		}
	}

	// A request without an Origin header is a non-browser call: no grant.
	bare := serve(g, originRequest(http.MethodOptions, "/v1/chat/completions", ""))
	if bare.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405 without an Origin header", bare.Code)
	}
	if got := bare.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Allow-Origin = %q", got)
	}
}

func TestCorsOriginNormalisation(t *testing.T) {
	g := newTestGateway(t, Options{Plugins: newFakeRegistry()})
	g.SetCorsOrigins([]string{"  https://Other.Example  "})

	for _, origin := range []string{"https://other.example", "https://Other.Example/"} {
		rr := serve(g, originRequest(http.MethodOptions, "/v1/chat/completions", origin))
		if rr.Code != http.StatusNoContent {
			t.Fatalf("%s status = %d, want 204", origin, rr.Code)
		}
		if got := rr.Header().Get("Access-Control-Allow-Origin"); got != origin {
			t.Fatalf("Allow-Origin = %q, want the request origin echoed", got)
		}
	}

	for _, origin := range []string{"http://other.example", "https://other.example:8443", "https://evil.test"} {
		rr := serve(g, originRequest(http.MethodOptions, "/v1/chat/completions", origin))
		if rr.Code != http.StatusMethodNotAllowed || rr.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("%s was granted: %d %q", origin, rr.Code, rr.Header().Get("Access-Control-Allow-Origin"))
		}
	}
}

func TestCorsGrantSurvivesAuthFailure(t *testing.T) {
	g := newTestGateway(t, Options{Plugins: newFakeRegistry()})
	g.SetCorsOrigins([]string{"http://app.test"})

	req := newChatRequest(t, "/v1/chat/completions", "", `{"model":"p/chat","messages":[]}`)
	req.Header.Set("Origin", "http://app.test")
	rr := serve(g, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "http://app.test" {
		t.Fatalf("Allow-Origin = %q, the browser needs it on errors too", got)
	}
}

func TestChatBodyFromResponsesPreviousResponseID(t *testing.T) {
	// A pointer plus the full input is what Codex-style clients send; the
	// gateway is stateless, so the pointer is ignored rather than rejected.
	body := []byte(`{"model":"m","previous_response_id":"resp_1",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	chat, err := chatBodyFromResponses(body)
	if err != nil {
		t.Fatalf("previous_response_id with full input: %v", err)
	}
	var parsed struct {
		Messages []map[string]any `json:"messages"`
	}
	decodeInto(t, chat, &parsed)
	if len(parsed.Messages) != 1 {
		t.Fatalf("messages = %+v, want the input message only", parsed.Messages)
	}

	// The pointer alone cannot be resolved without server-side state.
	_, err = chatBodyFromResponses([]byte(`{"model":"m","previous_response_id":"resp_1"}`))
	if err == nil {
		t.Fatal("previous_response_id without input must be rejected")
	}
	if !strings.Contains(err.Error(), "无状态") {
		t.Fatalf("error %q does not explain the stateless gateway", err.Error())
	}
}

func TestEnvelopeWriterResponsesEmptyStreamFails(t *testing.T) {
	rec := feedEnvelope(t, envelopeResponses, http.StatusOK, "text/event-stream", true, []string{
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
		`data: [DONE]` + "\n\n",
	})
	body := rec.Body.String()
	events := parseSSE(t, strings.TrimSuffix(body, "data: [DONE]\n\n"))

	var sawFailed, sawCompleted bool
	for _, ev := range events {
		switch ev.event {
		case "response.failed":
			sawFailed = true
			var payload struct {
				Response struct {
					Status string         `json:"status"`
					Error  map[string]any `json:"error"`
				} `json:"response"`
			}
			decodeInto(t, []byte(ev.data), &payload)
			if payload.Response.Status != "failed" || payload.Response.Error["code"] != "empty_response" {
				t.Fatalf("response.failed payload = %s", ev.data)
			}
		case "response.completed":
			sawCompleted = true
		}
	}
	if !sawFailed {
		t.Fatalf("no response.failed event for an empty stream: %q", body)
	}
	if sawCompleted {
		t.Fatalf("an empty stream must not report response.completed: %q", body)
	}
}

func TestEnvelopeWriterCompletionsEmptyStreamFails(t *testing.T) {
	rec := feedEnvelope(t, envelopeCompletions, http.StatusOK, "text/event-stream", true, []string{
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}` + "\n\n",
		`data: [DONE]` + "\n\n",
	})
	body := rec.Body.String()
	events := parseSSE(t, strings.TrimSuffix(body, "data: [DONE]\n\n"))
	if len(events) != 1 {
		t.Fatalf("events = %d, want one error frame (%q)", len(events), body)
	}
	var payload struct {
		Error map[string]any `json:"error"`
		Usage map[string]any `json:"usage"`
	}
	decodeInto(t, []byte(events[0].data), &payload)
	if payload.Error == nil || payload.Error["code"] != "empty_response" {
		t.Fatalf("frame = %s, want an empty_response error", events[0].data)
	}
	if payload.Usage != nil {
		t.Fatalf("an empty stream must not report usage: %s", events[0].data)
	}
}
