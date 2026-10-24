package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aiworkgate/internal/model"
)

func TestAuthentication(t *testing.T) {
	srv, upstream := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"ok":true}`))
	reg := newFakeRegistry().add(
		pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "plug-token")
	keys := newFakeKeys()
	logs := &fakeLogSink{}
	g := newTestGateway(t, Options{Plugins: reg, Keys: keys, Log: logs})

	body := `{"model":"p/chat","messages":[]}`
	cases := []struct {
		name string
		auth string
		want int
	}{
		{"missing", "", http.StatusUnauthorized},
		{"wrong_scheme", "Basic abc", http.StatusUnauthorized},
		{"wrong_token", "Bearer nope", http.StatusUnauthorized},
		{"valid", "Bearer " + clientKey, http.StatusOK},
		{"lowercase_scheme", "bearer " + clientKey, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rr := serve(g, req)
			if rr.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", rr.Code, tc.want, rr.Body.String())
			}
			if tc.want == http.StatusUnauthorized {
				if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
					t.Fatalf("Content-Type = %q, want JSON", ct)
				}
				payload := decodeError(t, rr)
				if payload.Error.Type != "authentication_error" || payload.Error.Code != "invalid_api_key" {
					t.Fatalf("unexpected error body: %+v", payload.Error)
				}
			}
		})
	}

	if got := keys.touchCount("key-1"); got != 2 {
		t.Fatalf("KeyVerifier.Touch calls = %d, want 2", got)
	}
	if got := upstream.count(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (unauthorized requests must not be proxied)", got)
	}
	if _, touches := reg.counts("p"); touches != 2 {
		t.Fatalf("plugin Touch calls = %d, want 2", touches)
	}
	rows := logs.wait(t, len(cases))
	for _, row := range rows {
		if row.KeyID != "" {
			continue
		}
		if row.Status != http.StatusUnauthorized {
			t.Fatalf("unauthenticated row status = %d, want 401", row.Status)
		}
	}
}

func TestChatBareModelForwardedToA(t *testing.T) {
	const respBody = `{"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":4}}`
	srv, calls := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream", "a")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, respBody)
	})
	fa := &fakeA{base: srv.URL, key: "sk-a"}
	logs := &fakeLogSink{}
	g := newTestGateway(t, Options{Plugins: newFakeRegistry(), A: fa, Log: logs})

	const body = `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"temperature":0.2}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?trace=1", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+clientKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Client", "keep")
	rr := serve(g, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	if got := rr.Body.String(); got != respBody {
		t.Fatalf("response body = %q, want %q", got, respBody)
	}
	if got := rr.Header().Get("X-Upstream"); got != "a" {
		t.Fatalf("X-Upstream = %q, want a", got)
	}
	if got := rr.Header().Get(sourceHeader); got != "a" {
		t.Fatalf("source header = %q, want a", got)
	}
	if got := rr.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}

	if calls.count() != 1 {
		t.Fatalf("A calls = %d, want 1", calls.count())
	}
	got := calls.last(t)
	if got.Method != http.MethodPost || got.Path != "/v1/chat/completions" || got.RawQuery != "trace=1" {
		t.Fatalf("A request = %s %s?%s", got.Method, got.Path, got.RawQuery)
	}
	if got.Authorization != "Bearer sk-a" {
		t.Fatalf("A Authorization = %q, want Bearer sk-a (client key must be replaced)", got.Authorization)
	}
	if got.ContentType != "application/json" {
		t.Fatalf("A Content-Type = %q", got.ContentType)
	}
	if got.Header.Get("X-Client") != "keep" {
		t.Fatalf("client header was not preserved: %v", got.Header)
	}
	if string(got.Body) != body {
		t.Fatalf("A body = %q, want original %q", got.Body, body)
	}

	rows := logs.wait(t, 1)
	row := rows[0]
	if row.Source != "a" || row.Model != "gpt-4o" || row.Status != http.StatusOK || row.Stream {
		t.Fatalf("unexpected log row: %+v", row)
	}
	if row.KeyID != "key-1" || row.KeyName != "default" {
		t.Fatalf("key fields not logged: %+v", row)
	}
	if row.PromptTokens != 3 || row.CompletionTokens != 4 {
		t.Fatalf("usage = %d/%d, want 3/4", row.PromptTokens, row.CompletionTokens)
	}
	if row.LatencyMS < 0 || row.TTFBMS < 0 {
		t.Fatalf("negative latency fields: %+v", row)
	}
}

