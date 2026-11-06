package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestChatHeadersMatchReference pins the exact header set the reference's
// inline headers() closure sends on chat: bearer + X-Org-Code + language +
// client platform, and Accept: text/event-stream.
func TestChatHeadersMatchReference(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		if r.URL.Path != llmChatPath {
			t.Errorf("chat path = %q, want %q", r.URL.Path, llmChatPath)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	cred := credentials{AccessToken: "tok-abc", OfficeIdentity: "org-42"}
	resp, err := newUpstreamClient().chat(t.Context(), srv.URL, cred, []byte(`{"model":"m","stream":true}`), "")
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	defer resp.Body.Close()

	if v := got.Get("Authorization"); v != "Bearer tok-abc" {
		t.Errorf("Authorization = %q", v)
	}
	if v := got.Get("X-Org-Code"); v != "org-42" {
		t.Errorf("X-Org-Code = %q, want the stored office_identity", v)
	}
	if v := got.Get("X-Raccoon-Language"); v != "zh" {
		t.Errorf("X-Raccoon-Language = %q, want zh", v)
	}
	if v := got.Get("X-Client-Platform"); v != clientPlatform {
		t.Errorf("X-Client-Platform = %q, want %q", v, clientPlatform)
	}
	if v := got.Get("Accept"); v != "text/event-stream" {
		t.Errorf("Accept = %q, want text/event-stream", v)
	}
}

// TestPersonalAccountSendsEmptyOrgCode covers the reference's note that a
// personal account sends an empty X-Org-Code header rather than omitting it.
func TestPersonalAccountSendsEmptyOrgCode(t *testing.T) {
	var present bool
	var value string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, present = r.Header["X-Org-Code"]
		value = r.Header.Get("X-Org-Code")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	resp, err := newUpstreamClient().chat(t.Context(), srv.URL, credentials{AccessToken: "t"}, []byte(`{"stream":true}`), "")
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	defer resp.Body.Close()
	if !present {
		t.Error("X-Org-Code header was omitted; the reference always sends it (empty for personal accounts)")
	}
	if value != "" {
		t.Errorf("X-Org-Code = %q, want empty", value)
	}
}

// TestThinkingStaysInsideExtraBody pins the reference's exact mapping
// (raccoonThinkingExtraBody): reasoning_effort "on"/"off" becomes
// extra_body.thinking.type "enabled"/"disabled", and the top-level field is
// removed because the upstream ignores it there.
func TestThinkingStaysInsideExtraBody(t *testing.T) {
	cases := map[string]string{"on": "enabled", "off": "disabled", "weird": "enabled"}
	for effort, wantType := range cases {
		in := []byte(`{"model":"sn-glm-5-3","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"` + effort + `","stream":false}`)
		out, err := forceStream(in)
		if err != nil {
			t.Fatalf("forceStream(%q): %v", effort, err)
		}
		var parsed struct {
			ExtraBody struct {
				Thinking struct {
					Type string `json:"type"`
				} `json:"thinking"`
			} `json:"extra_body"`
			Messages []map[string]any `json:"messages"`
			Stream   bool             `json:"stream"`
		}
		if err := json.Unmarshal(out, &parsed); err != nil {
			t.Fatalf("forceStream produced invalid JSON: %v", err)
		}
		var top map[string]json.RawMessage
		_ = json.Unmarshal(out, &top)
		if _, leaked := top["reasoning_effort"]; leaked {
			t.Errorf("reasoning_effort leaked to the top level (upstream ignores it there): %s", out)
		}
		if got := parsed.ExtraBody.Thinking.Type; got != wantType {
			t.Errorf("effort %q -> thinking.type %q, want %q: %s", effort, got, wantType, out)
		}
		if !parsed.Stream || len(parsed.Messages) != 1 {
			t.Errorf("stream/messages lost for effort %q: %s", effort, out)
		}
	}
}

// TestNoEffortSendsNoExtraBody matches the reference, which omits extra_body
// entirely when no effort was given (server default = on).
func TestNoEffortSendsNoExtraBody(t *testing.T) {
	out, err := forceStream([]byte(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatalf("forceStream: %v", err)
	}
	if strings.Contains(string(out), "extra_body") {
		t.Fatalf("absent reasoning_effort must not invent extra_body: %s", out)
	}
}

func TestForceStreamKeepsExistingExtraBody(t *testing.T) {
	in := []byte(`{"model":"m","extra_body":{"foo":"bar"},"reasoning_effort":"off"}`)
	out, err := forceStream(in)
	if err != nil {
		t.Fatalf("forceStream: %v", err)
	}
	s := string(out)
	for _, want := range []string{`"foo":"bar"`, `"thinking":{"type":"disabled"}`} {
		if !strings.Contains(s, want) {
			t.Errorf("extra_body lost %s: %s", want, s)
		}
	}
	if strings.Contains(s, `"reasoning_effort"`) {
		t.Errorf("raw reasoning_effort survived inside extra_body: %s", s)
	}
}

// TestParseCatalogReadsEnvelope checks the catalog envelope shape and the two
// display rules the reference applies: a price multiplier suffix and the
// discount arrow.
func TestParseCatalogReadsEnvelope(t *testing.T) {
	body := []byte(`{"code":0,"data":{"list":[
		{"id":"sn-glm-5-3","description":"GLM-5-3","context_window":1000000,"baseMultiplier":1.0,"effectiveMultiplier":0.75},
		{"id":"free-model","description":"Free","context_window":256000,"effectiveMultiplier":0},
		{"id":"vendor/x","description":"should be dropped"},
		{"id":"","description":"empty id"}
	]}}`)
	models, err := parseCatalog(body)
	if err != nil {
		t.Fatalf("parseCatalog: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("kept %d models, want 2: %+v", len(models), models)
	}
	byID := map[string]raccoonModel{}
	for _, m := range models {
		byID[m.ID] = m
	}
	if got := byID["sn-glm-5-3"].Name; got != "GLM-5-3 · x1→x0.75" {
		t.Errorf("discounted name = %q, want the base→effective arrow", got)
	}
	if got := byID["free-model"].Name; got != "Free · 免费" {
		t.Errorf("zero effective multiplier name = %q, want a 免费 suffix", got)
	}
	if byID["sn-glm-5-3"].ContextLength != 1000000 {
		t.Errorf("context length = %d, want 1000000", byID["sn-glm-5-3"].ContextLength)
	}
}

func TestParseCatalogAcceptsBareArray(t *testing.T) {
	body := []byte(`[{"id":"a","name":"A","context_length":1000}]`)
	models, err := parseCatalog(body)
	if err != nil {
		t.Fatalf("parseCatalog: %v", err)
	}
	if len(models) != 1 || models[0].ID != "a" || models[0].ContextLength != 1000 {
		t.Fatalf("models = %+v", models)
	}
}

func TestFormatMultiplierTrimsTrailingZeros(t *testing.T) {
	cases := map[float64]string{
		1.0:    "1",
		0.75:   "0.75",
		12.0:   "12",
		0.125:  "0.125",
		3.3333: "3.3333",
	}
	for in, want := range cases {
		if got := formatMultiplier(in); got != want {
			t.Errorf("formatMultiplier(%v) = %q, want %q", in, got, want)
		}
	}
}

// TestAggregateSSEMergesToolCallFragments is the accumulation contract: empty
// names must not overwrite a parsed one, arguments concatenate, and an empty
// stream is an error rather than an empty answer.
func TestAggregateSSEMergesToolCallFragments(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"c1","created":99,"choices":[{"delta":{"role":"assistant","content":"Hi"},"finish_reason":null}]}`,
		`data: {"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_9","type":"function","function":{"name":"calc","arguments":"{\"a\""}}]},"finish_reason":null}]}`,
		`data: {"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"","arguments":":1}"}}]},"finish_reason":null}]}`,
		`data: {"id":"c1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n\n")

	out, err := aggregateSSE(strings.NewReader(stream), "sn-glm-5-3")
	if err != nil {
		t.Fatalf("aggregateSSE: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		`"content":"Hi"`, `"id":"call_9"`, `"name":"calc"`,
		`"arguments":"{\"a\":1}"`, `"finish_reason":"tool_calls"`, `"created":99`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in:\n%s", want, s)
		}
	}
	if strings.Contains(s, `"name":""`) {
		t.Errorf("empty name overwrote the parsed tool name:\n%s", s)
	}
}

func TestAggregateSSEAcceptsDataWithoutSpace(t *testing.T) {
	// The reference notes the upstream sometimes omits the space after "data:".
	stream := "data:{\"choices\":[{\"delta\":{\"content\":\"x\"},\"finish_reason\":\"stop\"}]}\n\ndata:[DONE]\n\n"
	out, err := aggregateSSE(strings.NewReader(stream), "m")
	if err != nil {
		t.Fatalf("aggregateSSE: %v", err)
	}
	if !strings.Contains(string(out), `"content":"x"`) {
		t.Fatalf("no-space frame was not parsed: %s", out)
	}
}

func TestAggregateSSENoFramesIsAnError(t *testing.T) {
	if _, err := aggregateSSE(strings.NewReader(": keep-alive\n\n"), "m"); err == nil {
		t.Fatal("aggregateSSE accepted a stream with no data frames; want an error")
	}
}

func TestFileStoreRoundTripsSealed(t *testing.T) {
	dir := t.TempDir()
	first, err := newStore(dir)
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	if first.configured() {
		t.Fatal("a fresh store must report not configured")
	}
	if err := first.set(credentials{AccessToken: "tok-secret", RefreshToken: "rt", OfficeIdentity: "org"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	second, err := newStore(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	creds, ok := second.get()
	if !ok || creds.AccessToken != "tok-secret" || creds.RefreshToken != "rt" || creds.OfficeIdentity != "org" {
		t.Fatalf("reload = %+v ok=%v", creds, ok)
	}
	if second.sealKind != "dpapi-user" {
		t.Fatalf("sealKind = %q, want dpapi-user on Windows", second.sealKind)
	}
	raw, err := os.ReadFile(second.path)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	if strings.Contains(string(raw), "tok-secret") {
		t.Fatal("store file contains the access token in plaintext")
	}
	if err := second.set(credentials{AccessToken: ""}); err == nil {
		t.Fatal("set accepted an empty token; that would wipe a working login")
	}
}

func TestRefreshableReflectsStoredRefreshToken(t *testing.T) {
	dir := t.TempDir()
	st, err := newStore(dir)
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	srv := &server{cfg: pluginConfig{ID: "raccoon", APILevel: 2, Token: "tok"}, store: st, client: newUpstreamClient()}

	// No refresh token stored -> not refreshable.
	if err := st.set(credentials{AccessToken: "a"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if _, ok := st.get(); !ok {
		t.Fatal("store should be configured")
	}
	creds, _ := st.get()
	if strings.TrimSpace(creds.RefreshToken) != "" {
		t.Fatal("test setup: expected no refresh token")
	}

	// With one -> refreshable, which is what /accounts must report.
	if err := st.set(credentials{AccessToken: "a", RefreshToken: "r"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	rec := doReq(t, srv.routes(), "/accounts")
	if rec.Code != http.StatusOK {
		t.Fatalf("accounts status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"refreshable":true`) {
		t.Fatalf("accounts did not report refreshable:true: %s", rec.Body.String())
	}
}

