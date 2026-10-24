package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"aiworkgate/internal/model"
)

const customUpstreamKey = "sk-custom-upstream"

// customFixture returns an Options that routes "my-model" to one httptest
// upstream with a stored key.
func customFixture(t *testing.T, models []CustomModel, key string, extra Options) Options {
	t.Helper()
	extra.CustomModels = func() ([]CustomModel, error) {
		return append([]CustomModel(nil), models...), nil
	}
	extra.CustomModelKey = func(id string) (string, bool) {
		for _, cm := range models {
			if cm.ID == id {
				return key, key != ""
			}
		}
		return "", false
	}
	return extra
}

type usageCall struct {
	keyID      string
	requests   int
	prompt     int
	completion int
	cached     int
}

type usageRecorder struct {
	mu    sync.Mutex
	calls []usageCall
}

func (u *usageRecorder) record(keyID, day string, requests, prompt, completion, cached int, credits *float64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls = append(u.calls, usageCall{keyID: keyID, requests: requests, prompt: prompt, completion: completion, cached: cached})
}

func (u *usageRecorder) all() []usageCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]usageCall(nil), u.calls...)
}

func TestCustomModelChatURL(t *testing.T) {
	cases := map[string]string{
		"https://api.example.com":     "https://api.example.com/v1/chat/completions",
		"https://api.example.com/":    "https://api.example.com/v1/chat/completions",
		"https://api.example.com/v1":  "https://api.example.com/v1/chat/completions",
		"https://api.example.com/v1/": "https://api.example.com/v1/chat/completions",
	}
	for base, want := range cases {
		if got := CustomChatURL(base); got != want {
			t.Fatalf("CustomChatURL(%q) = %q, want %q", base, got, want)
		}
	}
}

func TestCustomModelNonStreamUsage(t *testing.T) {
	const upstreamBody = `{"id":"c1","model":"vendor-model","choices":[{"message":{"content":"hi"}}],` +
		`"usage":{"prompt_tokens":11,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":2}}}`
	srv, calls := newUpstreamServer(t, jsonResponder(http.StatusOK, upstreamBody))

	logs := &fakeLogSink{}
	usage := &usageRecorder{}
	g := newTestGateway(t, customFixture(t, []CustomModel{{
		ID: "cm-1", Name: "my-model", BaseURL: srv.URL + "/v1", UpstreamModel: "vendor-model", Enabled: true,
	}}, customUpstreamKey, Options{Plugins: newFakeRegistry(), Log: logs, KeyUsage: usage.record}))

	body := `{"model":"my-model","messages":[{"role":"user","content":"hi"}],"temperature":0.3}`
	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get(sourceHeader); got != "custom:cm-1" {
		t.Fatalf("source header = %q, want custom:cm-1", got)
	}
	var payload struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response JSON: %v (body %s)", err, rr.Body.String())
	}
	if payload.Model != "my-model" {
		t.Fatalf("response model = %q, want the client-requested my-model", payload.Model)
	}

	got := calls.last(t)
	if got.Path != "/v1/chat/completions" {
		t.Fatalf("upstream path = %q, want /v1/chat/completions", got.Path)
	}
	if got.Authorization != "Bearer "+customUpstreamKey {
		t.Fatalf("upstream Authorization = %q, want the stored key", got.Authorization)
	}
	if got.Authorization == "Bearer "+clientKey {
		t.Fatal("the AIWorkGate client key must never be forwarded upstream")
	}
	if got.ContentType != "application/json" {
		t.Fatalf("upstream Content-Type = %q", got.ContentType)
	}
	var forwarded struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(got.Body, &forwarded); err != nil || forwarded.Model != "vendor-model" {
		t.Fatalf("upstream body model = %q (err %v), want vendor-model", forwarded.Model, err)
	}

	rows := logs.wait(t, 1)
	row := rows[0]
	if row.Source != "custom:cm-1" || row.Model != "my-model" || row.Status != http.StatusOK {
		t.Fatalf("log row = %+v, want source custom:cm-1 model my-model status 200", row)
	}
	if row.PromptTokens != 11 || row.CompletionTokens != 5 || row.CachedTokens != 2 {
		t.Fatalf("log tokens = %d/%d/%d, want 11/5/2", row.PromptTokens, row.CompletionTokens, row.CachedTokens)
	}
	if row.Credits != nil {
		t.Fatalf("credits = %v, want nil for custom models", *row.Credits)
	}

	recorded := usage.all()
	if len(recorded) != 1 {
		t.Fatalf("KeyUsage calls = %d, want exactly 1", len(recorded))
	}
	if recorded[0].requests != 1 || recorded[0].prompt != 11 || recorded[0].completion != 5 || recorded[0].cached != 2 {
		t.Fatalf("usage = %+v, want one request with 11/5/2 tokens", recorded[0])
	}
}