func TestChatPluginForwardRewritesModelOnly(t *testing.T) {
	const respBody = `{"id":"pl-1","choices":[]}`
	srv, calls := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream", "plugin")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, respBody)
	})
	reg := newFakeRegistry().add(
		pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "plug-token")
	logs := &fakeLogSink{}
	g := newTestGateway(t, Options{Plugins: reg, Log: logs})

	const body = `{"model":"p/chat","messages":[{"role":"user","content":"hallo"}],"temperature":0.7,"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],"metadata":{"tag":"värde"},"max_tokens":42}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?trace=2", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+clientKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Client", "keep")
	rr := serve(g, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	if got := rr.Body.String(); got != respBody {
		t.Fatalf("response body = %q, want %q", got, respBody)
	}
	if got := rr.Header().Get(sourceHeader); got != "plugin:p" {
		t.Fatalf("source header = %q, want plugin:p", got)
	}

	if calls.count() != 1 {
		t.Fatalf("plugin calls = %d, want 1", calls.count())
	}
	got := calls.last(t)
	if got.Path != "/v1/chat/completions" || got.RawQuery != "trace=2" {
		t.Fatalf("plugin path = %s?%s", got.Path, got.RawQuery)
	}
	if got.Authorization != "Bearer plug-token" {
		t.Fatalf("plugin Authorization = %q, want Bearer plug-token", got.Authorization)
	}
	if got.ContentType != "application/json" || got.Header.Get("X-Client") != "keep" {
		t.Fatalf("headers not preserved: %v", got.Header)
	}
	if strings.Contains(string(got.Body), "p/chat") {
		t.Fatalf("model prefix was not stripped: %s", got.Body)
	}
	var sent struct {
		Model       string `json:"model"`
		MaxTokens   int    `json:"max_tokens"`
		Temperature float64
		Messages    []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
		Metadata map[string]string `json:"metadata"`
	}
	if err := json.Unmarshal(got.Body, &sent); err != nil {
		t.Fatalf("plugin body is not JSON: %v", err)
	}
	if sent.Model != "chat" {
		t.Fatalf("plugin model = %q, want chat", sent.Model)
	}
	if sent.MaxTokens != 42 || sent.Temperature != 0.7 {
		t.Fatalf("scalar fields changed: %+v", sent)
	}
	if len(sent.Messages) != 1 || sent.Messages[0].Role != "user" || sent.Messages[0].Content != "hallo" {
		t.Fatalf("messages changed: %+v", sent.Messages)
	}
	if len(sent.Tools) != 1 || sent.Tools[0].Type != "function" || sent.Tools[0].Function.Name != "f" {
		t.Fatalf("tools changed: %+v", sent.Tools)
	}
	if sent.Metadata["tag"] != "värde" {
		t.Fatalf("metadata changed: %+v", sent.Metadata)
	}
	if ensures, _ := reg.counts("p"); ensures != 1 {
		t.Fatalf("EnsureRunning calls = %d, want 1", ensures)
	}
	if _, touches := reg.counts("p"); touches != 1 {
		t.Fatalf("plugin Touch calls = %d, want 1", touches)
	}

	rows := logs.wait(t, 1)
	row := rows[0]
	if row.Source != "plugin:p" || row.Model != "p/chat" || row.Status != http.StatusOK || row.Stream {
		t.Fatalf("unexpected log row: %+v", row)
	}
}

func TestSSEPassthroughIsIncremental(t *testing.T) {
	const (
		firstEvent  = "data: {\"choices\":[{\"delta\":{\"content\":\"one\"}}]}\n\n"
		secondEvent = "data: {\"choices\":[{\"delta\":{\"content\":\"two\"}}]}\n\n"
		usageEvent  = "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":7}}\n\n"
		doneEvent   = "data: [DONE]\n\n"
	)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()

	entered := make(chan struct{}, 1)
	srv, calls := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, firstEvent)
		w.(http.Flusher).Flush()
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		case <-time.After(5 * time.Second):
			return
		}
		_, _ = io.WriteString(w, secondEvent+usageEvent+doneEvent)
		w.(http.Flusher).Flush()
	})
	reg := newFakeRegistry().add(
		pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "plug-token")
	logs := &fakeLogSink{}
	g := newTestGateway(t, Options{Plugins: reg, Log: logs})
	gw := httptest.NewServer(g)
	defer gw.Close()

	req, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"p/chat","stream":true,"messages":[]}`))
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
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache", got)
	}
	if got := resp.Header.Get(sourceHeader); got != "plugin:p" {
		t.Fatalf("source header = %q, want plugin:p", got)
	}

	var raw bytes.Buffer
	br := bufio.NewReader(io.TeeReader(resp.Body, &raw))
	readLine := func() string {
		t.Helper()
		type result struct {
			line string
			err  error
		}
		ch := make(chan result, 1)
		go func() {
			line, err := br.ReadString('\n')
			ch <- result{line: line, err: err}
		}()
		select {
		case res := <-ch:
			if res.err != nil {
				t.Fatalf("read SSE line: %v", res.err)
			}
			return res.line
		case <-time.After(3 * time.Second):
			t.Fatal("SSE chunk was buffered; first event was not flushed")
			return ""
		}
	}

	if line := readLine(); line != "data: {\"choices\":[{\"delta\":{\"content\":\"one\"}}]}\n" {
		t.Fatalf("first SSE line = %q", line)
	}
	if line := readLine(); line != "\n" {
		t.Fatalf("event separator = %q, want blank line", line)
	}
	// The upstream is still blocked on release, so receiving the first event
	// above already proves incremental forwarding.
	select {
	case <-entered:
	default:
		t.Fatal("upstream did not block after the first chunk")
	}
	unblock()

	if _, err := io.ReadAll(br); err != nil {
		t.Fatalf("read rest of SSE: %v", err)
	}
	if want := firstEvent + secondEvent + usageEvent + doneEvent; raw.String() != want {
		t.Fatalf("SSE body mismatch:\n got %q\nwant %q", raw.String(), want)
	}

	if calls.count() != 1 {
		t.Fatalf("plugin calls = %d, want 1", calls.count())
	}
	forwarded := calls.last(t)
	if forwarded.Authorization != "Bearer plug-token" {
		t.Fatalf("plugin Authorization = %q", forwarded.Authorization)
	}
	if strings.Contains(string(forwarded.Body), "p/chat") {
		t.Fatalf("SSE request model prefix was not stripped: %s", forwarded.Body)
	}

	rows := logs.wait(t, 1)
	row := rows[0]
	if row.Source != "plugin:p" || row.Model != "p/chat" || row.Status != http.StatusOK {
		t.Fatalf("unexpected SSE log row: %+v", row)
	}
	if !row.Stream {
		t.Fatalf("stream flag not logged: %+v", row)
	}
	if row.PromptTokens != 5 || row.CompletionTokens != 7 {
		t.Fatalf("SSE usage = %d/%d, want 5/7", row.PromptTokens, row.CompletionTokens)
	}
}

