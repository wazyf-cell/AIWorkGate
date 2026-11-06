package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestChatHeadersMatchReference pins lobsteraiChatHeaders: bearer + Accept with
// both event-stream and application/json, User-Agent, and the two
// X-LobsterAI-* headers. The capabilities header is load-bearing — the reference
// states the server filters the model list by it, and chat is where the client
// declares what it can handle.
func TestChatHeadersMatchReference(t *testing.T) {
	var got http.Header
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, gotPath = r.Header.Clone(), r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	resp, err := newUpstreamClient().chat(t.Context(), srv.URL, "tok-abc", "2026.9.4", []byte(`{"model":"m","stream":true}`), "")
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	defer resp.Body.Close()

	if gotPath != chatPath {
		t.Errorf("chat path = %q, want %q", gotPath, chatPath)
	}
	if v := got.Get("Authorization"); v != "Bearer tok-abc" {
		t.Errorf("Authorization = %q", v)
	}
	if v := got.Get("Accept"); v != "text/event-stream, application/json" {
		t.Errorf("Accept = %q, want the reference's dual accept", v)
	}
	if v := got.Get("User-Agent"); v != userAgent {
		t.Errorf("User-Agent = %q, want %q", v, userAgent)
	}
	if v := got.Get("X-LobsterAI-Client-Capabilities"); v != clientCapabilities {
		t.Errorf("X-LobsterAI-Client-Capabilities = %q, want %q", v, clientCapabilities)
	}
	if v := got.Get("X-LobsterAI-Client-Version"); v != "2026.9.4" {
		t.Errorf("X-LobsterAI-Client-Version = %q", v)
	}
}

// TestModelsQueryCarriesKeyfromIdentity is the models-endpoint contract: the
// identity payload travels as a QUERY STRING (firstKeyfrom/latestKeyfrom/
// version/uuid/userId) and the capabilities header must be present, because the
// server filters the model list by it.
func TestModelsQueryCarriesKeyfromIdentity(t *testing.T) {
	var gotQuery, gotCaps string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		gotCaps = r.Header.Get("X-LobsterAI-Client-Capabilities")
		_, _ = w.Write([]byte(`{"code":0,"data":[{"modelId":"kimi-k3","modelName":"Kimi K3","contextWindow":131072}]}`))
	}))
	defer srv.Close()

	cred := credentials{
		AccessToken:   "tok",
		FirstKeyfrom:  "111",
		LatestKeyfrom: "222",
		UUID:          "uuid-1",
		UserID:        "user-1",
	}
	body, err := newUpstreamClient().models(t.Context(), srv.URL, "tok", "2026.9.4", cred, "")
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	for _, want := range []string{"firstKeyfrom=111", "latestKeyfrom=222", "version=2026.9.4", "uuid=uuid-1", "userId=user-1"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query missing %q: %s", want, gotQuery)
		}
	}
	if gotCaps != clientCapabilities {
		t.Errorf("capabilities header = %q; without it the server drops kimi-k3", gotCaps)
	}
	models, err := parseModelsBody(body)
	if err != nil {
		t.Fatalf("parseModelsBody: %v", err)
	}
	if len(models) != 1 || models[0].ID != "kimi-k3" || models[0].ContextLength != 131072 {
		t.Fatalf("models = %+v", models)
	}
}

