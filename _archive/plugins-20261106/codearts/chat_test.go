package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func chatRequestBody(t *testing.T, model string, stream bool, extra map[string]any) string {
	t.Helper()
	m := map[string]any{
		"model":    model,
		"messages": []map[string]any{{"role": "user", "content": "hello"}},
		"stream":   stream,
	}
	for k, v := range extra {
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal chat body: %v", err)
	}
	return string(b)
}

func sseServer(t *testing.T, chunks ...string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, c := range chunks {
			_, _ = fmt.Fprint(w, c)
		}
	}))
}

func sseLine(v any) string {
	b, _ := json.Marshal(v)
	return "data: " + string(b) + "\n\n"
}

func upstreamChunk(delta map[string]any, finish any, usage map[string]any) string {
	m := map[string]any{
		"id": "up-1",
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finish,
		}},
	}
	if usage != nil {
		m["usage"] = usage
	}
	return sseLine(m)
}

func toolCallDelta(index int, id, name, args string) map[string]any {
	fn := map[string]any{}
	if name != "" {
		fn["name"] = name
	}
	fn["arguments"] = args
	call := map[string]any{"index": index, "function": fn}
	if id != "" {
		call["id"] = id
		call["type"] = "function"
	}
	return call
}

type clientChunk struct {
	Choices []struct {
		Delta struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage       map[string]any `json:"usage"`
	ErrorObject *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

type parsedSSE struct {
	content   strings.Builder
	reasoning strings.Builder
	tools     map[int]*struct {
		id   string
		name string
		args strings.Builder
	}
	finish string
	done   bool
	usage  map[string]any
	errors []string
}

func parseClientSSE(t *testing.T, body string) parsedSSE {
	t.Helper()
	out := parsedSSE{tools: map[int]*struct {
		id   string
		name string
		args strings.Builder
	}{}}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			out.done = true
			continue
		}
		var c clientChunk
		if err := json.Unmarshal([]byte(payload), &c); err != nil {
			t.Fatalf("bad SSE payload %q: %v", payload, err)
		}
		if c.ErrorObject != nil {
			out.errors = append(out.errors, c.ErrorObject.Message)
			continue
		}
		if c.Usage != nil {
			out.usage = c.Usage
		}
		for _, ch := range c.Choices {
			out.content.WriteString(ch.Delta.Content)
			out.reasoning.WriteString(ch.Delta.ReasoningContent)
			if ch.FinishReason != nil && *ch.FinishReason != "" {
				out.finish = *ch.FinishReason
			}
			for _, tc := range ch.Delta.ToolCalls {
				call, ok := out.tools[tc.Index]
				if !ok {
					call = &struct {
						id   string
						name string
						args strings.Builder
					}{}
					out.tools[tc.Index] = call
				}
				if tc.ID != "" {
					call.id = tc.ID
				}
				if tc.Function.Name != "" {
					call.name = tc.Function.Name
				}
				call.args.WriteString(tc.Function.Arguments)
			}
		}
	}
	return out
}