func TestConcurrencyLimitQueueTimeout(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()

	srv, calls := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	reg := newFakeRegistry()
	reg.add(pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "tok")
	reg.setConcurrency("p", 1)
	logs := &fakeLogSink{}
	g := newTestGateway(t, Options{
		Plugins:      reg,
		Log:          logs,
		QueueTimeout: 50 * time.Millisecond,
	})

	body := `{"model":"p/chat","messages":[]}`
	firstRec := httptest.NewRecorder()
	firstDone := make(chan struct{})
	go func() {
		g.ServeHTTP(firstRec, newChatRequest(t, "/v1/chat/completions", clientKey, body))
		close(firstDone)
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first request never reached the plugin")
	}

	start := time.Now()
	secondRec := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, body))
	elapsed := time.Since(start)
	if secondRec.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want 429 (body %s)", secondRec.Code, secondRec.Body.String())
	}
	payload := decodeError(t, secondRec)
	if payload.Error.Type != "concurrency_limit" {
		t.Fatalf("error type = %q, want concurrency_limit", payload.Error.Type)
	}
	if elapsed < 25*time.Millisecond {
		t.Fatalf("429 returned after %v, expected it to wait for the 50ms queue timeout", elapsed)
	}

	unblock()
	select {
	case <-firstDone:
	case <-time.After(3 * time.Second):
		t.Fatal("first request did not finish")
	}
	if firstRec.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200 (body %s)", firstRec.Code, firstRec.Body.String())
	}
	if calls.count() != 1 {
		t.Fatalf("plugin calls = %d, want 1", calls.count())
	}

	rows := logs.wait(t, 2)
	statuses := map[int]bool{}
	for _, row := range rows {
		statuses[row.Status] = true
		if row.Source != "plugin:p" || row.Model != "p/chat" {
			t.Fatalf("unexpected log row: %+v", row)
		}
	}
	if !statuses[200] || !statuses[429] {
		t.Fatalf("logged statuses = %v, want 200 and 429", statuses)
	}
}

