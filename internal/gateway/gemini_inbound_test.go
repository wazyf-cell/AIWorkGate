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