// TestParseModelsBodyUsesModelIDField pins the field names: the catalog uses
// modelId/modelName, NOT the OpenAI id/name. Reading the wrong field yields an
// empty list, which looks like "no models" rather than a bug.
func TestParseModelsBodyUsesModelIDField(t *testing.T) {
	body := []byte(`{"code":0,"data":[
		{"modelId":"glm-5.2","modelName":"GLM 5.2","contextWindow":131072,"costMultiplier":0.5},
		{"modelId":"glm-5v-turbo","modelName":"GLM 5V","contextWindow":131072,"supportsImage":true},
		{"modelId":"vendor/x","modelName":"dropped"},
		{"modelId":"","modelName":"dropped too"},
		{"id":"openai-shaped","name":"must be ignored"}
	]}`)
	models, err := parseModelsBody(body)
	if err != nil {
		t.Fatalf("parseModelsBody: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("kept %d models, want 2: %+v", len(models), models)
	}
	byID := map[string]lobsteraiModel{}
	for _, m := range models {
		byID[m.ID] = m
	}
	if _, ok := byID["openai-shaped"]; ok {
		t.Error("a row using id/name instead of modelId/modelName must not be published")
	}
	if got := byID["glm-5.2"].Name; got != "GLM 5.2 · x0.5" {
		t.Errorf("cost multiplier suffix = %q, want 'GLM 5.2 · x0.5'", got)
	}
	if !byID["glm-5v-turbo"].Vision {
		t.Error("supportsImage must map to vision")
	}
}

// TestParseModelsBodyRejectsNonZeroCode covers the envelope guard: a non-zero
// code means the payload is an error, not a model list.
func TestParseModelsBodyRejectsNonZeroCode(t *testing.T) {
	if _, err := parseModelsBody([]byte(`{"code":401,"msg":"bad token"}`)); err == nil {
		t.Fatal("a non-zero envelope code must be an error")
	}
}

func TestParseModelsBodyAcceptsNestedData(t *testing.T) {
	body := []byte(`{"code":0,"data":{"data":[{"modelId":"deepseek-v4-pro","contextWindow":131072}]}}`)
	models, err := parseModelsBody(body)
	if err != nil {
		t.Fatalf("parseModelsBody: %v", err)
	}
	if len(models) != 1 || models[0].ID != "deepseek-v4-pro" {
		t.Fatalf("models = %+v", models)
	}
}

// TestForceStreamIsUnconditional pins the hard constraint: the reference states
// stream:false makes the upstream return 500, so the flag is forced regardless of
// what the client asked for.
func TestForceStreamIsUnconditional(t *testing.T) {
	in := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":false,"temperature":0.2}`)
	out, err := forceStream(in)
	if err != nil {
		t.Fatalf("forceStream: %v", err)
	}
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if string(parsed["stream"]) != "true" {
		t.Errorf("stream = %s, want true (stream:false returns 500 upstream)", parsed["stream"])
	}
	if string(parsed["temperature"]) != "0.2" {
		t.Errorf("temperature lost: %s", out)
	}
	if streamRequested(in) {
		t.Error("streamRequested reported true for stream:false")
	}
}

// TestAggregateSSEAcceptsDataWithoutSpace covers the upstream quirk the reference
// documents: "龙虾上游实测无空格".
func TestAggregateSSEAcceptsDataWithoutSpace(t *testing.T) {
	stream := "data:{\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"x\"},\"finish_reason\":\"stop\"}]}\n\ndata:[DONE]\n\n"
	out, err := aggregateSSE(strings.NewReader(stream), "glm-5.2")
	if err != nil {
		t.Fatalf("aggregateSSE: %v", err)
	}
	if !strings.Contains(string(out), `"content":"x"`) {
		t.Fatalf("no-space frame not parsed: %s", out)
	}
}

// TestAggregateSSEMergesToolCallsAndReasoning covers the two accumulation rules:
// reasoning_content becomes its own block, and tool fragments merge by index with
// a non-empty name never overwritten.
func TestAggregateSSEMergesToolCallsAndReasoning(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"c1","created":7,"choices":[{"delta":{"role":"assistant","reasoning_content":"thinking..."},"finish_reason":null}]}`,
		`data: {"id":"c1","choices":[{"delta":{"content":"Done"},"finish_reason":null}]}`,
		`data: {"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"search","arguments":"{\"q\""}}]},"finish_reason":null}]}`,
		`data: {"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"","arguments":":\"x\"}"}}]},"finish_reason":null}]}`,
		`data: {"id":"c1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n\n")

	out, err := aggregateSSE(strings.NewReader(stream), "glm-5.2")
	if err != nil {
		t.Fatalf("aggregateSSE: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		`"reasoning_content":"thinking..."`, `"content":"Done"`,
		`"id":"call_1"`, `"name":"search"`, `"arguments":"{\"q\":\"x\"}"`,
		`"finish_reason":"tool_calls"`, `"created":7`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in:\n%s", want, s)
		}
	}
	if strings.Contains(s, `"name":""`) {
		t.Errorf("empty name overwrote the parsed tool name:\n%s", s)
	}
}