func TestChatStreamPassthrough(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	var headers []http.Header
	chunks := []string{
		upstreamChunk(map[string]any{"role": "assistant", "content": "Hello "}, nil, nil),
		upstreamChunk(map[string]any{"content": "world"}, nil, nil),
		upstreamChunk(map[string]any{"reasoning_content": "thinking"}, nil, nil),
		upstreamChunk(map[string]any{"tool_calls": []any{toolCallDelta(0, "call_1", "get_weather", `{"city":`)}}, nil, nil),
		upstreamChunk(map[string]any{"tool_calls": []any{toolCallDelta(0, "", "", `"Paris"}`)}}, nil, nil),
		upstreamChunk(map[string]any{}, "tool_calls", map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}),
		"data: [DONE]\n\n",
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		headers = append(headers, r.Header.Clone())
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			_, _ = fmt.Fprint(w, c)
		}
	}))
	defer upstream.Close()

	ep := testEndpoints()
	ep.Chat = upstream.URL
	var acct Account
	p, h := newTestPlugin(t, func(o *Options) { o.Endpoints = ep })
	acct = seedAccount(t, p, "alice")

	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{body: chatRequestBody(t, "GLM-5.2", true, nil)})
	if rec.Code != http.StatusOK {
		t.Fatalf("chat status = %d, body %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	got := parseClientSSE(t, rec.Body.String())
	if !got.done {
		t.Fatalf("missing [DONE]: %s", rec.Body.String())
	}
	if got.content.String() != "Hello world" {
		t.Fatalf("content = %q", got.content.String())
	}
	if got.reasoning.String() != "thinking" {
		t.Fatalf("reasoning = %q", got.reasoning.String())
	}
	if len(got.tools) != 1 {
		t.Fatalf("tool calls = %+v", got.tools)
	}
	call := got.tools[0]
	if call.name != "get_weather" || call.args.String() != `{"city":"Paris"}` || call.id != "call_1" {
		t.Fatalf("tool call = %+v args=%q", call, call.args.String())
	}
	if got.finish != "tool_calls" {
		t.Fatalf("finish = %q", got.finish)
	}
	if got.usage == nil || got.usage["total_tokens"].(float64) != 15 {
		t.Fatalf("usage = %+v", got.usage)
	}

	mu.Lock()
	body := bodies[0]
	hdr := headers[0]
	mu.Unlock()
	var up upstreamBody
	if err := json.Unmarshal([]byte(body), &up); err != nil {
		t.Fatalf("upstream body: %v", err)
	}
	if !up.Stream || up.PromptCacheKey != acct.Identity.SessionID || up.MaxTokens != 65536 || !up.ToolStream {
		t.Fatalf("upstream body = %+v", up)
	}
	if len(up.Include) != 1 || up.Include[0] != "reasoning.encrypted_content" || up.ReasoningSummary != "auto" {
		t.Fatalf("upstream include/summary = %+v", up)
	}
	if hdr.Get("Session-Id") != acct.Identity.SessionID || hdr.Get("Chat-Id") != acct.Identity.ChatID || hdr.Get("lang") != "en" {
		t.Fatalf("upstream headers = %v", hdr)
	}
	if !strings.HasPrefix(hdr.Get("Authorization"), "SDK-HMAC-SHA256 Access=AK-alice,") {
		t.Fatalf("authorization = %q", hdr.Get("Authorization"))
	}
}

func TestChatNonStreamAggregates(t *testing.T) {
	upstream := sseServer(t,
		sseLine(map[string]any{"id": "u", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "Hello "}}}}),
		sseLine(map[string]any{"id": "u", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "there", "reasoning_content": "why"}}}}),
		sseLine(map[string]any{"id": "u", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}}),
		"data: [DONE]\n\n",
	)
	defer upstream.Close()
	ep := testEndpoints()
	ep.Chat = upstream.URL
	p, h := newTestPlugin(t, func(o *Options) { o.Endpoints = ep })
	seedAccount(t, p, "alice")

	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{body: chatRequestBody(t, "GLM-5.2", false, nil)})
	if rec.Code != http.StatusOK {
		t.Fatalf("chat status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role             string `json:"role"`
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Object != "chat.completion" || resp.Model != "GLM-5.2" || len(resp.Choices) != 1 {
		t.Fatalf("response = %+v", resp)
	}
	if resp.Choices[0].Message.Content != "Hello there" || resp.Choices[0].Message.ReasoningContent != "why" || resp.Choices[0].FinishReason != "stop" {
		t.Fatalf("choice = %+v", resp.Choices[0])
	}
	if resp.Usage == nil || resp.Usage["total_tokens"].(float64) != 5 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
}

func TestChatStreamErrorBeforeOutputIsHTTPError(t *testing.T) {
	upstream := sseServer(t, sseLine(map[string]any{"text": "[DONE]", "error_code": "InferHub.4001", "error_msg": "bad thing"}))
	defer upstream.Close()
	ep := testEndpoints()
	ep.Chat = upstream.URL
	p, h := newTestPlugin(t, func(o *Options) { o.Endpoints = ep })
	seedAccount(t, p, "alice")

	for _, stream := range []bool{false, true} {
		rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{body: chatRequestBody(t, "GLM-5.2", stream, nil)})
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("stream=%v status = %d, body %s", stream, rec.Code, rec.Body.String())
		}
		if env := decodeAs[errorEnvelope](t, rec); !strings.Contains(env.Error.Message, "bad thing") {
			t.Fatalf("stream=%v error = %+v", stream, env.Error)
		}
	}
}