func TestCustomModelStreamPassthroughRewritesModel(t *testing.T) {
	srv, calls := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		frames := []string{
			`data: {"id":"1","model":"vendor-model","choices":[{"delta":{"content":"he"}}]}` + "\n\n",
			`: keep-alive` + "\n\n",
			`data: {"id":"1","model":"vendor-model","choices":[{"delta":{}}],"usage":{"prompt_tokens":7,"completion_tokens":3}}` + "\n\n",
			`data: [DONE]` + "\n\n",
		}
		for _, frame := range frames {
			_, _ = io.WriteString(w, frame)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	})

	logs := &fakeLogSink{}
	usage := &usageRecorder{}
	g := newTestGateway(t, customFixture(t, []CustomModel{{
		ID: "cm-1", Name: "my-model", BaseURL: srv.URL, UpstreamModel: "vendor-model", Enabled: true,
	}}, customUpstreamKey, Options{Plugins: newFakeRegistry(), Log: logs, KeyUsage: usage.record}))

	body := `{"model":"my-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	out := rr.Body.String()
	if strings.Contains(out, "vendor-model") {
		t.Fatalf("stream still echoes the upstream model: %q", out)
	}
	if strings.Count(out, `"model":"my-model"`) != 2 {
		t.Fatalf("rewritten frames = %d, want 2 (body %q)", strings.Count(out, `"model":"my-model"`), out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("[DONE] missing from stream: %q", out)
	}
	if !strings.Contains(out, ": keep-alive") {
		t.Fatalf("comment frame was not preserved: %q", out)
	}

	calls.last(t)
	rows := logs.wait(t, 1)
	if rows[0].PromptTokens != 7 || rows[0].CompletionTokens != 3 || !rows[0].Stream {
		t.Fatalf("stream usage = %+v, want 7/3 tokens and stream=true", rows[0])
	}
	recorded := usage.all()
	if len(recorded) != 1 || recorded[0].prompt != 7 || recorded[0].completion != 3 {
		t.Fatalf("KeyUsage = %+v, want one 7/3 call", recorded)
	}
}

func TestCustomModelUpstreamErrorRedactsKey(t *testing.T) {
	srv, _ := newUpstreamServer(t, jsonResponder(http.StatusUnauthorized,
		fmt.Sprintf(`{"error":{"message":"bad key %s"}}`, customUpstreamKey)))

	logs := &fakeLogSink{}
	g := newTestGateway(t, customFixture(t, []CustomModel{{
		ID: "cm-1", Name: "my-model", BaseURL: srv.URL, UpstreamModel: "vendor-model", Enabled: true,
	}}, customUpstreamKey, Options{Plugins: newFakeRegistry(), Log: logs}))

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"my-model","messages":[{"role":"user","content":"hi"}]}`))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want the upstream 401", rr.Code)
	}
	if strings.Contains(rr.Body.String(), customUpstreamKey) {
		t.Fatalf("response leaks the upstream key: %s", rr.Body.String())
	}
	payload := decodeError(t, rr)
	if payload.Error.Code != "custom_upstream_error" || payload.Error.Type != "upstream_error" {
		t.Fatalf("error body = %+v, want custom_upstream_error", payload.Error)
	}
	if !strings.Contains(payload.Error.Message, "***") {
		t.Fatalf("message = %q, want the redacted upstream text", payload.Error.Message)
	}

	rows := logs.wait(t, 1)
	if strings.Contains(rows[0].Error, customUpstreamKey) {
		t.Fatalf("log row leaks the upstream key: %q", rows[0].Error)
	}
}