func TestPassThroughEndpoints(t *testing.T) {
	aSrv, aCalls := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"a":true}`))
	pSrv, pCalls := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"p":true}`))
	reg := newFakeRegistry().add(
		pluginStatus("p", true, model.ModelInfo{ID: "chat"}), pSrv.URL, "tok")
	g := newTestGateway(t, Options{
		Plugins: reg,
		A:       &fakeA{base: aSrv.URL, key: "sk-a"},
	})

	paths := []string{
		"/v1/messages",
		"/v1/responses",
		"/v1/completions",
		"/v1/embeddings",
		"/v1/images/generations",
	}
	for _, path := range paths {
		t.Run("bare"+path, func(t *testing.T) {
			body := `{"model":"gpt-4o","input":"x"}`
			rr := serve(g, newChatRequest(t, path, clientKey, body))
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
			}
			if got := rr.Body.String(); got != `{"a":true}` {
				t.Fatalf("body = %q", got)
			}
			got := aCalls.last(t)
			if got.Path != path || got.Authorization != "Bearer sk-a" {
				t.Fatalf("A request = %s auth %q, want %s", got.Path, got.Authorization, path)
			}
			if string(got.Body) != body {
				t.Fatalf("A body = %q, want %q", got.Body, body)
			}
		})
	}

	// /v1/completions and /v1/responses are translated for plugin models (see
	// envelope_test.go) and /v1/messages has its own translation (anthropic_test.go),
	// so only the remaining endpoints answer 501.
	for _, path := range []string{"/v1/embeddings", "/v1/images/generations"} {
		t.Run("plugin"+path, func(t *testing.T) {
			beforeA, beforeP := aCalls.count(), pCalls.count()
			rr := serve(g, newChatRequest(t, path, clientKey, `{"model":"p/chat"}`))
			if rr.Code != http.StatusNotImplemented {
				t.Fatalf("status = %d, want 501 (body %s)", rr.Code, rr.Body.String())
			}
			payload := decodeError(t, rr)
			if payload.Error.Code != "unsupported_endpoint" {
				t.Fatalf("error code = %q, want unsupported_endpoint", payload.Error.Code)
			}
			if aCalls.count() != beforeA || pCalls.count() != beforeP {
				t.Fatal("501 response must not reach an upstream")
			}
		})
	}
}