func TestChatStreamErrorAfterOutputEmitsSSEError(t *testing.T) {
	upstream := sseServer(t,
		sseLine(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "partial"}}}}),
		sseLine(map[string]any{"text": "[DONE]", "error_code": "InferHub.ModelArts.9001", "error_msg": "upstream exploded"}),
	)
	defer upstream.Close()
	ep := testEndpoints()
	ep.Chat = upstream.URL
	p, h := newTestPlugin(t, func(o *Options) { o.Endpoints = ep })
	seedAccount(t, p, "alice")

	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{body: chatRequestBody(t, "GLM-5.2", true, nil)})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	got := parseClientSSE(t, rec.Body.String())
	if got.content.String() != "partial" {
		t.Fatalf("content = %q", got.content.String())
	}
	if len(got.errors) != 1 || !strings.Contains(got.errors[0], "upstream exploded") {
		t.Fatalf("errors = %v, body %s", got.errors, rec.Body.String())
	}
	if !got.done {
		t.Fatal("error stream must terminate with [DONE]")
	}
}

func TestChatDSMLModeInjectsPromptAndParsesTools(t *testing.T) {
	var body upstreamBody
	var raw map[string]any
	dsml := "<thought>plan</thought>Hi " +
		"<\uFF5CDSML\uFF5Ctool_calls><\uFF5CDSML\uFF5Cinvoke name=\"get_weather\">" +
		"<\uFF5CDSML\uFF5Cparameter name=\"city\" string=\"true\">Paris</\uFF5CDSML\uFF5Cparameter>" +
		"</\uFF5CDSML\uFF5Cinvoke></\uFF5CDSML\uFF5Ctool_calls>"
	chunks := make([]string, 0, 8)
	runes := []rune(dsml)
	for i := 0; i < len(runes); i += 9 {
		end := i + 9
		if end > len(runes) {
			end = len(runes)
		}
		chunks = append(chunks, sseLine(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": string(runes[i:end])}}}}))
	}
	chunks = append(chunks, sseLine(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}}), "data: [DONE]\n\n")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawBytes, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(rawBytes, &body)
		_ = json.Unmarshal(rawBytes, &raw)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			_, _ = fmt.Fprint(w, c)
		}
	}))
	defer upstream.Close()
	ep := testEndpoints()
	ep.Chat = upstream.URL
	p, h := newTestPlugin(t, func(o *Options) { o.Endpoints = ep })
	seedAccount(t, p, "alice")

	tools := []map[string]any{{
		"type": "function",
		"function": map[string]any{
			"name":        "get_weather",
			"description": "weather",
			"parameters":  map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
		},
	}}
	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{body: chatRequestBody(t, "deepseek-v4-flash", false, map[string]any{"tools": tools})})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if _, hasTools := raw["tools"]; hasTools {
		t.Fatal("DSML mode must omit the native tools array")
	}
	hasDSML := false
	for _, m := range body.Messages {
		if s, _ := m["content"].(string); strings.Contains(s, "DSML") && m["role"] == "system" {
			hasDSML = true
		}
	}
	if !hasDSML {
		t.Fatalf("DSML system prompt missing: %+v", body.Messages)
	}

	var resp struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				Reasoning string `json:"reasoning_content"`
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
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	msg := resp.Choices[0].Message
	if strings.TrimSpace(msg.Content) != "Hi" {
		t.Fatalf("content = %q", msg.Content)
	}
	if msg.Reasoning != "plan" {
		t.Fatalf("reasoning = %q", msg.Reasoning)
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Function.Name != "get_weather" {
		t.Fatalf("tool calls = %+v", msg.ToolCalls)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(msg.ToolCalls[0].Function.Arguments), &args); err != nil || args["city"] != "Paris" {
		t.Fatalf("arguments = %q", msg.ToolCalls[0].Function.Arguments)
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish = %q", resp.Choices[0].FinishReason)
	}
}

func TestChatRotatesOnceOnRateLimit(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		hit := hits
		mu.Unlock()
		if strings.Contains(r.Header.Get("Authorization"), "Access=AK-alice,") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error_code":"InferHub.ModelArts.81111.429","error_msg":"TPM exceeded"}`))
			return
		}
		if hit == 0 {
			t.Errorf("first request did not start with alice")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, sseLine(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "from bob"}}}}))
		_, _ = fmt.Fprint(w, sseLine(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}}))
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	ep := testEndpoints()
	ep.Chat = upstream.URL
	p, h := newTestPlugin(t, func(o *Options) { o.Endpoints = ep })
	alice := seedAccount(t, p, "alice")
	seedAccount(t, p, "bob")

	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{body: chatRequestBody(t, "GLM-5.2", false, nil)})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "from bob" {
		t.Fatalf("response = %+v", resp)
	}
	mu.Lock()
	total := hits
	mu.Unlock()
	if total != 2 {
		t.Fatalf("hits = %d, want 2", total)
	}
	stored, _ := p.findAccount(alice.ID)
	if !stored.Quota.RateLimited || stored.Quota.ResetAt == 0 {
		t.Fatalf("alice rate limit not recorded: %+v", stored.Quota)
	}
}