func TestCustomModelDisabledFallsThrough(t *testing.T) {
	customSrv, customCalls := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"model":"vendor-model"}`))
	aSrv, aCalls := newUpstreamServer(t, jsonResponder(http.StatusNotFound,
		`{"error":{"message":"unknown model","type":"invalid_request_error","code":"model_not_found"}}`))

	g := newTestGateway(t, customFixture(t, []CustomModel{{
		ID: "cm-1", Name: "my-model", BaseURL: customSrv.URL, UpstreamModel: "vendor-model", Enabled: false,
	}}, customUpstreamKey, Options{Plugins: newFakeRegistry(), A: &fakeA{base: aSrv.URL}}))

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"my-model","messages":[{"role":"user","content":"hi"}]}`))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want the normal A 404 (body %s)", rr.Code, rr.Body.String())
	}
	if customCalls.count() != 0 {
		t.Fatalf("disabled custom upstream calls = %d, want 0", customCalls.count())
	}
	if aCalls.count() != 1 {
		t.Fatalf("A calls = %d, want 1", aCalls.count())
	}
}

// ------------------------------------------------------- converted protocols
//
// The custom upstream is reached through the same converters the plugin path
// uses (chatSender seam). These tests pin the translated Messages / Responses /
// completions protocols, the stored-key semantics and the single usage count.

const customChatReply = `{"id":"chatcmpl-77","model":"vendor-model",` +
	`"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"Hello from vendor"}}],` +
	`"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,` +
	`"prompt_tokens_details":{"cached_tokens":3},"cache_creation_input_tokens":2}}`

// customModelGateway wires one custom model "my-model" to srvURL.
func customModelGateway(t *testing.T, srvURL string, logs *fakeLogSink, usage *usageRecorder) *Gateway {
	t.Helper()
	return newTestGateway(t, customFixture(t, []CustomModel{{
		ID: "cm-1", Name: "my-model", BaseURL: srvURL, UpstreamModel: "vendor-model", Enabled: true,
	}}, customUpstreamKey, Options{Plugins: newFakeRegistry(), Log: logs, KeyUsage: usage.record}))
}

// sseResponder writes the frames as one text/event-stream reply.
func sseResponder(frames []string) func(http.ResponseWriter, *http.Request, []byte) {
	return func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, frame := range frames {
			_, _ = io.WriteString(w, frame)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	}
}