func TestUnknownModelRouting(t *testing.T) {
	aSrv, aCalls := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		var meta struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &meta)
		w.Header().Set("Content-Type", "application/json")
		if meta.Model == "gpt-unknown" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"a says no","type":"invalid_request_error"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	pSrv, pCalls := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"p":true}`))
	reg := newFakeRegistry().add(
		pluginStatus("p", true, model.ModelInfo{ID: "chat"}), pSrv.URL, "tok")
	logs := &fakeLogSink{}
	g := newTestGateway(t, Options{
		Plugins: reg,
		A:       &fakeA{base: aSrv.URL, key: "sk-a"},
		Log:     logs,
	})

	t.Run("known_plugin_unknown_model", func(t *testing.T) {
		rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"p/nope"}`))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body %s)", rr.Code, rr.Body.String())
		}
		payload := decodeError(t, rr)
		if payload.Error.Code != "model_not_found" {
			t.Fatalf("error code = %q, want model_not_found", payload.Error.Code)
		}
		if !strings.Contains(payload.Error.Message, "p/chat") {
			t.Fatalf("404 message does not list plugin models: %q", payload.Error.Message)
		}
		if aCalls.count() != 0 || pCalls.count() != 0 {
			t.Fatal("unknown plugin model must not be proxied")
		}
	})

	// "other/x" is not a known plugin id. Current behavior (documented by this
	// test): it takes the bare/A route, because only installed plugin ids are
	// treated as prefixes. A decides what to do with the unknown name.
	t.Run("unknown_plugin_prefix_goes_to_a", func(t *testing.T) {
		rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"other/x"}`))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
		}
		got := aCalls.last(t)
		var meta struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(got.Body, &meta); err != nil {
			t.Fatalf("A body: %v", err)
		}
		if meta.Model != "other/x" {
			t.Fatalf("A model = %q, want other/x", meta.Model)
		}
		if got.Authorization != "Bearer sk-a" {
			t.Fatalf("A Authorization = %q", got.Authorization)
		}
	})

	t.Run("empty_inner_model", func(t *testing.T) {
		beforeA, beforeP := aCalls.count(), pCalls.count()
		rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"p/"}`))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body %s)", rr.Code, rr.Body.String())
		}
		if aCalls.count() != beforeA || pCalls.count() != beforeP {
			t.Fatal("empty plugin model must not be proxied")
		}
	})

	t.Run("unknown_bare_model_goes_to_a", func(t *testing.T) {
		rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"gpt-unknown"}`))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want A's 404 (body %s)", rr.Code, rr.Body.String())
		}
		if got := rr.Body.String(); !strings.Contains(got, "a says no") {
			t.Fatalf("A error body was not passed through: %q", got)
		}
	})

	rows := logs.wait(t, 4)
	byStatus := map[int]int{}
	for _, row := range rows {
		byStatus[row.Status]++
	}
	if byStatus[200] != 1 || byStatus[404] != 3 {
		t.Fatalf("logged statuses = %v, want one 200 and three 404", byStatus)
	}
}

func TestPluginUnavailable(t *testing.T) {
	pSrv, pCalls := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"p":true}`))
	cases := []struct {
		name    string
		baseURL string
		ensure  error
		wantMsg string
	}{
		{"ensure_running_error", pSrv.URL, errors.New("boom"), "boom"},
		{"no_endpoint", "", nil, "未运行"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := newFakeRegistry().add(
				pluginStatus("p", true, model.ModelInfo{ID: "chat"}), tc.baseURL, "tok")
			if tc.ensure != nil {
				reg.setEnsureErr("p", tc.ensure)
			}
			logs := &fakeLogSink{}
			g := newTestGateway(t, Options{Plugins: reg, Log: logs})

			rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"p/chat"}`))
			if rr.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503 (body %s)", rr.Code, rr.Body.String())
			}
			if got := rr.Header().Get("Retry-After"); got == "" {
				t.Fatal("503 response is missing Retry-After")
			}
			payload := decodeError(t, rr)
			if payload.Error.Type != "plugin_unavailable" || payload.Error.Code != "plugin_unavailable" {
				t.Fatalf("unexpected error: %+v", payload.Error)
			}
			if !strings.Contains(payload.Error.Message, tc.wantMsg) {
				t.Fatalf("message = %q, want it to contain %q", payload.Error.Message, tc.wantMsg)
			}
			if pCalls.count() != 0 {
				t.Fatal("unavailable plugin must not be called")
			}
			rows := logs.wait(t, 1)
			if rows[0].Status != http.StatusServiceUnavailable || rows[0].Source != "plugin:p" {
				t.Fatalf("unexpected log row: %+v", rows[0])
			}
		})
	}
}

func TestAUnavailable(t *testing.T) {
	dead := deadServerURL(t)
	logs := &fakeLogSink{}
	g := newTestGateway(t, Options{
		Plugins: newFakeRegistry(),
		A:       &fakeA{base: dead, key: "sk-a"},
		Log:     logs,
	})

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"gpt-4o"}`))
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body %s)", rr.Code, rr.Body.String())
	}
	payload := decodeError(t, rr)
	if payload.Error.Type != "upstream_unavailable" || payload.Error.Message != aDownMessage {
		t.Fatalf("unexpected error: %+v", payload.Error)
	}
	if got := rr.Header().Get(sourceHeader); got != "a" {
		t.Fatalf("source header = %q, want a", got)
	}
	rows := logs.wait(t, 1)
	if rows[0].Status != http.StatusBadGateway || rows[0].Source != "a" {
		t.Fatalf("unexpected log row: %+v", rows[0])
	}
	if !strings.Contains(rows[0].Error, aDownMessage) {
		t.Fatalf("error summary = %q, want it to contain %q", rows[0].Error, aDownMessage)
	}

	t.Run("nil_a", func(t *testing.T) {
		g2 := newTestGateway(t, Options{Plugins: newFakeRegistry()})
		rr2 := serve(g2, newChatRequest(t, "/v1/messages", clientKey, `{"model":"claude"}`))
		if rr2.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502 (body %s)", rr2.Code, rr2.Body.String())
		}
		if payload := decodeError(t, rr2); payload.Error.Message != aDownMessage {
			t.Fatalf("message = %q, want %q", payload.Error.Message, aDownMessage)
		}
	})
}

