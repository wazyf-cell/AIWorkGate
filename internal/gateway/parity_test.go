package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aiworkgate/internal/model"
)

// ------------------------------------------------------------------ item 1
// Mid-stream upstream error frames (A: sse.rs:1055-1077, wb_sse.rs:87).

func TestAnthropicWriterUpstreamErrorFrame(t *testing.T) {
	cases := []struct {
		name     string
		frame    string
		wantType string
	}{
		{
			name:     "data error object",
			frame:    `data: {"error":{"message":"upstream exploded","type":"rate_limit_exceeded","code":"rate_limit"}}` + "\n\n",
			wantType: "rate_limit_error",
		},
		{
			name:     "event error",
			frame:    "event: error\ndata: {\"message\":\"upstream exploded\",\"code\":\"server_error\"}\n\n",
			wantType: "api_error",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := feedAnthropic(t, true, http.StatusOK, "text/event-stream", []string{
				`data: {"choices":[{"index":0,"delta":{"content":"partial"}}]}` + "\n\n",
				tc.frame,
				"data: [DONE]\n\n",
			})
			body := rec.Body.String()
			if strings.Contains(body, "message_delta") || strings.Contains(body, "message_stop") {
				t.Fatalf("an error frame must not finish the message: %q", body)
			}
			events := parseSSE(t, body)
			want := []string{
				"message_start", "content_block_start", "content_block_delta",
				"content_block_stop", "error",
			}
			if got := eventNames(events); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("events = %v, want %v", got, want)
			}
			if payload := decodeEvent(t, events[3]); payload["index"] != float64(0) {
				t.Fatalf("the open block must be closed before the error: %v", payload)
			}
			payload := decodeEvent(t, events[len(events)-1])
			if payload["type"] != "error" {
				t.Fatalf("last event = %v", payload)
			}
			errBody := payload["error"].(map[string]any)
			if errBody["type"] != tc.wantType || errBody["message"] != "upstream exploded" {
				t.Fatalf("error body = %v, want type %s", errBody, tc.wantType)
			}
		})
	}
}

func TestEnvelopeWriterResponsesUpstreamErrorFrame(t *testing.T) {
	rec := feedEnvelope(t, envelopeResponses, http.StatusOK, "text/event-stream", true, []string{
		`data: {"choices":[{"index":0,"delta":{"content":"partial"}}]}` + "\n\n",
		`data: {"error":{"message":"upstream exploded","type":"server_error"}}` + "\n\n",
		"data: [DONE]\n\n",
	})
	body := rec.Body.String()
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("no terminator may follow an error frame: %q", body)
	}
	if strings.Contains(body, "response.completed") {
		t.Fatalf("an error frame must not report completion: %q", body)
	}
	events := parseSSE(t, body)
	names := eventNames(events)
	if names[len(names)-1] != "response.failed" {
		t.Fatalf("events = %v, want a final response.failed", names)
	}
	payload := decodeEvent(t, events[len(events)-1])
	if payload["type"] != "response.failed" {
		t.Fatalf("payload = %v", payload)
	}
	response := payload["response"].(map[string]any)
	if response["status"] != "failed" {
		t.Fatalf("status = %v, want failed", response["status"])
	}
	errBody := response["error"].(map[string]any)
	if errBody["code"] != "upstream_error" || errBody["message"] != "upstream exploded" {
		t.Fatalf("error = %v", errBody)
	}
}

func TestEnvelopeWriterCompletionsUpstreamErrorFrame(t *testing.T) {
	rec := feedEnvelope(t, envelopeCompletions, http.StatusOK, "text/event-stream", true, []string{
		`data: {"choices":[{"index":0,"delta":{"content":"partial"}}]}` + "\n\n",
		"event: error\ndata: {\"message\":\"upstream exploded\",\"type\":\"server_error\"}\n\n",
		"data: [DONE]\n\n",
	})
	body := rec.Body.String()
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("no terminator may follow an error frame: %q", body)
	}
	events := parseSSE(t, body)
	last := events[len(events)-1]
	var payload map[string]any
	decodeInto(t, []byte(last.data), &payload)
	errBody, ok := payload["error"].(map[string]any)
	if !ok {
		t.Fatalf("last frame = %s, want an error frame", last.data)
	}
	if errBody["message"] != "upstream exploded" || errBody["type"] != "server_error" ||
		errBody["code"] == "" {
		t.Fatalf("error = %v", errBody)
	}
}

// ------------------------------------------------------------------ item 2
// Anthropic cache-token usage (A: sse.rs:826-829, 922-927).

