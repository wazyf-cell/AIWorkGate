package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aiworkgate/internal/model"
)

func geminiRequest(t *testing.T, path, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", clientKey)
	return req
}

func TestGeminiGenerateContentInbound(t *testing.T) {
	srv, rec := newUpstreamServer(t, jsonResponder(http.StatusOK,
		`{"model":"gpt-4o","choices":[{"message":{"content":"hello"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`))
	reg := newFakeRegistry().add(pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "tok")
	g := newTestGateway(t, Options{Plugins: reg})

	rr := serve(g, geminiRequest(t, "/v1beta/models/p/chat:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],`+
			`"generationConfig":{"temperature":0.3,"maxOutputTokens":9}}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rr.Code, rr.Body.String())
	}
	var payload struct {
		Candidates []struct {
			Content struct {
				Parts []struct{ Text string } `json:"parts"`
				Role  string                  `json:"role"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		Usage struct {
			Prompt     int `json:"promptTokenCount"`
			Candidates int `json:"candidatesTokenCount"`
			Total      int `json:"totalTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v (%s)", err, rr.Body.String())
	}
	if len(payload.Candidates) != 1 || payload.Candidates[0].Content.Parts[0].Text != "hello" ||
		payload.Candidates[0].Content.Role != "model" || payload.Candidates[0].FinishReason != "STOP" {
		t.Fatalf("candidates = %+v", payload.Candidates)
	}
	if payload.Usage.Total != 7 || payload.Usage.Prompt != 5 || payload.Usage.Candidates != 2 {
		t.Fatalf("usage = %+v", payload.Usage)
	}

	up := rec.last(t)
	var chat map[string]any
	if err := json.Unmarshal(up.Body, &chat); err != nil {
		t.Fatalf("upstream body: %v", err)
	}
	if chat["model"] != "chat" || chat["temperature"] != 0.3 || chat["max_tokens"] != float64(9) {
		t.Fatalf("upstream chat body = %v", chat)
	}
}

func TestGeminiInlineImageMapsToImageURL(t *testing.T) {
	srv, rec := newUpstreamServer(t, jsonResponder(http.StatusOK,
		`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"total_tokens":1}}`))
	reg := newFakeRegistry().add(pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "tok")
	g := newTestGateway(t, Options{Plugins: reg})

	rr := serve(g, geminiRequest(t, "/v1beta/models/p/chat:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"what is this"},`+
			`{"inlineData":{"mimeType":"image/png","data":"QUJD"}}]}]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rr.Code, rr.Body.String())
	}
	up := rec.last(t)
	if !strings.Contains(string(up.Body), `data:image/png;base64,QUJD`) ||
		!strings.Contains(string(up.Body), `"image_url"`) {
		t.Fatalf("image part lost: %s", up.Body)
	}
}

func TestGeminiStreamInbound(t *testing.T) {
	srv, _ := newUpstreamServer(t, sseResponder([]string{
		`data: {"model":"gpt-4o","choices":[{"delta":{"content":"he"}}]}` + "\n\n",
		`data: {"choices":[{"delta":{"content":"llo"},"finish_reason":"stop"}]}` + "\n\n",
		`data: {"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}` + "\n\n",
		"data: [DONE]\n\n",
	}))
	reg := newFakeRegistry().add(pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "tok")
	g := newTestGateway(t, Options{Plugins: reg})

	rr := serve(g, geminiRequest(t, "/v1beta/models/p/chat:streamGenerateContent?alt=sse",
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	text := rr.Body.String()
	if !strings.Contains(text, `"text":"he"`) || !strings.Contains(text, `"text":"llo"`) {
		t.Fatalf("stream deltas lost: %s", text)
	}
	if !strings.Contains(text, `"finishReason":"STOP"`) || !strings.Contains(text, `"totalTokenCount":7`) {
		t.Fatalf("stream tail lost: %s", text)
	}
}

func TestGeminiErrorMapping(t *testing.T) {
	srv, _ := newUpstreamServer(t, jsonResponder(http.StatusTooManyRequests,
		`{"error":{"message":"rate limited","type":"rate_limit"}}`))
	reg := newFakeRegistry().add(pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "tok")
	g := newTestGateway(t, Options{Plugins: reg})

	rr := serve(g, geminiRequest(t, "/v1beta/models/p/chat:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d (%s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"status":"RESOURCE_EXHAUSTED"`) ||
		!strings.Contains(rr.Body.String(), "rate limited") {
		t.Fatalf("error body = %s", rr.Body.String())
	}
}

func TestGeminiRequestTranslatesToolsAndCalls(t *testing.T) {
	srv, rec := newUpstreamServer(t, jsonResponder(http.StatusOK,
		`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	reg := newFakeRegistry().add(pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "tok")
	g := newTestGateway(t, Options{Plugins: reg})

	rr := serve(g, geminiRequest(t, "/v1beta/models/p/chat:generateContent", `{
		"tools":[{"functionDeclarations":[{"name":"get_weather","description":"lookup",
			"parameters":{"type":"object","properties":{"city":{"type":"string"}}}}]}],
		"contents":[
			{"role":"user","parts":[{"text":"weather?"}]},
			{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"city":"Paris"}}}]},
			{"role":"user","parts":[{"functionResponse":{"name":"get_weather","response":{"temp":18}}}]}
		]
	}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rr.Code, rr.Body.String())
	}

	var body struct {
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name       string `json:"name"`
				Parameters struct {
					Properties map[string]any `json:"properties"`
				} `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
			Content    string `json:"content"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rec.last(t).Body, &body); err != nil {
		t.Fatalf("upstream body: %v", err)
	}
	if len(body.Tools) != 1 || body.Tools[0].Function.Name != "get_weather" || body.Tools[0].Function.Parameters.Properties["city"] == nil {
		t.Fatalf("tools = %+v", body.Tools)
	}
	if len(body.Messages) != 3 {
		t.Fatalf("messages = %+v", body.Messages)
	}
	call := body.Messages[1]
	if call.Role != "assistant" || len(call.ToolCalls) != 1 || call.ToolCalls[0].Function.Name != "get_weather" ||
		call.ToolCalls[0].Function.Arguments != `{"city":"Paris"}` || call.ToolCalls[0].ID == "" {
		t.Fatalf("assistant tool call = %+v", call)
	}
	result := body.Messages[2]
	if result.Role != "tool" || result.ToolCallID != call.ToolCalls[0].ID || result.Content != `{"temp":18}` {
		t.Fatalf("tool result = %+v", result)
	}
}

