package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestChatHeadersMatchReference pins the header asymmetry the reference
// documents: chat takes BOTH Authorization: Bearer and the token header, while
// the models catalog takes ONLY the token header (sending a bearer there is
// documented to return 100002 "缺少 token").
func TestChatHeadersMatchReference(t *testing.T) {
	var gotAuth, gotToken, gotAccept, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotToken = r.Header.Get("token")
		gotAccept = r.Header.Get("Accept")
		gotCT = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := newUpstreamClient()
	resp, err := c.chat(t.Context(), srv.URL, "tok-abc", []byte(`{"model":"m","stream":true}`))
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	defer resp.Body.Close()

	if gotAuth != "Bearer tok-abc" {
		t.Errorf("chat Authorization = %q, want %q", gotAuth, "Bearer tok-abc")
	}
	if gotToken != "tok-abc" {
		t.Errorf("chat token header = %q, want %q (the reference sends both)", gotToken, "tok-abc")
	}
	if gotAccept != "text/event-stream" {
		t.Errorf("chat Accept = %q, want text/event-stream", gotAccept)
	}
	if gotCT != "application/json" {
		t.Errorf("chat Content-Type = %q, want application/json", gotCT)
	}
}

func TestModelsUsesTokenHeaderOnly(t *testing.T) {
	var gotAuth, gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotToken = r.Header.Get("token")
		_, _ = w.Write([]byte(`{"code":"000000","data":[{"id":"spark-x","type":"chat","context_length":1048576}]}`))
	}))
	defer srv.Close()

	body, err := newUpstreamClient().models(t.Context(), srv.URL, "tok-models")
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	if gotToken != "tok-models" {
		t.Errorf("models token header = %q, want %q", gotToken, "tok-models")
	}
	if gotAuth != "" {
		t.Errorf("models sent Authorization = %q, want empty: the reference says a bearer here yields 100002", gotAuth)
	}
	models, err := parseModelsBody(body)
	if err != nil {
		t.Fatalf("parseModelsBody: %v", err)
	}
	if len(models) != 1 || models[0].ID != "spark-x" {
		t.Fatalf("models = %+v, want one entry spark-x", models)
	}
	if models[0].ContextLength != 1048576 {
		t.Errorf("context length = %d", models[0].ContextLength)
	}
}

// TestParseModelsBodyFiltersNonChatAndSlashIDs covers the two catalog filters the
// reference applies: only type=="chat" rows are chat models, and an id
// containing "/" is not publishable to the host.
func TestParseModelsBodyFiltersNonChatAndSlashIDs(t *testing.T) {
	body := []byte(`[
		{"id":"chat-a","type":"chat","context_length":1000},
		{"id":"embed-b","type":"embedding","context_length":2000},
		{"id":"vendor/chat-c","type":"chat"},
		{"id":"","type":"chat"},
		{"id":"chat-d","type":"chat","capabilities":{"input_modalities":["text","image"]}}
	]`)
	models, err := parseModelsBody(body)
	if err != nil {
		t.Fatalf("parseModelsBody: %v", err)
	}
	got := map[string]loomyModel{}
	for _, m := range models {
		got[m.ID] = m
	}
	if _, ok := got["embed-b"]; ok {
		t.Error("embedding row must be filtered out")
	}
	if _, ok := got["vendor/chat-c"]; ok {
		t.Error("an id containing / must be dropped")
	}
	if _, ok := got[""]; ok {
		t.Error("an empty id must be dropped")
	}
	if len(got) != 2 {
		t.Fatalf("kept %d models, want 2: %+v", len(got), got)
	}
	if !got["chat-d"].Vision {
		t.Error("chat-d declares image input modality but Vision is false")
	}
	if got["chat-a"].Vision {
		t.Error("chat-a declares no image modality but Vision is true")
	}
}