func TestAnthropicUsageCarriesCachedTokens(t *testing.T) {
	const cachedUsage = `{"prompt_tokens":20,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":12}}`

	t.Run("non stream body", func(t *testing.T) {
		var reply chatReply
		decodeInto(t, []byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],`+
			`"usage":`+cachedUsage+`}`), &reply)
		body := anthropicMessageBody(reply, "p/chat", "msg_x")
		usage := body["usage"].(map[string]any)
		if usage["cache_read_input_tokens"] != 12 {
			t.Fatalf("cache_read_input_tokens = %v, want 12", usage["cache_read_input_tokens"])
		}
		if usage["cache_creation_input_tokens"] != 0 {
			t.Fatalf("cache_creation_input_tokens = %v, want 0 when unknown", usage["cache_creation_input_tokens"])
		}
		if usage["input_tokens"] != 20 || usage["output_tokens"] != 5 {
			t.Fatalf("usage = %v", usage)
		}
	})

	t.Run("stream message_start and message_delta", func(t *testing.T) {
		// Usage arriving before the first delta is carried by message_start.
		rec := feedAnthropic(t, true, http.StatusOK, "text/event-stream", []string{
			`data: {"choices":[],"usage":` + cachedUsage + `}` + "\n\n",
			`data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n",
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":` + cachedUsage + `}` + "\n\n",
			"data: [DONE]\n\n",
		})
		events := parseSSE(t, rec.Body.String())
		start := decodeEvent(t, events[0])
		startUsage := start["message"].(map[string]any)["usage"].(map[string]any)
		if startUsage["cache_read_input_tokens"] != float64(12) {
			t.Fatalf("message_start usage = %v, want cached 12", startUsage)
		}
		last := decodeEvent(t, events[len(events)-2])
		if last["type"] != "message_delta" {
			t.Fatalf("event before message_stop = %v", last)
		}
		deltaUsage := last["usage"].(map[string]any)
		if deltaUsage["cache_read_input_tokens"] != float64(12) {
			t.Fatalf("message_delta usage = %v, want cached 12", deltaUsage)
		}
		if deltaUsage["cache_creation_input_tokens"] != float64(0) {
			t.Fatalf("cache_creation_input_tokens = %v, want 0", deltaUsage["cache_creation_input_tokens"])
		}
	})
}

// ------------------------------------------------------------------ item 3
// SSE keep-alive during silence (A: wb_route.rs:619-635, F-34).

func TestSSEKeepAliveDuringSilence(t *testing.T) {
	previous := sseKeepAliveInterval
	sseKeepAliveInterval = 50 * time.Millisecond
	defer func() { sseKeepAliveInterval = previous }()

	const (
		first  = "data: {\"choices\":[{\"delta\":{\"content\":\"one\"}}]}\n\n"
		second = "data: {\"choices\":[{\"delta\":{\"content\":\"two\"}}]}\n\n"
		done   = "data: [DONE]\n\n"
	)
	srv, _ := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, first)
		w.(http.Flusher).Flush()
		select {
		case <-time.After(300 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, second+done)
		w.(http.Flusher).Flush()
	})
	reg := newFakeRegistry().add(
		pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "plug-token")
	g := newTestGateway(t, Options{Plugins: reg, Log: &fakeLogSink{}})
	gw := httptest.NewServer(g)
	defer gw.Close()

	cases := []struct {
		name       string
		path       string
		body       string
		wantPrefix string
		wantSuffix string
	}{
		{"passthrough chat stream", "/v1/chat/completions",
			`{"model":"p/chat","stream":true,"messages":[]}`, first, second + done},
		{"converted responses stream", "/v1/responses",
			`{"model":"p/chat","stream":true,"input":"hi"}`,
			"event: response.created\n", "data: [DONE]\n\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, gw.URL+tc.path, strings.NewReader(tc.body))
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			req.Header.Set("Authorization", "Bearer "+clientKey)
			req.Header.Set("Content-Type", "application/json")
			client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer resp.Body.Close()
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read stream: %v", err)
			}
			body := string(raw)
			if !strings.Contains(body, ": keep-alive\n\n") {
				t.Fatalf("no keep-alive during a silent stream: %q", body)
			}
			// The comment sits between whole frames: the first event is forwarded
			// untouched, the real events survive, and every comment starts a line.
			if !strings.HasPrefix(body, tc.wantPrefix) {
				t.Fatalf("first event was altered: %q", body)
			}
			if !strings.HasSuffix(body, tc.wantSuffix) {
				t.Fatalf("stream tail was altered: %q", body)
			}
			for rest := body; ; {
				idx := strings.Index(rest, ": keep-alive")
				if idx < 0 {
					break
				}
				if idx != 0 && !strings.HasSuffix(body[:len(body)-len(rest)+idx], "\n\n") {
					t.Fatalf("keep-alive was spliced into an event: %q", body)
				}
				rest = rest[idx+len(": keep-alive"):]
			}
		})
	}
}