func TestAggregateSSENoFramesIsAnError(t *testing.T) {
	if _, err := aggregateSSE(strings.NewReader(": ping\n\n"), "m"); err == nil {
		t.Fatal("aggregateSSE accepted a stream with no data frames; want an error")
	}
}

// TestClientVersionParsing covers parseClientVersionFromUpdate's shape
// ({data:{value:{version}}}) and the version grammar.
func TestClientVersionParsing(t *testing.T) {
	body := []byte(`{"data":{"value":{"version":"2026.10.1"}}}`)
	if got := parseClientVersionFromUpdate(body); got != "2026.10.1" {
		t.Errorf("parsed version = %q", got)
	}
	if got := parseClientVersionFromUpdate([]byte(`{"data":{}}`)); got != "" {
		t.Errorf("missing version should be empty, got %q", got)
	}
	valid := []string{"2026.9.4", "1.2.3", "2026.9.4-beta"}
	for _, v := range valid {
		if parseClientVersion(v) == "" {
			t.Errorf("parseClientVersion(%q) rejected a valid version", v)
		}
	}
	invalid := []string{"", "v1.2.3", "abc", "-1.0.0"}
	for _, v := range invalid {
		if parseClientVersion(v) != "" {
			t.Errorf("parseClientVersion(%q) accepted an invalid version", v)
		}
	}
}