func TestCustomModelMessagesNonStream(t *testing.T) {
	srv, calls := newUpstreamServer(t, jsonResponder(http.StatusOK, customChatReply))
	logs := &fakeLogSink{}
	usage := &usageRecorder{}
	g := customModelGateway(t, srv.URL+"/v1", logs, usage)

	rr := serve(g, newChatRequest(t, "/v1/messages", clientKey,
		`{"model":"my-model","max_tokens":64,"system":"be brief","messages":[{"role":"user","content":"hi"}]}`))
	if rr.Code == http.StatusNotImplemented {
		t.Fatalf("/v1/messages is still unsupported: %s", rr.Body.String())
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get(sourceHeader); got != "custom:cm-1" {
		t.Fatalf("source header = %q, want custom:cm-1", got)
	}

	up := calls.last(t)
	if up.Path != "/v1/chat/completions" || up.Authorization != "Bearer "+customUpstreamKey {
		t.Fatalf("upstream request = %s auth %q", up.Path, up.Authorization)
	}
	if up.Authorization == "Bearer "+clientKey {
		t.Fatal("the AIWorkGate client key must never be forwarded upstream")
	}
	chat := decodeChatRequest(t, up.Body)
	if chat.Model != "vendor-model" {
		t.Fatalf("upstream model = %q, want the physical vendor-model", chat.Model)
	}
	if len(chat.Messages) != 2 || chat.Messages[0].Role != "system" || chat.Messages[1].Role != "user" {
		t.Fatalf("converted messages = %+v", chat.Messages)
	}
	if got := numberField(t, chat.top, "max_tokens"); got != 64 {
		t.Fatalf("max_tokens = %v, want 64", got)
	}

	var body struct {
		Type    string `json:"type"`
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	decodeInto(t, rr.Body.Bytes(), &body)
	if body.Type != "message" || body.Model != "my-model" || body.StopReason != "end_turn" {
		t.Fatalf("Messages reply = %+v", body)
	}
	if len(body.Content) != 1 || body.Content[0].Type != "text" || body.Content[0].Text != "Hello from vendor" {
		t.Fatalf("content = %+v", body.Content)
	}
	// Cache counters survive the conversion in the Anthropic shape.
	if body.Usage.InputTokens != 11 || body.Usage.OutputTokens != 7 ||
		body.Usage.CacheReadInputTokens != 3 || body.Usage.CacheCreationInputTokens != 2 {
		t.Fatalf("usage = %+v, want 11/7 with 3 cached and 2 created", body.Usage)
	}

	row := logs.wait(t, 1)[0]
	if row.Source != "custom:cm-1" || row.Model != "my-model" || row.Status != http.StatusOK {
		t.Fatalf("log row = %+v", row)
	}
	if row.PromptTokens != 11 || row.CompletionTokens != 7 || row.CachedTokens != 3 {
		t.Fatalf("log tokens = %d/%d/%d, want 11/7/3", row.PromptTokens, row.CompletionTokens, row.CachedTokens)
	}
	recorded := usage.all()
	if len(recorded) != 1 || recorded[0].requests != 1 || recorded[0].prompt != 11 || recorded[0].cached != 3 {
		t.Fatalf("KeyUsage = %+v, want exactly one 11/7/3 record", recorded)
	}
}

func TestCustomModelMessagesStream(t *testing.T) {
	srv, _ := newUpstreamServer(t, sseResponder(streamFrames(true)))
	logs := &fakeLogSink{}
	usage := &usageRecorder{}
	g := customModelGateway(t, srv.URL, logs, usage)

	rr := serve(g, newChatRequest(t, "/v1/messages", clientKey,
		`{"model":"my-model","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	if strings.Contains(rr.Body.String(), "[DONE]") {
		t.Fatal("the chat terminator leaked into the Anthropic stream")
	}
	events := parseSSE(t, rr.Body.String())
	names := eventNames(events)
	if len(names) == 0 || names[0] != "message_start" || names[len(names)-1] != "message_stop" {
		t.Fatalf("event sequence = %v, want message_start ... message_stop", names)
	}
	started := decodeEvent(t, events[0])
	message := started["message"].(map[string]any)
	if started["type"] != "message_start" || message["model"] != "my-model" {
		t.Fatalf("message_start = %v", started)
	}
	counts := map[string]int{}
	for _, ev := range events {
		counts[ev.event]++
	}
	if counts["content_block_start"] == 0 || counts["content_block_start"] != counts["content_block_stop"] {
		t.Fatalf("content blocks are unbalanced: %v", names)
	}
	delta, ok := eventByType(t, events, "message_delta")
	if !ok {
		t.Fatalf("no message_delta in %v", names)
	}
	usagePayload := delta["usage"].(map[string]any)
	if usagePayload["input_tokens"] != float64(11) || usagePayload["output_tokens"] != float64(7) {
		t.Fatalf("message_delta usage = %v", usagePayload)
	}

	row := logs.wait(t, 1)[0]
	if !row.Stream || row.PromptTokens != 11 || row.CompletionTokens != 7 {
		t.Fatalf("log row = %+v, want one streamed 11/7 record", row)
	}
	if recorded := usage.all(); len(recorded) != 1 || recorded[0].prompt != 11 || recorded[0].completion != 7 {
		t.Fatalf("KeyUsage = %+v, want exactly one record", recorded)
	}
}

// eventByType returns the payload of the first event with the given type field.
func eventByType(t *testing.T, events []sseEvent, typ string) (map[string]any, bool) {
	t.Helper()
	for _, ev := range events {
		payload := decodeEvent(t, ev)
		if payload["type"] == typ {
			return payload, true
		}
	}
	return nil, false
}

func TestCustomModelMessagesStreamUpstreamError(t *testing.T) {
	frames := []string{
		`data: {"choices":[{"index":0,"delta":{"content":"partial"}}]}` + "\n\n",
		`data: {"error":{"message":"vendor exploded","type":"overloaded_error"}}` + "\n\n",
	}
	srv, _ := newUpstreamServer(t, sseResponder(frames))
	g := customModelGateway(t, srv.URL, &fakeLogSink{}, &usageRecorder{})

	rr := serve(g, newChatRequest(t, "/v1/messages", clientKey,
		`{"model":"my-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	events := parseSSE(t, rr.Body.String())
	last := events[len(events)-1]
	if last.event != "error" {
		t.Fatalf("final event = %q, want error (%v)", last.event, eventNames(events))
	}
	payload := decodeEvent(t, last)
	if payload["type"] != "error" {
		t.Fatalf("error payload = %v", payload)
	}
	body := payload["error"].(map[string]any)
	if body["type"] != "overloaded_error" || body["message"] != "vendor exploded" {
		t.Fatalf("error body = %v", body)
	}
	if strings.Contains(rr.Body.String(), "message_stop") {
		t.Fatal("a failed stream must not pretend to finish")
	}
}

func TestCustomModelResponsesNonStream(t *testing.T) {
	srv, calls := newUpstreamServer(t, jsonResponder(http.StatusOK, chatJSONReply))
	logs := &fakeLogSink{}
	usage := &usageRecorder{}
	g := customModelGateway(t, srv.URL, logs, usage)

	rr := serve(g, newChatRequest(t, "/v1/responses", clientKey,
		`{"model":"my-model","instructions":"be brief","input":"hello"}`))
	if rr.Code == http.StatusNotImplemented {
		t.Fatalf("/v1/responses is still unsupported: %s", rr.Body.String())
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	up := calls.last(t)
	chat := decodeChatRequest(t, up.Body)
	if chat.Model != "vendor-model" || up.Authorization != "Bearer "+customUpstreamKey {
		t.Fatalf("upstream model = %q auth %q", chat.Model, up.Authorization)
	}
	if len(chat.Messages) != 2 || chat.Messages[0].Role != "system" || chat.Messages[1].Content != "hello" {
		t.Fatalf("converted input = %+v", chat.Messages)
	}

	var body struct {
		Object     string `json:"object"`
		Model      string `json:"model"`
		Status     string `json:"status"`
		OutputText string `json:"output_text"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}
	decodeInto(t, rr.Body.Bytes(), &body)
	if body.Object != "response" || body.Model != "my-model" || body.Status != "completed" {
		t.Fatalf("Responses envelope = %+v", body)
	}
	if body.OutputText != "Hello world" {
		t.Fatalf("output_text = %q", body.OutputText)
	}
	if body.Usage.InputTokens != 11 || body.Usage.OutputTokens != 7 || body.Usage.TotalTokens != 18 {
		t.Fatalf("usage = %+v", body.Usage)
	}

	row := logs.wait(t, 1)[0]
	if row.Source != "custom:cm-1" || row.Model != "my-model" || row.PromptTokens != 11 {
		t.Fatalf("log row = %+v", row)
	}
	if recorded := usage.all(); len(recorded) != 1 || recorded[0].prompt != 11 || recorded[0].completion != 7 {
		t.Fatalf("KeyUsage = %+v, want exactly one record", recorded)
	}
}

func TestCustomModelResponsesIncompleteStatus(t *testing.T) {
	const reply = `{"id":"chatcmpl-9","model":"vendor-model","choices":[{"index":0,"finish_reason":"length",` +
		`"message":{"role":"assistant","content":"trunc"}}],` +
		`"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7,` +
		`"prompt_tokens_details":{"cached_tokens":1},"completion_tokens_details":{"reasoning_tokens":2}}}`
	srv, _ := newUpstreamServer(t, jsonResponder(http.StatusOK, reply))
	g := customModelGateway(t, srv.URL, &fakeLogSink{}, &usageRecorder{})

	rr := serve(g, newChatRequest(t, "/v1/responses", clientKey, `{"model":"my-model","input":"hi"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Status            string `json:"status"`
		IncompleteDetails struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Usage struct {
			InputTokens        int `json:"input_tokens"`
			OutputTokens       int `json:"output_tokens"`
			TotalTokens        int `json:"total_tokens"`
			InputTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
			OutputTokensDetails struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	}
	decodeInto(t, rr.Body.Bytes(), &body)
	// A length stop stays "incomplete" instead of claiming a completed answer.
	if body.Status != "incomplete" || body.IncompleteDetails.Reason != "max_output_tokens" {
		t.Fatalf("status = %q details = %+v", body.Status, body.IncompleteDetails)
	}
	if body.Usage.TotalTokens != 7 || body.Usage.InputTokensDetails.CachedTokens != 1 ||
		body.Usage.OutputTokensDetails.ReasoningTokens != 2 {
		t.Fatalf("usage details = %+v", body.Usage)
	}
}

func TestCustomModelResponsesStream(t *testing.T) {
	srv, _ := newUpstreamServer(t, sseResponder(streamFrames(false)))
	logs := &fakeLogSink{}
	usage := &usageRecorder{}
	g := customModelGateway(t, srv.URL, logs, usage)

	rr := serve(g, newChatRequest(t, "/v1/responses", clientKey,
		`{"model":"my-model","input":"hi","stream":true}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}
	out := rr.Body.String()
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("stream does not end with [DONE]: %q", out)
	}
	if strings.Contains(out, "vendor-model") {
		t.Fatalf("stream still echoes the upstream model: %q", out)
	}
	events := parseSSE(t, strings.TrimSuffix(out, "data: [DONE]\n\n"))
	names := eventNames(events)
	if len(names) == 0 || names[0] != "response.created" || names[len(names)-1] != "response.completed" {
		t.Fatalf("sequence = %v", names)
	}
	created := decodeEvent(t, events[0])
	if created["response"].(map[string]any)["model"] != "my-model" {
		t.Fatalf("response.created model = %v", created["response"])
	}
	final := decodeEvent(t, events[len(events)-1])
	response := final["response"].(map[string]any)
	if response["model"] != "my-model" || response["status"] != "completed" {
		t.Fatalf("final response = %v", response)
	}
	if response["output_text"] != "Hello world" {
		t.Fatalf("output_text = %v", response["output_text"])
	}
	if usagePayload, ok := response["usage"].(map[string]any); !ok ||
		usagePayload["input_tokens"] != float64(11) || usagePayload["output_tokens"] != float64(7) {
		t.Fatalf("usage = %v", response["usage"])
	}

	row := logs.wait(t, 1)[0]
	if !row.Stream || row.PromptTokens != 11 || row.CompletionTokens != 7 {
		t.Fatalf("log row = %+v", row)
	}
	if recorded := usage.all(); len(recorded) != 1 || recorded[0].completion != 7 {
		t.Fatalf("KeyUsage = %+v, want exactly one record", recorded)
	}
}

func TestCustomModelResponsesStreamUpstreamError(t *testing.T) {
	frames := []string{
		`data: {"choices":[{"index":0,"delta":{"content":"partial"}}]}` + "\n\n",
		`data: {"error":{"message":"vendor exploded","type":"server_error"}}` + "\n\n",
	}
	srv, _ := newUpstreamServer(t, sseResponder(frames))
	g := customModelGateway(t, srv.URL, &fakeLogSink{}, &usageRecorder{})

	rr := serve(g, newChatRequest(t, "/v1/responses", clientKey,
		`{"model":"my-model","input":"hi","stream":true}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	out := rr.Body.String()
	if strings.Contains(out, "data: [DONE]") {
		t.Fatalf("a failed response must not end with [DONE]: %q", out)
	}
	events := parseSSE(t, out)
	last := events[len(events)-1]
	if last.event != "response.failed" {
		t.Fatalf("final event = %q, want response.failed (%v)", last.event, eventNames(events))
	}
	payload := decodeEvent(t, last)
	response := payload["response"].(map[string]any)
	if payload["type"] != "response.failed" || response["model"] != "my-model" || response["status"] != "failed" {
		t.Fatalf("response.failed = %v", payload)
	}
	errBody := response["error"].(map[string]any)
	if errBody["code"] != "upstream_error" || errBody["message"] != "vendor exploded" {
		t.Fatalf("error = %v", errBody)
	}
}

func TestCustomModelCompletionsRoute(t *testing.T) {
	srv, calls := newUpstreamServer(t, jsonResponder(http.StatusOK, chatJSONReply))
	g := customModelGateway(t, srv.URL, &fakeLogSink{}, &usageRecorder{})

	rr := serve(g, newChatRequest(t, "/v1/completions", clientKey,
		`{"model":"my-model","prompt":"hello"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	chat := decodeChatRequest(t, calls.last(t).Body)
	if chat.Model != "vendor-model" || len(chat.Messages) != 1 || chat.Messages[0].Content != "hello" {
		t.Fatalf("forwarded body = %s", calls.last(t).Body)
	}
	var body struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Text string `json:"text"`
		} `json:"choices"`
	}
	decodeInto(t, rr.Body.Bytes(), &body)
	if body.Object != "text_completion" || body.Model != "my-model" {
		t.Fatalf("completions envelope = %+v", body)
	}
	if len(body.Choices) != 1 || body.Choices[0].Text != "Hello world" {
		t.Fatalf("choices = %+v", body.Choices)
	}
}

func TestCustomModelMessagesErrorRedactsKey(t *testing.T) {
	srv, calls := newUpstreamServer(t, jsonResponder(http.StatusUnauthorized,
		fmt.Sprintf(`{"error":{"message":"bad key %s"}}`, customUpstreamKey)))
	logs := &fakeLogSink{}
	g := customModelGateway(t, srv.URL, logs, &usageRecorder{})

	rr := serve(g, newChatRequest(t, "/v1/messages", clientKey,
		`{"model":"my-model","messages":[{"role":"user","content":"hi"}]}`))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want the upstream 401 (body %s)", rr.Code, rr.Body.String())
	}
	if up := calls.last(t); up.Authorization != "Bearer "+customUpstreamKey {
		t.Fatalf("upstream Authorization = %q, want only the stored key", up.Authorization)
	}
	payload := decodeAnthropicError(t, rr)
	if payload.Error.Type != "authentication_error" {
		t.Fatalf("error type = %q, want authentication_error", payload.Error.Type)
	}
	if strings.Contains(rr.Body.String(), customUpstreamKey) {
		t.Fatalf("response leaks the upstream key: %s", rr.Body.String())
	}
	if !strings.Contains(payload.Error.Message, "***") {
		t.Fatalf("message = %q, want the redacted upstream text", payload.Error.Message)
	}
	row := logs.wait(t, 1)[0]
	if row.Source != "custom:cm-1" || strings.Contains(row.Error, customUpstreamKey) {
		t.Fatalf("log row leaks the upstream key: %+v", row)
	}
}

func TestCustomModelListMerge(t *testing.T) {
	reg := newFakeRegistry().add(
		pluginStatus("p", true, model.ModelInfo{ID: "chat"}), "http://127.0.0.1:1", "plug-token")
	fa := &fakeA{base: "http://127.0.0.1:1", models: []model.ModelInfo{
		{ID: "gpt-4o"}, {ID: "other-model"},
	}}
	g := newTestGateway(t, customFixture(t, []CustomModel{
		{ID: "cm-1", Name: "my-model", BaseURL: "https://a.example.com/v1", UpstreamModel: "v1", Enabled: true},
		{ID: "cm-2", Name: "GPT-4o", BaseURL: "https://b.example.com/v1", UpstreamModel: "v2", Enabled: true},
		{ID: "cm-3", Name: "off-model", BaseURL: "https://c.example.com/v1", UpstreamModel: "v3", Enabled: false},
	}, customUpstreamKey, Options{Plugins: reg, A: fa}))

	rr := serve(g, newGetRequest(t, "/v1/models", clientKey))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	var list modelsList
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode model list: %v (body %s)", err, rr.Body.String())
	}
	byID := make(map[string]modelEntry, len(list.Data))
	order := make([]string, 0, len(list.Data))
	for _, entry := range list.Data {
		byID[entry.ID] = entry
		order = append(order, entry.ID)
	}
	if _, ok := byID["p/chat"]; !ok {
		t.Fatalf("plugin model missing: %v", order)
	}
	custom, ok := byID["my-model"]
	if !ok || custom.Vendor != "custom" || custom.Name != "my-model" {
		t.Fatalf("custom entry = %+v (ok %v), want vendor custom", custom, ok)
	}
	// The custom gpt-4o replaces A's bare entry in place (custom wins over A).
	if got := byID["GPT-4o"]; got.Vendor != "custom" {
		t.Fatalf("GPT-4o entry = %+v, want the custom entry", got)
	}
	if strings.Count(strings.Join(order, ","), "GPT-4o") != 1 {
		t.Fatalf("GPT-4o appears more than once: %v", order)
	}
	if _, ok := byID["off-model"]; ok {
		t.Fatalf("disabled custom model was published: %v", order)
	}
	posCustom, posOther := -1, -1
	for i, id := range order {
		switch id {
		case "my-model":
			posCustom = i
		case "other-model":
			posOther = i
		}
	}
	if posCustom < 0 || posOther < 0 || posCustom > posOther {
		t.Fatalf("custom entries should precede A entries: %v", order)
	}

	// The conflict helper sees a plugin id collision (defensive: names cannot
	// contain "/", but the admin flag relies on this check).
	conflicts := g.CustomModelConflicts(context.Background(), []string{"P/CHAT", "my-model"})
	if !conflicts["p/chat"] {
		t.Fatalf("conflicts = %v, want p/chat flagged", conflicts)
	}
	if conflicts["my-model"] {
		t.Fatalf("my-model must not be flagged: %v", conflicts)
	}
}

func TestCustomModelListRespectsKeyScope(t *testing.T) {
	reg := newFakeRegistry().add(
		pluginStatus("p", true, model.ModelInfo{ID: "chat"}), "http://127.0.0.1:1", "plug-token")
	opts := customFixture(t, []CustomModel{
		{ID: "cm-1", Name: "my-model", BaseURL: "https://a.example.com/v1", UpstreamModel: "v1", Enabled: true},
		{ID: "cm-2", Name: "other-model", BaseURL: "https://b.example.com/v1", UpstreamModel: "v2", Enabled: true},
	}, customUpstreamKey, Options{
		Plugins: reg,
		KeyRecord: func(keyID string) (KeyRecord, bool, error) {
			return KeyRecord{ID: keyID, Enabled: true, ModelScope: []string{"my-model"}}, true, nil
		},
	})
	g := newTestGateway(t, opts)

	rr := serve(g, newGetRequest(t, "/v1/models", clientKey))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	var list modelsList
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode model list: %v", err)
	}
	if len(list.Data) != 1 || list.Data[0].ID != "my-model" {
		t.Fatalf("scoped list = %+v, want only my-model", list.Data)
	}
}
