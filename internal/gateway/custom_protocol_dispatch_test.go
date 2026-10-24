package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCustomAnthropicUpstreamNonStream(t *testing.T) {
	const upstreamBody = `{"id":"msg_1","content":[{"type":"text","text":"hello"}],` +
		`"stop_reason":"end_turn","usage":{"input_tokens":4,"output_tokens":2}}`
	srv, calls := newUpstreamServer(t, jsonResponder(http.StatusOK, upstreamBody))
	cm := CustomModel{
		ID: "cm-a", Name: "claude-local", Protocol: "anthropic", BaseURL: srv.URL,
		UpstreamModel: "claude-x", Enabled: true, InputTypes: []string{"text", "image"},
	}
	g := newTestGateway(t, customFixture(t, []CustomModel{cm}, customUpstreamKey, Options{Plugins: newFakeRegistry()}))

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"claude-local","messages":[{"role":"user","content":"hi"}]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	var reply struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &reply); err != nil {
		t.Fatalf("decode reply: %v (body %s)", err, rr.Body.String())
	}
	if reply.Model != "claude-local" || len(reply.Choices) != 1 ||
		reply.Choices[0].Message.Content != "hello" || reply.Choices[0].FinishReason != "stop" {
		t.Fatalf("reply = %+v", reply)
	}

	got := calls.last(t)
	if got.Path != "/v1/messages" {
		t.Fatalf("upstream path = %q, want /v1/messages", got.Path)
	}
	if got.Header.Get("x-api-key") != customUpstreamKey {
		t.Fatalf("x-api-key = %q, want the stored key", got.Header.Get("x-api-key"))
	}
	if got.Header.Get("anthropic-version") == "" {
		t.Fatal("anthropic-version header missing")
	}
	if got.Authorization != "" {
		t.Fatalf("Authorization sent to Anthropic upstream: %q", got.Authorization)
	}
	var forwarded struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
	}
	if err := json.Unmarshal(got.Body, &forwarded); err != nil {
		t.Fatalf("decode forwarded body: %v", err)
	}
	if forwarded.Model != "claude-x" || forwarded.MaxTokens <= 0 {
		t.Fatalf("forwarded body = %+v", forwarded)
	}
}

func TestCustomGeminiUpstreamStream(t *testing.T) {
	stream := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hi\"}]}}]}\n\n" +
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\" there\"}]},\"finishReason\":\"STOP\"}]," +
		"\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":2,\"totalTokenCount\":5}}\n\n"
	srv, calls := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream)
	})
	cm := CustomModel{
		ID: "cm-g", Name: "gemini-local", Protocol: "gemini", BaseURL: srv.URL,
		UpstreamModel: "gemini-2.5-flash", Enabled: true, InputTypes: []string{"text"},
	}
	g := newTestGateway(t, customFixture(t, []CustomModel{cm}, customUpstreamKey, Options{Plugins: newFakeRegistry()}))

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"gemini-local","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	text := rr.Body.String()
	for _, want := range []string{`"content":"Hi"`, `"content":" there"`, `"finish_reason":"stop"`,
		`"prompt_tokens":3`, `"completion_tokens":2`, `"model":"gemini-local"`, "data: [DONE]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("stream missing %q: %s", want, text)
		}
	}

	got := calls.last(t)
	if got.Path != "/v1beta/models/gemini-2.5-flash:streamGenerateContent" {
		t.Fatalf("upstream path = %q", got.Path)
	}
	if got.RawQuery != "alt=sse" {
		t.Fatalf("upstream query = %q, want alt=sse", got.RawQuery)
	}
	if got.Header.Get("x-goog-api-key") != customUpstreamKey {
		t.Fatalf("x-goog-api-key = %q", got.Header.Get("x-goog-api-key"))
	}
	var forwarded struct {
		Contents []map[string]any `json:"contents"`
	}
	if err := json.Unmarshal(got.Body, &forwarded); err != nil || len(forwarded.Contents) == 0 {
		t.Fatalf("forwarded Gemini body = %s (err %v)", got.Body, err)
	}
}