func TestChatRateLimitWithoutSiblingReturnsUpstreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error_code":"4291","error_msg":"insufficient quota"}`))
	}))
	defer upstream.Close()
	ep := testEndpoints()
	ep.Chat = upstream.URL
	p, h := newTestPlugin(t, func(o *Options) { o.Endpoints = ep })
	seedAccount(t, p, "solo")

	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{body: chatRequestBody(t, "GLM-5.2", false, nil)})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429, body %s", rec.Code, rec.Body.String())
	}
	if env := decodeAs[errorEnvelope](t, rec); env.Error.Code != "quota_exceeded" || !strings.Contains(env.Error.Message, "insufficient quota") {
		t.Fatalf("error = %+v", env.Error)
	}
}

func TestChatQueueRetryThenSuccess(t *testing.T) {
	var mu sync.Mutex
	chatHits, queueHits := 0, 0
	chat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		chatHits++
		hit := chatHits
		mu.Unlock()
		if hit == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error_code":"TM.00001041","error_msg":"peak usage, try again after 10s"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, sseLine(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "queued ok"}}}}))
		_, _ = fmt.Fprint(w, sseLine(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}}))
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer chat.Close()
	queue := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queueHits++
		hit := queueHits
		mu.Unlock()
		status := "waiting"
		if hit > 1 {
			status = "working"
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": status, "queue_position": 3})
	}))
	defer queue.Close()
	ep := testEndpoints()
	ep.Chat = chat.URL
	ep.Queue = queue.URL
	p, h := newTestPlugin(t, func(o *Options) { o.Endpoints = ep })
	seedAccount(t, p, "alice")

	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{body: chatRequestBody(t, "GLM-5.2", false, nil)})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	ch, qh := chatHits, queueHits
	mu.Unlock()
	if ch != 2 || qh < 1 {
		t.Fatalf("chat hits = %d, queue hits = %d", ch, qh)
	}
}

func TestChatQueueTimeout(t *testing.T) {
	chat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error_code":"TM.00001041","error_msg":"queue full"}`))
	}))
	defer chat.Close()
	queue := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "waiting"})
	}))
	defer queue.Close()
	ep := testEndpoints()
	ep.Chat = chat.URL
	ep.Queue = queue.URL
	p, h := newTestPlugin(t, func(o *Options) {
		o.Endpoints = ep
		o.QueueMaxAttempts = 2
		o.QueueRetryDelay = 20 * time.Millisecond
	})
	seedAccount(t, p, "alice")

	start := time.Now()
	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{body: chatRequestBody(t, "GLM-5.2", false, nil)})
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504, body %s", rec.Code, rec.Body.String())
	}
	if env := decodeAs[errorEnvelope](t, rec); env.Error.Code != "queue_timeout" {
		t.Fatalf("error = %+v", env.Error)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("queue timeout took too long: %v", time.Since(start))
	}
}

func TestChatBenefitHeaderAndFallback(t *testing.T) {
	var mu sync.Mutex
	maasHeaders := []string{}
	authHeaders := []string{}
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		hit := hits
		maasHeaders = append(maasHeaders, r.Header.Get("maas_type"))
		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if hit == 1 {
			_, _ = fmt.Fprint(w, sseLine(map[string]any{"text": "[DONE]", "error_code": "InferHub.4004.200", "error_msg": "benefit not found"}))
			return
		}
		_, _ = fmt.Fprint(w, sseLine(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "ok"}}}}))
		_, _ = fmt.Fprint(w, sseLine(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}}))
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	ep := testEndpoints()
	ep.Chat = upstream.URL
	p, h := newTestPlugin(t, func(o *Options) { o.Endpoints = ep })
	seedAccount(t, p, "alice")

	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{body: chatRequestBody(t, "glm-5.3-flash", false, nil)})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	gotMaas := append([]string(nil), maasHeaders...)
	gotAuth := append([]string(nil), authHeaders...)
	mu.Unlock()
	if len(gotMaas) != 2 || gotMaas[0] != "benefit" || gotMaas[1] != "" {
		t.Fatalf("maas headers = %v", gotMaas)
	}
	if !strings.Contains(gotAuth[0], "maas_type") || strings.Contains(gotAuth[1], "maas_type") {
		t.Fatalf("signed headers after dropping benefit = %v", gotAuth)
	}
}
