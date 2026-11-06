package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The unit tests above pin the pure conversion functions. These pin the whole
// request path: handler -> protocol -> upstream client -> response translation.
// Without them, a wrong endpoint path or a missing header would still pass every
// unit test and only fail against the live service.

// newTestServer builds a plugin server whose credential points at upstream.
func newTestServer(t *testing.T, upstream string) (*server, http.Handler) {
	t.Helper()
	st, err := newStore(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := st.set(credentials{
		AccessToken: "header.eyJzdWIiOiJhY2N0In0.sig",
		BaseURL:     upstream,
		Nickname:    "test",
	}); err != nil {
		t.Fatalf("set credential: %v", err)
	}
	srv := &server{cfg: pluginConfig{ID: "minimax", Token: "tk"}, store: st, client: newUpstreamClient()}
	return srv, srv.routes()
}

func TestChatSendsTheDocumentedWire(t *testing.T) {
	var gotPath, gotAuth, gotAccept, gotCT string
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		gotCT = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":9}}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"pong\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		io.WriteString(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n")
		io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()

	_, handler := newTestServer(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"MiniMax-M3","reasoning_effort":"high",
			"messages":[{"role":"system","content":"terse"},{"role":"user","content":"ping"}]}`))
	req.Header.Set("Authorization", "Bearer tk")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// The inference path is /mavis/api/v1/llm/v1/messages.
	if gotPath != inferPath {
		t.Fatalf("upstream path = %q, want %q", gotPath, inferPath)
	}
	if gotAuth != "Bearer header.eyJzdWIiOiJhY2N0In0.sig" {
		t.Fatalf("upstream Authorization = %q", gotAuth)
	}
	// inferHeaders sets exactly these three; Accept is text/event-stream.
	if gotAccept != "text/event-stream" {
		t.Fatalf("upstream Accept = %q, want text/event-stream", gotAccept)
	}
	if gotCT != "application/json" {
		t.Fatalf("upstream Content-Type = %q", gotCT)
	}
	// The wire body is Anthropic-shaped: stream forced on, system as a string,
	// and the effort expressed as adaptive thinking plus output_config.
	if gotBody["stream"] != true {
		t.Fatalf("wire stream = %v, want true", gotBody["stream"])
	}
	if gotBody["system"] != "terse" {
		t.Fatalf("wire system = %v, want the hoisted string", gotBody["system"])
	}
	thinking, _ := gotBody["thinking"].(map[string]any)
	if thinking["type"] != "adaptive" {
		t.Fatalf("wire thinking = %v, want adaptive", gotBody["thinking"])
	}
	oc, _ := gotBody["output_config"].(map[string]any)
	if oc["effort"] != "high" {
		t.Fatalf("wire output_config = %v, want effort high", gotBody["output_config"])
	}

	// The response is folded back into one chat.completion.
	var out struct {
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
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v (%s)", err, rec.Body.String())
	}
	if out.Object != "chat.completion" || out.Choices[0].Message.Content != "pong" {
		t.Fatalf("response = %s", rec.Body.String())
	}
	if out.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q", out.Choices[0].FinishReason)
	}
	if out.Usage.Prompt != 9 || out.Usage.Completion != 2 {
		t.Fatalf("usage = %+v", out.Usage)
	}
}

// A streaming caller gets chat.completion.chunk frames ending in [DONE].
func TestChatStreamsOpenAIChunks(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		io.WriteString(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()

	_, handler := newTestServer(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"MiniMax-M3","stream":true,"messages":[{"role":"user","content":"ping"}]}`))
	req.Header.Set("Authorization", "Bearer tk")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"object":"chat.completion.chunk"`) {
		t.Fatalf("no chunk frames: %s", body)
	}
	if !strings.Contains(body, `"content":"hi"`) {
		t.Fatalf("no text delta: %s", body)
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Fatalf("stream does not end with [DONE]: %s", body)
	}
}

// A 402 from the upstream is a quota signal, and the plugin must not report it as
// a generic 502 (the host routes on the status).
func TestChatMapsUpstream402ToQuotaExceeded(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		io.WriteString(w, `{"error":{"message":"quota exhausted"}}`)
	}))
	defer upstream.Close()

	_, handler := newTestServer(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"MiniMax-M3","messages":[{"role":"user","content":"x"}]}`))
	req.Header.Set("Authorization", "Bearer tk")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 for an upstream 402", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "quota_exceeded") {
		t.Fatalf("body = %s, want a quota_exceeded error type", rec.Body.String())
	}
}

// The credits envelope is read defensively: a base_resp error or a missing
// details array means "unknown", never a fabricated zero.
func TestParseCreditTotal(t *testing.T) {
	t.Run("sums details inside data", func(t *testing.T) {
		raw := []byte(`{"base_resp":{"status_code":0},"data":{"details":[{"remaining_amount":12.5},{"remaining_amount":7}]}}`)
		total, ok := parseCreditTotal(raw)
		if !ok || total != 19.5 {
			t.Fatalf("total = %v ok = %v, want 19.5", total, ok)
		}
	})
	t.Run("business error is rejected", func(t *testing.T) {
		raw := []byte(`{"base_resp":{"status_code":1004},"data":{"details":[{"remaining_amount":12}]}}`)
		if _, ok := parseCreditTotal(raw); ok {
			t.Fatal("a non-zero base_resp.status_code must not yield a balance")
		}
	})
	t.Run("missing details is rejected", func(t *testing.T) {
		if _, ok := parseCreditTotal([]byte(`{"base_resp":{"status_code":0}}`)); ok {
			t.Fatal("a payload with no details array must not yield a balance")
		}
	})
	t.Run("raw details without the data wrapper", func(t *testing.T) {
		raw := []byte(`{"details":[{"remaining_amount":3}]}`)
		total, ok := parseCreditTotal(raw)
		if !ok || total != 3 {
			t.Fatalf("total = %v ok = %v, want 3", total, ok)
		}
	})
}

// The credit endpoint reports only how much is left; it has no denominator.
// Echoing the remaining amount back as `total` would make the host compute
// remaining/total = 100% and never raise a low-quota alert, so /accounts must
// report `remaining` and `plan` alone.
func TestAccountsQuotaDoesNotFabricateTotal(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != creditPath {
			t.Errorf("credit path = %q, want %q", r.URL.Path, creditPath)
		}
		io.WriteString(w, `{"base_resp":{"status_code":0},"data":{"details":[{"remaining_amount":12.5},{"remaining_amount":7}]}}`)
	}))
	defer upstream.Close()

	_, handler := newTestServer(t, upstream.URL)
	req := httptest.NewRequest(http.MethodGet, "/accounts", nil)
	req.Header.Set("Authorization", "Bearer tk")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Accounts []struct {
			Quota map[string]any `json:"quota"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode /accounts: %v (%s)", err, rec.Body.String())
	}
	if len(out.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(out.Accounts))
	}
	quota := out.Accounts[0].Quota
	if _, ok := quota["total"]; ok {
		t.Fatalf("quota carries a fabricated total: %v", quota)
	}
	if got, ok := quota["remaining"].(float64); !ok || got != 19.5 {
		t.Fatalf("quota.remaining = %v, want 19.5", quota["remaining"])
	}
	if quota["plan"] != "credits" {
		t.Fatalf("quota.plan = %v, want credits", quota["plan"])
	}
}