func TestForceStreamPreservesOtherFields(t *testing.T) {
	in := []byte(`{"model":"spark-x","messages":[{"role":"user","content":"hi"}],"temperature":0.3,"max_tokens":64,"stream":false}`)
	out, err := forceStream(in)
	if err != nil {
		t.Fatalf("forceStream: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"stream":true`) {
		t.Fatalf("stream not forced: %s", s)
	}
	for _, want := range []string{`"model":"spark-x"`, `"temperature":0.3`, `"max_tokens":64`, `"content":"hi"`} {
		if !strings.Contains(s, want) {
			t.Errorf("field %s lost: %s", want, s)
		}
	}
	if streamRequested(in) {
		t.Error("streamRequested reported true for stream:false")
	}
	if !streamRequested(out) {
		t.Error("streamRequested reported false after forceStream")
	}

	if _, err := forceStream([]byte(`[1,2]`)); err == nil {
		t.Error("forceStream accepted a JSON array; want an error")
	}
}

// TestAggregateSSEMergesToolCallFragments is the accumulation contract: the first
// tool fragment carries id/name, later ones only arguments, and an empty name
// must never overwrite the parsed one (that produces `unknown tool ""`).
func TestAggregateSSEMergesToolCallFragments(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"c1","created":123,"choices":[{"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
		`data: {"id":"c1","choices":[{"delta":{"content":"Hello "},"finish_reason":null}]}`,
		`data: {"id":"c1","choices":[{"delta":{"content":"world"},"finish_reason":null}]}`,
		// toolless first fragment: id + name
		`data: {"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"ci"}}]},"finish_reason":null}]}`,
		// continuation: empty name, more arguments
		`data: {"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"","arguments":"ty\":\"SH\"}"}}]},"finish_reason":null}]}`,
		`data: {"id":"c1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n\n")

	out, err := aggregateSSE(strings.NewReader(stream), "spark-x")
	if err != nil {
		t.Fatalf("aggregateSSE: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		`"content":"Hello world"`,
		`"id":"call_1"`,
		`"name":"get_weather"`,
		`"finish_reason":"tool_calls"`,
		`"model":"spark-x"`,
		`"created":123`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in:\n%s", want, s)
		}
	}
	// The merged arguments must be the concatenation, with the escaped braces.
	if !strings.Contains(s, `"arguments":"{\"city\":\"SH\"}"`) {
		t.Errorf("arguments not concatenated correctly:\n%s", s)
	}
	if strings.Contains(s, `"name":""`) {
		t.Errorf("empty name overwrote the parsed tool name:\n%s", s)
	}
}

func TestAggregateSSENoFramesIsAnError(t *testing.T) {
	if _, err := aggregateSSE(strings.NewReader("event: ping\n\n"), "m"); err == nil {
		t.Fatal("aggregateSSE accepted a stream with no data frames; want an error")
	}
}

// TestFileStoreRoundTripsUnderDPAPI proves the credential really lands on disk
// sealed: a fresh store on the same directory reads it back, and the raw bytes
// do not contain the token.
func TestFileStoreRoundTripsSealed(t *testing.T) {
	dir := t.TempDir()
	first, err := newStore(dir)
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	if first.configured() {
		t.Fatal("a fresh store must report not configured")
	}
	if err := first.set(credentials{AccessToken: "tok-secret-value", UserID: "u1", Label: "acct"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	second, err := newStore(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	creds, ok := second.get()
	if !ok || creds.AccessToken != "tok-secret-value" || creds.UserID != "u1" {
		t.Fatalf("reload = %+v ok=%v", creds, ok)
	}
	if second.sealKind != "dpapi-user" {
		t.Fatalf("sealKind = %q, want dpapi-user on Windows", second.sealKind)
	}
	raw, err := os.ReadFile(second.path)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	if strings.Contains(string(raw), "tok-secret-value") {
		t.Fatal("store file contains the access token in plaintext")
	}
	if err := second.set(credentials{AccessToken: "   "}); err == nil {
		t.Fatal("set accepted an empty token; that would wipe a working login")
	}
}

func TestFingerprintIsShortAndStable(t *testing.T) {
	a := fingerprint("tok-1")
	if a != fingerprint("tok-1") {
		t.Error("fingerprint is not stable")
	}
	if a == fingerprint("tok-2") {
		t.Error("different tokens produced the same fingerprint")
	}
	if len(a) != len("sha256:")+8 {
		t.Errorf("fingerprint %q should be sha256: plus 8 hex chars", a)
	}
	if fingerprint("") != "" {
		t.Error("empty secret should produce an empty fingerprint")
	}
}