// ------------------------------------------------------------------ item 4
// Consecutive same-role message merge (A: wb_payload.rs:14, 597-724).

func TestChatBodyFromAnthropicMergesSameRole(t *testing.T) {
	chat, _, err := chatBodyFromAnthropic([]byte(`{"model":"p/chat","messages":[` +
		`{"role":"user","content":"first"},` +
		`{"role":"user","content":[{"type":"text","text":"second"},{"type":"text","text":"third"}]},` +
		`{"role":"assistant","content":"answer"},` +
		`{"role":"assistant","content":"more"}]}`))
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	req := decodeChatRequest(t, chat)
	if len(req.Messages) != 2 {
		t.Fatalf("messages = %+v, want two merged turns", req.Messages)
	}
	if req.Messages[0].Role != "user" || req.Messages[0].Content != "first\n\nsecondthird" {
		t.Fatalf("merged user = %+v", req.Messages[0])
	}
	if req.Messages[1].Role != "assistant" || req.Messages[1].Content != "answer\n\nmore" {
		t.Fatalf("merged assistant = %+v", req.Messages[1])
	}

	// A tool exchange in between must keep the turns apart.
	chat, _, err = chatBodyFromAnthropic([]byte(`{"model":"p/chat","messages":[` +
		`{"role":"user","content":"a"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"f","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]},` +
		`{"role":"user","content":"b"}]}`))
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	req = decodeChatRequest(t, chat)
	if len(req.Messages) != 4 {
		t.Fatalf("tool turns must not merge: %+v", req.Messages)
	}
}

func TestChatBodyFromResponsesMergesSameRoleText(t *testing.T) {
	body := `{"model":"p/chat","input":[` +
		`{"type":"message","role":"user","content":"first"},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"second"}]}]}`
	chat, err := chatBodyFromResponses([]byte(body))
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	req := decodeChatRequest(t, chat)
	if len(req.Messages) != 1 {
		t.Fatalf("messages = %+v, want one merged user turn", req.Messages)
	}
	if req.Messages[0].Content != "first\n\nsecond" {
		t.Fatalf("merged content = %q", req.Messages[0].Content)
	}

	// Adjacent user turns around a function call stay separate.
	body = `{"model":"p/chat","input":[` +
		`{"type":"message","role":"user","content":"a"},` +
		`{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"c1","output":"r"},` +
		`{"type":"message","role":"user","content":"b"}]}`
	chat, err = chatBodyFromResponses([]byte(body))
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	req = decodeChatRequest(t, chat)
	if len(req.Messages) != 4 {
		t.Fatalf("tool turns must not merge: %+v", req.Messages)
	}
}

// ------------------------------------------------------------------ item 5
// Image content passthrough (A: payload.rs:467 drops images; we pass them on).