func TestAccountsEmptyUntilConfigured(t *testing.T) {
	dir := t.TempDir()
	st, err := newStore(dir)
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	srv := &server{cfg: pluginConfig{ID: "raccoon", APILevel: 2, Token: "tok"}, store: st, client: newUpstreamClient()}

	rec := doReq(t, srv.routes(), "/accounts")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"accounts":[]`) {
		t.Fatalf("unconfigured accounts should be empty: %s", rec.Body.String())
	}

	if err := st.set(credentials{AccessToken: "a"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	rec = doReq(t, srv.routes(), "/accounts")
	if strings.Contains(rec.Body.String(), `"accounts":[]`) {
		t.Fatalf("configured accounts should report one entry: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"credential":{"present":true`) {
		t.Fatalf("account must carry credential.present=true or the scheduler filters it: %s", rec.Body.String())
	}
	// quota must stay unstated so the scheduler's credit filter does not park it.
	if strings.Contains(rec.Body.String(), `"total":`) || strings.Contains(rec.Body.String(), `"remaining":`) {
		t.Fatalf("quota must not carry numbers: %s", rec.Body.String())
	}
}

func TestHealthIsUnauthenticatedAndOthersAreNot(t *testing.T) {
	dir := t.TempDir()
	st, err := newStore(dir)
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	h := (&server{cfg: pluginConfig{ID: "raccoon", APILevel: 2, Token: "tok"}, store: st, client: newUpstreamClient()}).routes()

	if rec := doReqNoAuth(t, h, "/health"); rec.Code != http.StatusOK {
		t.Fatalf("/health without token = %d, want 200", rec.Code)
	}
	if !strings.Contains(doReqNoAuth(t, h, "/health").Body.String(), `"ok":true`) {
		t.Error("/health must report ok:true")
	}
	if rec := doReqNoAuth(t, h, "/accounts"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("/accounts without token = %d, want 401", rec.Code)
	}
	if rec := doReqNoAuth(t, h, "/v1/models"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("/v1/models without token = %d, want 401", rec.Code)
	}
}