// TestExchangePostsReferenceBody pins the exchange body fields the reference
// sends: authCode + firstKeyfrom + latestKeyfrom + uuid + version.
func TestExchangePostsReferenceBody(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != exchangePath {
			t.Errorf("exchange path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("exchange must be anonymous, got Authorization %q", got)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"at","refreshToken":"rt","expiresIn":3600,"user":{"id":"u1","yid":"y1","userId":"au1","nickname":"13800001111"}}}`))
	}))
	defer srv.Close()

	creds, err := newUpstreamClient().exchangeAuthCode(t.Context(), srv.URL, "CODE-123",
		loginSession{UUID: "uuid-x", FirstKeyfrom: "1000", LatestKeyfrom: "2000"}, "2026.9.4", "")
	if err != nil {
		t.Fatalf("exchangeAuthCode: %v", err)
	}
	for k, want := range map[string]string{
		"authCode": "CODE-123", "firstKeyfrom": "1000", "uuid": "uuid-x", "version": "2026.9.4",
	} {
		if got[k] != want {
			t.Errorf("body[%s] = %q, want %q (body=%v)", k, got[k], want, got)
		}
	}
	if got["latestKeyfrom"] == "" {
		t.Error("latestKeyfrom must be the current timestamp at exchange time")
	}
	if creds.AccessToken != "at" || creds.RefreshToken != "rt" || creds.UserID != "u1" {
		t.Errorf("credentials = %+v", creds)
	}
	// The session's keyfrom tuple is frozen onto the credential — the reference
	// matches Go's KeyfromBody, which reads the stored LatestKeyfrom and never
	// updates it on refresh.
	if creds.FirstKeyfrom != "1000" || creds.LatestKeyfrom != "2000" || creds.UUID != "uuid-x" {
		t.Errorf("keyfrom tuple not carried from the session: %+v", creds)
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
	if err := first.set(credentials{AccessToken: "tok-secret", RefreshToken: "rt", FirstKeyfrom: "1", UUID: "u"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	second, err := newStore(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	creds, ok := second.get()
	if !ok || creds.AccessToken != "tok-secret" || creds.RefreshToken != "rt" || creds.FirstKeyfrom != "1" {
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
	if err := second.set(credentials{AccessToken: " "}); err == nil {
		t.Fatal("set accepted an empty token; that would wipe a working login")
	}
}

func TestLoginStartReturnsRealPortalURL(t *testing.T) {
	dir := t.TempDir()
	st, err := newStore(dir)
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	srv := &server{cfg: pluginConfig{ID: "lobsterai", APILevel: 2, Port: "18800", Token: "tok"}, store: st, client: newUpstreamClient()}

	rec := doReqToken(t, srv.routes(), http.MethodPost, "/accounts/login", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("login start = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		LoginURL     string `json:"login_url"`
		RedirectURI  string `json:"redirect_uri"`
		SessionID    string `json:"session_id"`
		FirstKeyfrom string `json:"first_keyfrom"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("login body is not JSON: %v", err)
	}
	// The URL is built exactly as buildLobsteraiLoginUrl: portal + /portal#/login?source=electron&...
	if !strings.HasPrefix(payload.LoginURL, portalBase+"/portal#/login?") {
		t.Errorf("login_url = %q, want the portal login page", payload.LoginURL)
	}
	for _, want := range []string{"source=electron", "redirect_uri=", "state="} {
		if !strings.Contains(payload.LoginURL, want) {
			t.Errorf("login_url missing %q: %s", want, payload.LoginURL)
		}
	}
	if payload.RedirectURI != "http://127.0.0.1:18800/auth/callback" {
		t.Errorf("redirect_uri = %q, want the plugin's own callback path", payload.RedirectURI)
	}
	if payload.SessionID == "" || payload.FirstKeyfrom == "" {
		t.Errorf("session_id/first_keyfrom must be present: %+v", payload)
	}
}

func TestAccountsAndAuthBoundary(t *testing.T) {
	dir := t.TempDir()
	st, err := newStore(dir)
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	srv := &server{cfg: pluginConfig{ID: "lobsterai", APILevel: 2, Token: "tok"}, store: st, client: newUpstreamClient()}
	h := srv.routes()

	// /health is public; everything else is not.
	if rec := doReqNoAuth(t, h, http.MethodGet, "/health"); rec.Code != http.StatusOK {
		t.Fatalf("/health without token = %d, want 200", rec.Code)
	}
	if rec := doReqNoAuth(t, h, http.MethodGet, "/accounts"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("/accounts without token = %d, want 401", rec.Code)
	}

	rec := doReqToken(t, h, http.MethodGet, "/accounts", "")
	if !strings.Contains(rec.Body.String(), `"accounts":[]`) {
		t.Fatalf("unconfigured accounts should be empty: %s", rec.Body.String())
	}
	if err := st.set(credentials{AccessToken: "a", RefreshToken: "r", Nickname: "13800001111"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	rec = doReqToken(t, h, http.MethodGet, "/accounts", "")
	body := rec.Body.String()
	if strings.Contains(body, `"accounts":[]`) {
		t.Fatalf("configured accounts should report one entry: %s", body)
	}
	if !strings.Contains(body, `"credential":{"present":true,"refreshable":true}`) {
		t.Fatalf("credential flags wrong: %s", body)
	}
	// A phone-like nickname must be masked in the label, never shown in full.
	// maskLobsteraiPhoneTail keeps the first 3 and the last 2 characters, and
	// fills the gap with stars: 11 - 3 - 2 = 6.
	if strings.Contains(body, "13800001111") {
		t.Fatalf("phone-like nickname leaked unmasked: %s", body)
	}
	if !strings.Contains(body, "138******11") {
		t.Fatalf("expected a masked label, got: %s", body)
	}
	// quota must stay unstated so the scheduler's credit filter does not park it.
	if strings.Contains(body, `"total":`) || strings.Contains(body, `"remaining":`) {
		t.Fatalf("quota must not carry numbers: %s", body)
	}
}