func TestGeminiNonStreamToolCallBackTranslated(t *testing.T) {
	srv, _ := newUpstreamServer(t, jsonResponder(http.StatusOK,
		`{"model":"m","choices":[{"message":{"content":"","tool_calls":[{"id":"call_1","type":"function",`+
			`"function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]},"finish_reason":"tool_calls"}],`+
			`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	reg := newFakeRegistry().add(pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "tok")
	g := newTestGateway(t, Options{Plugins: reg})

	rr := serve(g, geminiRequest(t, "/v1beta/models/p/chat:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"weather?"}]}]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rr.Code, rr.Body.String())
	}
	var payload struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text         string `json:"text"`
					FunctionCall *struct {
						Name string         `json:"name"`
						Args map[string]any `json:"args"`
					} `json:"functionCall"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v (%s)", err, rr.Body.String())
	}
	candidate := payload.Candidates[0]
	if candidate.FinishReason != "STOP" || len(candidate.Content.Parts) != 1 {
		t.Fatalf("candidate = %+v", candidate)
	}
	call := candidate.Content.Parts[0].FunctionCall
	if call == nil || call.Name != "get_weather" || call.Args["city"] != "Paris" {
		t.Fatalf("functionCall = %+v", candidate.Content.Parts[0])
	}
}

func TestGeminiStreamToolCallBackTranslated(t *testing.T) {
	srv, _ := newUpstreamServer(t, sseResponder([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"get_weather","arguments":"{\"city\":"}}]}}]}` + "\n\n",
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Paris\"}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n",
		`data: {"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n\n",
		"data: [DONE]\n\n",
	}))
	reg := newFakeRegistry().add(pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "tok")
	g := newTestGateway(t, Options{Plugins: reg})

	rr := serve(g, geminiRequest(t, "/v1beta/models/p/chat:streamGenerateContent?alt=sse",
		`{"contents":[{"role":"user","parts":[{"text":"weather?"}]}]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rr.Code, rr.Body.String())
	}
	text := rr.Body.String()
	if !strings.Contains(text, `"functionCall"`) || !strings.Contains(text, `"name":"get_weather"`) ||
		!strings.Contains(text, `"city":"Paris"`) {
		t.Fatalf("tool call missing from stream: %s", text)
	}
	if !strings.Contains(text, `"finishReason":"STOP"`) || !strings.Contains(text, `"totalTokenCount":2`) {
		t.Fatalf("stream tail lost: %s", text)
	}
}
