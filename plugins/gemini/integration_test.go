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
// Without them a wrong endpoint path, a missing identity header or a dropped
// envelope key would still pass every unit test and only fail live.

// newTestServer builds a plugin server whose credential points at upstream.
func newTestServer(t *testing.T, upstream string) (*server, http.Handler) {
	t.Helper()
	st, err := newStore(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := st.set(credentials{
		AccessToken:             "ya29.test",
		RefreshToken:            "1//rt",
		Expiry:                  "2099-01-01T00:00:00Z",
		CloudaicompanionProject: "proj-from-credential",
		Email:                   "tester@example.com",
		EndpointBase:            upstream,
	}); err != nil {
		t.Fatalf("set credential: %v", err)
	}
	srv := newServer(pluginConfig{ID: "gemini", Token: "tk"}, st)
	return srv, srv.routes()
}

func TestChatSendsTheDocumentedWire(t *testing.T) {
	var gotPath, gotMethod, gotAccept string
	var headers http.Header
	var body map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		headers = r.Header.Clone()
		if r.URL.Query().Get("alt") == "sse" {
			gotAccept = "alt=sse"
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"pong\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":7,\"candidatesTokenCount\":2}}}\n\n")
	}))
	defer upstream.Close()

	_, handler := newTestServer(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gemini-3.8-flash","reasoning_effort":"low",
			"messages":[{"role":"system","content":"terse"},{"role":"user","content":"ping"}]}`))
	req.Header.Set("Authorization", "Bearer tk")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// The inference path, with alt=sse on the query.
	if gotMethod != http.MethodPost {
		t.Fatalf("upstream method = %q", gotMethod)
	}
	if gotPath != "/v1internal:streamGenerateContent" {
		t.Fatalf("upstream path = %q", gotPath)
	}
	if gotAccept != "alt=sse" {
		t.Fatalf("upstream query alt = %q, want sse", gotAccept)
	}
	// The five identity headers are not decoration; the upstream fingerprints
	// on them. Every one must be present with the reference's exact value.
	for _, want := range identityHeaders {
		if got := headers.Get(want[0]); got != want[1] {
			t.Fatalf("identity header %q = %q, want %q", want[0], got, want[1])
		}
	}
	if got := headers.Get("Authorization"); got != "Bearer ya29.test" {
		t.Fatalf("Authorization = %q", got)
	}
	// The streaming request deliberately sends no Accept header.
	if got := headers.Get("Accept"); got != "" {
		t.Fatalf("Accept must not be sent on the stream request, got %q", got)
	}

	// The wire body is the Cloud Code envelope, not a bare generateContent body.
	if body["model"] != "gemini-3.8-flash-low" {
		t.Fatalf("envelope model = %v, want the tiered suffix", body["model"])
	}
	if body["project"] != "proj-from-credential" {
		t.Fatalf("envelope project = %v", body["project"])
	}
	if body["userAgent"] != "antigravity" {
		t.Fatalf("envelope userAgent = %v", body["userAgent"])
	}
	if !strings.HasPrefix(anyString(body["requestId"]), "agent/") {
		t.Fatalf("envelope requestId = %v, want the agent/<ms>/<hex> shape", body["requestId"])
	}
	// A bare generateContent body would put contents at the top level; the
	// envelope must nest it under "request".
	if _, leaked := body["contents"]; leaked {
		t.Fatalf("contents must be nested under request, not top level: %v", body)
	}
	inner, ok := body["request"].(map[string]any)
	if !ok {
		t.Fatalf("no request object: %v", body)
	}
	if _, ok := inner["contents"]; !ok {
		t.Fatalf("request.contents missing: %v", inner)
	}
	gc, _ := inner["generationConfig"].(map[string]any)
	if gc["maxOutputTokens"] != float64(defaultMaxOutputTokens) {
		t.Fatalf("maxOutputTokens = %v, want %d", gc["maxOutputTokens"], defaultMaxOutputTokens)
	}
	tc, _ := gc["thinkingConfig"].(map[string]any)
	// effort low -> budget 1000.
	if tc["thinkingBudget"] != float64(budgetLow) {
		t.Fatalf("thinkingBudget = %v, want %d", tc["thinkingBudget"], budgetLow)
	}
	if tc["includeThoughts"] != true {
		t.Fatalf("includeThoughts = %v, want true", tc["includeThoughts"])
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
	if out.Usage.Prompt != 7 || out.Usage.Completion != 2 {
		t.Fatalf("usage = %+v", out.Usage)
	}
}

func anyString(v any) string {
	s, _ := v.(string)
	return s
}

// The sandbox endpoint is the fallback when the daily one refuses.
func TestPostStreamFallsBackToTheSandboxEndpoint(t *testing.T) {
	var hits []string
	daily := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, "daily")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"error":{"message":"permission denied"}}`)
	}))
	defer daily.Close()
	sandbox := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, "sandbox")
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"from sandbox\"}]},\"finishReason\":\"STOP\"}]}\n\n")
	}))
	defer sandbox.Close()

	srv := newServer(pluginConfig{ID: "gemini", Token: "tk"}, nil)
	spec, err := modelSpecFor("gemini-3.8-flash", "medium")
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	wire, err := translateRequest([]byte(`{"model":"gemini-3.8-flash","messages":[{"role":"user","content":"hi"}]}`),
		requestOptions{ModelID: "gemini-3.8-flash", Spec: spec, Project: "proj"})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	// Drive the real rotation helper with the two mock endpoints in the
	// handler's order (daily then sandbox).
	resp, _, err := srv.postStream(t.Context(), "ya29.test", []string{daily.URL, sandbox.URL}, wire)
	if err != nil {
		t.Fatalf("postStream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want the sandbox 200", resp.StatusCode)
	}
	var text strings.Builder
	tl := newGeminiTranslator(func(delta map[string]any, finish any) {
		if s, ok := delta["content"].(string); ok {
			text.WriteString(s)
		}
	})
	if err := pumpGemini(resp.Body, tl); err != nil {
		t.Fatalf("pump: %v", err)
	}
	if text.String() != "from sandbox" {
		t.Fatalf("content = %q, want the sandbox answer", text.String())
	}
	if len(hits) != 2 || hits[0] != "daily" || hits[1] != "sandbox" {
		t.Fatalf("endpoint order = %v, want daily then sandbox", hits)
	}
}