func TestAnthropicImageBlocksBecomeImageURLParts(t *testing.T) {
	chat, images, err := chatBodyFromAnthropic([]byte(`{"model":"p/chat","messages":[{"role":"user","content":[` +
		`{"type":"text","text":"what is this"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"QUJD"}},` +
		`{"type":"image","source":{"type":"url","url":"https://example.com/x.png"}}]}]}`))
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if images != 0 {
		t.Fatalf("dropped images = %d, want 0 (they are passed through)", images)
	}
	if !strings.Contains(string(chat), `"type":"image_url"`) {
		t.Fatalf("converted body has no image_url part: %s", chat)
	}
	var req struct {
		Messages []struct {
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	decodeInto(t, chat, &req)
	parts := req.Messages[0].Content
	if len(parts) != 3 {
		t.Fatalf("content parts = %v", parts)
	}
	if parts[0]["type"] != "text" || parts[0]["text"] != "what is this" {
		t.Fatalf("text part = %v", parts[0])
	}
	first := parts[1]["image_url"].(map[string]any)["url"]
	if first != "data:image/jpeg;base64,QUJD" {
		t.Fatalf("base64 image url = %v", first)
	}
	second := parts[2]["image_url"].(map[string]any)["url"]
	if second != "https://example.com/x.png" {
		t.Fatalf("url image = %v", second)
	}
}

func TestResponsesInputImageBecomesImageURLPart(t *testing.T) {
	chat, err := chatBodyFromResponses([]byte(`{"model":"p/chat","input":[` +
		`{"type":"message","role":"user","content":[` +
		`{"type":"input_text","text":"look"},` +
		`{"type":"input_image","image_url":"data:image/png;base64,QUJD"}]}]}`))
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if !strings.Contains(string(chat), `"type":"image_url"`) {
		t.Fatalf("converted body has no image_url part: %s", chat)
	}
	var req struct {
		Messages []struct {
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	decodeInto(t, chat, &req)
	parts := req.Messages[0].Content
	if len(parts) != 2 || parts[1]["type"] != "image_url" {
		t.Fatalf("content parts = %v", parts)
	}
	if url := parts[1]["image_url"].(map[string]any)["url"]; url != "data:image/png;base64,QUJD" {
		t.Fatalf("image url = %v", url)
	}

	// A shape we cannot express keeps the placeholder instead of vanishing.
	chat, err = chatBodyFromResponses([]byte(`{"model":"p/chat","input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_image","file_id":"file_1"}]}]}`))
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if !strings.Contains(string(chat), "[image_url]") {
		t.Fatalf("unsupported image shape lost its placeholder: %s", chat)
	}
}

// ------------------------------------------------------------------ item 6
// Responses envelope fidelity (A: wb_responses.rs:519).

func TestResponseBodyMarksLengthAsIncomplete(t *testing.T) {
	var reply chatReply
	decodeInto(t, []byte(`{"id":"chatcmpl-1","choices":[{"finish_reason":"length",`+
		`"message":{"role":"assistant","content":"cut"}}],`+
		`"usage":{"prompt_tokens":20,"completion_tokens":5,"total_tokens":25,`+
		`"prompt_tokens_details":{"cached_tokens":12},`+
		`"completion_tokens_details":{"reasoning_tokens":3}}}`), &reply)
	body := responseBody(reply, "p/chat", 1)

	if body["status"] != "incomplete" {
		t.Fatalf("status = %v, want incomplete", body["status"])
	}
	incomplete, ok := body["incomplete_details"].(map[string]any)
	if !ok || incomplete["reason"] != "max_output_tokens" {
		t.Fatalf("incomplete_details = %v", body["incomplete_details"])
	}
	usage := body["usage"].(map[string]any)
	if usage["input_tokens"] != 20 || usage["output_tokens"] != 5 || usage["total_tokens"] != 25 {
		t.Fatalf("flat usage fields changed: %v", usage)
	}
	inputDetails, ok := usage["input_tokens_details"].(map[string]any)
	if !ok || inputDetails["cached_tokens"] != 12 {
		t.Fatalf("input_tokens_details = %v", usage["input_tokens_details"])
	}
	outputDetails, ok := usage["output_tokens_details"].(map[string]any)
	if !ok || outputDetails["reasoning_tokens"] != 3 {
		t.Fatalf("output_tokens_details = %v", usage["output_tokens_details"])
	}
}

func TestEnvelopeWriterResponsesLengthStreamIsIncomplete(t *testing.T) {
	rec := feedEnvelope(t, envelopeResponses, http.StatusOK, "text/event-stream", true, []string{
		`data: {"choices":[{"index":0,"delta":{"content":"cut"}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"length"}],` +
			`"usage":{"prompt_tokens":20,"completion_tokens":5,` +
			`"prompt_tokens_details":{"cached_tokens":12},` +
			`"completion_tokens_details":{"reasoning_tokens":3}}}` + "\n\n",
		"data: [DONE]\n\n",
	})
	body := rec.Body.String()
	events := parseSSE(t, strings.TrimSuffix(body, "data: [DONE]\n\n"))
	last := events[len(events)-1]
	if last.event != "response.incomplete" {
		t.Fatalf("last event = %q, want response.incomplete (%q)", last.event, body)
	}
	payload := decodeEvent(t, last)
	if payload["type"] != "response.incomplete" {
		t.Fatalf("payload type = %v", payload["type"])
	}
	response := payload["response"].(map[string]any)
	if response["status"] != "incomplete" {
		t.Fatalf("status = %v, want incomplete", response["status"])
	}
	incomplete, ok := response["incomplete_details"].(map[string]any)
	if !ok || incomplete["reason"] != "max_output_tokens" {
		t.Fatalf("incomplete_details = %v", response["incomplete_details"])
	}
	usage := response["usage"].(map[string]any)
	if got := usage["input_tokens_details"].(map[string]any)["cached_tokens"]; got != float64(12) {
		t.Fatalf("cached_tokens = %v", got)
	}
	if got := usage["output_tokens_details"].(map[string]any)["reasoning_tokens"]; got != float64(3) {
		t.Fatalf("reasoning_tokens = %v", got)
	}
}

// The incomplete path must not change the shape of an ordinary answer.
func TestResponseBodyKeepsCompletedForStop(t *testing.T) {
	var reply chatReply
	decodeInto(t, []byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`), &reply)
	body := responseBody(reply, "p/chat", 1)
	if body["status"] != "completed" {
		t.Fatalf("status = %v", body["status"])
	}
	if _, ok := body["incomplete_details"]; ok {
		t.Fatalf("completed response carries incomplete_details: %v", body)
	}
	raw, err := json.Marshal(body)
	if err != nil || !json.Valid(raw) {
		t.Fatalf("response body is not valid JSON: %v", err)
	}
}