func TestUpstreamRedirectIsPassedThrough(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	srv, _ := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Location", target.URL+"/elsewhere")
		w.WriteHeader(http.StatusTemporaryRedirect)
	})
	g := newTestGateway(t, Options{
		Plugins: newFakeRegistry(),
		A:       &fakeA{base: srv.URL, key: "sk-a"},
	})
	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"gpt-4o"}`))
	if rr.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want 307 (redirects must not be followed)", rr.Code)
	}
	if got := rr.Header().Get("Location"); got != target.URL+"/elsewhere" {
		t.Fatalf("Location = %q", got)
	}
	if got := targetHits.Load(); got != 0 {
		t.Fatalf("redirect target was called %d times; the gateway must not follow redirects", got)
	}
}

func TestUnknownEndpoint(t *testing.T) {
	g := newTestGateway(t, Options{Plugins: newFakeRegistry()})
	rr := serve(g, newGetRequest(t, "/v1/nope", clientKey))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
	if payload := decodeError(t, rr); payload.Error.Type != "not_found_error" {
		t.Fatalf("unexpected error: %+v", payload.Error)
	}
}

func TestNewReturnsWorkingHandler(t *testing.T) {
	srv, calls := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"ok":true}`))
	reg := newFakeRegistry().add(
		pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "tok")
	var handler http.Handler = New(Options{Plugins: reg, Keys: newFakeKeys()})
	if handler == nil {
		t.Fatal("New returned nil")
	}
	rr := serve(handler, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"p/chat"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	if calls.count() != 1 {
		t.Fatalf("plugin calls = %d, want 1", calls.count())
	}
}

func TestInvalidJSONBody(t *testing.T) {
	srv, calls := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"ok":true}`))
	reg := newFakeRegistry().add(
		pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "tok")
	g := newTestGateway(t, Options{Plugins: reg})

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rr.Code, rr.Body.String())
	}
	payload := decodeError(t, rr)
	if payload.Error.Code != "invalid_json" {
		t.Fatalf("error code = %q, want invalid_json", payload.Error.Code)
	}
	if calls.count() != 0 {
		t.Fatal("invalid JSON must not be proxied")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func TestRequestBodyReadError(t *testing.T) {
	g := newTestGateway(t, Options{Plugins: newFakeRegistry()})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", failingReader{})
	req.Header.Set("Authorization", "Bearer "+clientKey)
	rr := serve(g, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rr.Code, rr.Body.String())
	}
	if payload := decodeError(t, rr); payload.Error.Code != "read_error" {
		t.Fatalf("error code = %q, want read_error", payload.Error.Code)
	}
}

func TestRequestBodyTooLarge(t *testing.T) {
	if testing.Short() {
		t.Skip("streams a 32MB body")
	}
	logs := &fakeLogSink{}
	g := newTestGateway(t, Options{Plugins: newFakeRegistry(), Log: logs})
	body := io.LimitReader(fillerReader{}, maxRequestBodyBytes+1)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req.Header.Set("Authorization", "Bearer "+clientKey)
	req.Header.Set("Content-Type", "application/json")
	rr := serve(g, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (body %s)", rr.Code, rr.Body.String())
	}
	payload := decodeError(t, rr)
	if payload.Error.Code != "body_too_large" {
		t.Fatalf("error code = %q, want body_too_large", payload.Error.Code)
	}
	rows := logs.wait(t, 1)
	if rows[0].Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("logged status = %d, want 413", rows[0].Status)
	}
}