// A 401 must not be retried against the second endpoint: the credential is the
// problem, so rotating endpoints would only double the failed calls.
func TestChatStopsOnUnauthorized(t *testing.T) {
	var hits int
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"invalid token"}}`)
	}))
	defer mock.Close()

	_, handler := newTestServer(t, mock.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gemini-3.8-flash","messages":[{"role":"user","content":"x"}]}`))
	req.Header.Set("Authorization", "Bearer tk")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want the upstream 401 passed through", rec.Code)
	}
	if hits != 1 {
		t.Fatalf("upstream hits = %d, want exactly 1 (no endpoint rotation on 401)", hits)
	}
}

// The project probe uses loadCodeAssist with the documented body.
func TestLoadCodeAssistProbeSendsDocumentedBody(t *testing.T) {
	var gotPath, gotBody string
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		io.WriteString(w, `{"cloudaicompanionProject":"proj-probed"}`)
	}))
	defer mock.Close()

	srv := &server{cfg: pluginConfig{ID: "gemini"}, client: newUpstreamClient()}
	project, probed, err := srv.client.loadCodeAssist(t.Context(), "ya29.test", defaultAccountID, []string{mock.URL})
	if err != nil {
		t.Fatalf("loadCodeAssist: %v", err)
	}
	if gotPath != codeAssistPth {
		t.Fatalf("path = %q, want %q", gotPath, codeAssistPth)
	}
	// The body is sent byte for byte from GEMINI_LOAD_CODE_ASSIST_BODY.
	if gotBody != loadCodeAssistBody {
		t.Fatalf("body = %q, want %q", gotBody, loadCodeAssistBody)
	}
	if project != "proj-probed" || !probed {
		t.Fatalf("project = %q probed = %v", project, probed)
	}
}

// A model outside the one-model catalog is a 404, not a silent substitution.
func TestChatRejectsUnknownModel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream must not be called for an unknown model")
	}))
	defer upstream.Close()

	_, handler := newTestServer(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"x"}]}`))
	req.Header.Set("Authorization", "Bearer tk")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
