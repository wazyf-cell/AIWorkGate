package main

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The gate is the part most likely to break silently: a body that looks fine
// still gets 403 FreeTierError if the shape is wrong, and no unit test of the
// response side would notice. These assertions are the measured facts from the
// live endpoint (2026-10-21) turned into regression guards.

func TestBuildPayloadForcesStreamTrue(t *testing.T) {
	out, _, err := BuildPayload([]byte(`{"model":"big-pickle","stream":false,"messages":[{"role":"user","content":"hi"}]}`), nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The gate: a non-streaming request was measured to get 403 FreeTierError.
	if got["stream"] != true {
		t.Fatalf("stream = %v, want true (the free tier gate)", got["stream"])
	}
}

func TestBuildPayloadInjectsOfficialGateTools(t *testing.T) {
	out, _, err := BuildPayload([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`), nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var got struct {
		Tools  []oaTool `json:"tools"`
		System string   `json:"system"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	names := map[string]bool{}
	for _, tool := range got.Tools {
		names[tool.Function.Name] = true
	}
	for _, want := range []string{"read", "shell"} {
		if !names[want] {
			t.Fatalf("gate tool %q missing from %v", want, names)
		}
	}
	if len(got.Tools) != 2 {
		t.Fatalf("tools = %d, want exactly the two gate tools", len(got.Tools))
	}
	// The injected placeholders need a note, or the model tries to call them.
	if !strings.Contains(got.System, "placeholders that do nothing") {
		t.Fatalf("system note missing: %q", got.System)
	}
}

func TestBuildPayloadRenamesCallerGateToolAndRestoresIt(t *testing.T) {
	// A caller tool that reuses the gate name but not the official definition
	// must be moved aside, and the response side must put the name back.
	in := []byte(`{
		"model":"m",
		"messages":[{"role":"user","content":"go"}],
		"tools":[
			{"type":"function","function":{"name":"read","description":"my own read","parameters":{"type":"object","properties":{}}}},
			{"type":"function","function":{"name":"lookup","description":"d","parameters":{"type":"object","properties":{}}}}
		]
	}`)
	out, aliases, err := BuildPayload(in, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var got struct {
		Tools []oaTool `json:"tools"`
	}
	_ = json.Unmarshal(out, &got)
	names := []string{}
	for _, tool := range got.Tools {
		names = append(names, tool.Function.Name)
	}
	if containsString(names, "read") && len(aliases) == 0 {
		t.Fatalf("a non-official read kept the gate name: %v", names)
	}
	if !containsString(names, "read_client") {
		t.Fatalf("caller read was not aliased: %v", names)
	}
	if aliases["read_client"] != "read" {
		t.Fatalf("aliases = %v, want read_client -> read", aliases)
	}
	// The official definitions must still be present under their own names.
	if !containsString(names, "shell") {
		t.Fatalf("gate shell missing: %v", names)
	}
}

func TestBuildPayloadKeepsOfficialGateToolAsIs(t *testing.T) {
	// A caller that already sends the exact official definition must not get a
	// duplicate injected (the upstream rejects duplicates of the gate tools).
	official, err := json.Marshal(officialGateTool("read"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	in := []byte(`{"model":"m","messages":[{"role":"user","content":"go"}],"tools":[` + string(official) + `]}`)
	out, aliases, err := BuildPayload(in, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(aliases) != 0 {
		t.Fatalf("official read was aliased: %v", aliases)
	}
	var got struct {
		Tools []oaTool `json:"tools"`
	}
	_ = json.Unmarshal(out, &got)
	count := 0
	for _, tool := range got.Tools {
		if tool.Function.Name == "read" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("read declared %d times, want exactly 1", count)
	}
}

func TestBuildPayloadDropsBadMaxTokensAndKeepsGood(t *testing.T) {
	// 0 / negative make the gateway reject the whole turn, so they must not be
	// forwarded; a positive value must survive.
	cases := []struct {
		name string
		body string
		want any
	}{
		{"zero dropped", `{"model":"m","max_tokens":0,"messages":[{"role":"user","content":"x"}]}`, nil},
		{"negative dropped", `{"model":"m","max_tokens":-5,"messages":[{"role":"user","content":"x"}]}`, nil},
		{"positive kept", `{"model":"m","max_tokens":512,"messages":[{"role":"user","content":"x"}]}`, float64(512)},
		{"max_completion fallback", `{"model":"m","max_completion_tokens":256,"messages":[{"role":"user","content":"x"}]}`, float64(256)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, _, err := BuildPayload([]byte(tc.body), nil)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			var got map[string]any
			_ = json.Unmarshal(out, &got)
			if got["max_tokens"] != tc.want {
				t.Fatalf("max_tokens = %v, want %v", got["max_tokens"], tc.want)
			}
		})
	}
}

// The free tier answers a body carrying tool_choice "none" with 403 (the
// reference deletes the key before sending). Both spellings must disappear;
// any other value must survive unchanged.
func TestBuildPayloadDropsToolChoiceNone(t *testing.T) {
	for _, choice := range []string{`"none"`, `{"type":"none"}`} {
		body := `{"model":"m","messages":[{"role":"user","content":"x"}],"tool_choice":` + choice + `}`
		out, _, err := BuildPayload([]byte(body), nil)
		if err != nil {
			t.Fatalf("build %s: %v", choice, err)
		}
		var got map[string]any
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if v, ok := got["tool_choice"]; ok {
			t.Fatalf("tool_choice %s reached the upstream as %v, want the key removed", choice, v)
		}
	}
}

func TestBuildPayloadKeepsOtherToolChoices(t *testing.T) {
	cases := []struct{ name, choice string }{
		{"auto", `"auto"`},
		{"required", `"required"`},
		{"named function", `{"type":"function","function":{"name":"lookup"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"m","messages":[{"role":"user","content":"x"}],"tool_choice":` + tc.choice + `}`
			out, _, err := BuildPayload([]byte(body), nil)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			v, ok := got["tool_choice"]
			if !ok {
				t.Fatalf("tool_choice %s was dropped, want it passed through", tc.choice)
			}
			var want any
			if err := json.Unmarshal([]byte(tc.choice), &want); err != nil {
				t.Fatalf("decode want: %v", err)
			}
			if !reflect.DeepEqual(v, want) {
				t.Fatalf("tool_choice = %v, want %v", v, want)
			}
		})
	}
}

func TestTranslateEffort(t *testing.T) {
	declared := []string{"low", "medium", "high"}
	// exact passthrough
	if got, exact := TranslateEffort("high", declared); got != "high" || !exact {
		t.Fatalf("exact = %q/%v, want high/true", got, exact)
	}
	// xhigh is unknown to this model: nearest is high (7 -> 5) rather than max.
	if got, exact := TranslateEffort("xhigh", declared); got != "high" || exact {
		t.Fatalf("mapped = %q/%v, want high/false", got, exact)
	}
	// minimal (rank 1) maps to low (rank 3), not to high.
	if got, _ := TranslateEffort("minimal", declared); got != "low" {
		t.Fatalf("minimal = %q, want low", got)
	}
	// A model with no declared levels gets no effort at all.
	if got, _ := TranslateEffort("high", nil); got != "" {
		t.Fatalf("no levels = %q, want empty", got)
	}
	// "none" must not be mapped onto a thinking level: the off/non-off split
	// is what the reference's translateReasoningEffort enforces.
	if got, _ := TranslateEffort("none", declared); got != "" {
		t.Fatalf("none = %q, want empty (no off level declared)", got)
	}
}

func TestBuildPayloadTranslatesEffortForDeclaredModel(t *testing.T) {
	caps := []Capability{{ID: "m", Efforts: []string{"low", "high"}}}
	out, _, err := BuildPayload([]byte(`{"model":"m","reasoning_effort":"max","messages":[{"role":"user","content":"x"}]}`), caps)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	// max (rank 8) is not declared; high (rank 5) is the nearest declared level
	// and must be sent instead of the illegal value.
	if got["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %v, want high", got["reasoning_effort"])
	}
}

func TestConsumeFoldsStreamAndRestoresToolNames(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"abc","model":"big-pickle","created":1792598905,"choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		``,
		`data: {"choices":[{"index":0,"delta":{"content":"po"}}]}`,
		``,
		`data: {"choices":[{"index":0,"delta":{"content":"ng"}}]}`,
		``,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read_client","arguments":"{\"path\":"}}]}}]}`,
		``,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.txt\"}"}}]}}]}`,
		``,
		`data: {"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":3,"total_tokens":12}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	c, err := Consume(strings.NewReader(sse), map[string]string{"read_client": "read"})
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	out, err := c.JSON("big-pickle")
	if err != nil {
		t.Fatalf("json: %v", err)
	}
	var got struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
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
		Usage struct {
			Prompt     int `json:"prompt_tokens"`
			Completion int `json:"completion_tokens"`
			Total      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Object != "chat.completion" {
		t.Fatalf("object = %q", got.Object)
	}
	if got.ID != "abc" {
		t.Fatalf("id = %q, want the upstream id", got.ID)
	}
	if got.Choices[0].Message.Content != "pong" {
		t.Fatalf("content = %q, want pong", got.Choices[0].Message.Content)
	}
	tc := got.Choices[0].Message.ToolCalls
	if len(tc) != 1 {
		t.Fatalf("tool_calls = %d, want 1", len(tc))
	}
	// The alias must be undone: the caller only knows "read".
	if tc[0].Function.Name != "read" {
		t.Fatalf("tool name = %q, want read", tc[0].Function.Name)
	}
	// Streamed argument fragments must be concatenated, not kept apart.
	if tc[0].Function.Arguments != `{"path":"a.txt"}` {
		t.Fatalf("arguments = %q", tc[0].Function.Arguments)
	}
	if got.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", got.Choices[0].FinishReason)
	}
	if got.Usage.Prompt != 9 || got.Usage.Completion != 3 || got.Usage.Total != 12 {
		t.Fatalf("usage = %+v", got.Usage)
	}
}

func TestClassifyUpstreamErrorNamesFreeTier(t *testing.T) {
	body := `{"type":"error","error":{"type":"FreeTierError","message":"OpenCode's free tier can only be used from within OpenCode."}}`
	_, kind, message := classifyUpstreamError(403, body)
	if kind != "free_tier" {
		t.Fatalf("kind = %q, want free_tier", kind)
	}
	// The advice must name the shape, because a key or proxy change cannot fix it.
	if !strings.Contains(message, "stream=true") {
		t.Fatalf("message does not name the shape: %q", message)
	}
}

func TestClassifyUpstreamErrorRateLimitIs429(t *testing.T) {
	status, kind, _ := classifyUpstreamError(429, `{"error":{"message":"Too many requests"}}`)
	if status != 429 || kind != "rate_limit" {
		t.Fatalf("got %d/%q, want 429/rate_limit", status, kind)
	}
	// A 5xx keeps its upstream status so the host can distinguish it.
	status, kind, _ = classifyUpstreamError(503, `upstream unavailable`)
	if status != 503 || kind != "server" {
		t.Fatalf("got %d/%q, want 503/server", status, kind)
	}
}

func TestParseModelsDevProjectsCapabilities(t *testing.T) {
	body := []byte(`{
		"opencode": {"models": {
			"free-vision": {"name":"Free Vision","modalities":{"input":["text","image"]},"limit":{"context":200000,"output":32000},"reasoning":true,"reasoning_options":[{"type":"effort","values":["low","high"]}],"tool_call":true,"cost":{"input":0,"output":0}},
			"paid-text": {"name":"Paid","modalities":{"input":["text"]},"limit":{"context":128000},"reasoning":true,"reasoning_options":[],"tool_call":false,"cost":{"input":2,"output":8}},
			"longcat-2.5-preview-free": {"modalities":{"input":["text","image"]},"cost":{"input":0,"output":0}}
		}}
	}`)
	entries, err := parseModelsDev(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	byID := map[string]Capability{}
	for _, e := range entries {
		byID[e.ID] = e
	}
	free := byID["free-vision"]
	if !free.SupportsImage() || free.Context != 200000 || !free.IsFree || !free.Reasoning {
		t.Fatalf("free-vision = %+v", free)
	}
	if len(free.Efforts) != 2 {
		t.Fatalf("efforts = %v", free.Efforts)
	}
	paid := byID["paid-text"]
	if paid.IsFree || paid.Reasoning {
		// reasoning must be false because it declares no levels: claiming true
		// with an empty list puts an empty selector in the UI.
		t.Fatalf("paid-text = %+v", paid)
	}
	// The measured override must win over the catalog, which gets this one wrong.
	lc := byID["longcat-2.5-preview-free"]
	if lc.SupportsImage() {
		t.Fatalf("longcat must be text-only by measurement: %+v", lc)
	}
}

func TestParseModelsDevRejectsMissingEntry(t *testing.T) {
	if _, err := parseModelsDev([]byte(`{"other":{"models":{}}}`)); err == nil {
		t.Fatal("expected an error when the opencode entry is absent")
	}
}

func TestProjectIDMatchesReferenceFormula(t *testing.T) {
	// Computed with the reference's deriveProjectId (node crypto):
	// sha1("git-remote:opencode/" + sha256("public 0")).
	const want = "9e2c575aff0ab741017172ce36629f03016821b1"
	if got := projectID("public", 0); got != want {
		t.Fatalf("projectID(public,0) = %q, want %q", got, want)
	}
	// Rotating the generation must change the fingerprint.
	if projectID("public", 1) == want {
		t.Fatal("generation 1 produced the same fingerprint")
	}
}

func TestSessionIDShape(t *testing.T) {
	id := sessionID()
	if len(id) != len("ses_")+sessionTailLength {
		t.Fatalf("session id %q has length %d, want %d", id, len(id), len("ses_")+sessionTailLength)
	}
	if !strings.HasPrefix(id, "ses_") {
		t.Fatalf("session id %q must start with ses_", id)
	}
	// The first 12 characters after the prefix are lowercase hex.
	hexPart := id[len("ses_"):][:12]
	for _, r := range hexPart {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Fatalf("hex segment %q contains %q", hexPart, r)
		}
	}
	if sessionID() == id {
		t.Fatal("two derived session ids are identical")
	}
}

func TestUserAgentForVersion(t *testing.T) {
	// v2 and up maps onto the accepted shape.
	if got := userAgentForVersion("2.0.26"); got != "opencode/latest/2.0.26/cli" {
		t.Fatalf("2.0.26 -> %q", got)
	}
	if got := userAgentForVersion("3.1.4"); got != "opencode/latest/3.1.4/cli" {
		t.Fatalf("3.1.4 -> %q", got)
	}
	// A major below 2 is rejected upstream, so the caller must keep the default.
	if got := userAgentForVersion("1.18.22"); got != "" {
		t.Fatalf("1.18.22 -> %q, want empty", got)
	}
	if got := userAgentForVersion("garbage"); got != "" {
		t.Fatalf("garbage -> %q, want empty", got)
	}
}

func TestEffectiveUserAgentOnlyHonoursV2Shape(t *testing.T) {
	creds := credentials{APIKey: "public"}
	if got := creds.effectiveUserAgent(); got != defaultUserAgent {
		t.Fatalf("default = %q", got)
	}
	creds.UserAgent = "opencode/1.18.22"
	if got := creds.effectiveUserAgent(); got != defaultUserAgent {
		t.Fatalf("a v1 override must be ignored, got %q", got)
	}
	creds.UserAgent = "opencode/latest/2.1.0/cli"
	if got := creds.effectiveUserAgent(); got != "opencode/latest/2.1.0/cli" {
		t.Fatalf("a v2 override must be honoured, got %q", got)
	}
}

func TestChatHeadersCarryFourSameSessionValues(t *testing.T) {
	sid := "ses_0123456789abABCDEFGHIJKLMN"
	headers := chatHeaders("proj", sid, defaultUserAgent)
	got := map[string]string{}
	for _, kv := range headers {
		got[kv[0]] = kv[1]
	}
	// The reference is explicit: these four must all carry the same value, and
	// missing any one is a 403.
	for _, name := range []string{"x-opencode-session", "x-opencode-session-id", "x-session-affinity", "x-session-id"} {
		if got[name] != sid {
			t.Fatalf("%s = %q, want %q", name, got[name], sid)
		}
	}
	if got["x-opencode-project"] != "proj" {
		t.Fatalf("project = %q", got["x-opencode-project"])
	}
	if got["x-opencode-client"] != "cli" {
		t.Fatalf("client = %q, want cli", got["x-opencode-client"])
	}
	if got["user-agent"] != defaultUserAgent {
		t.Fatalf("user-agent = %q", got["user-agent"])
	}
}

func TestContainsImagePart(t *testing.T) {
	withImage := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}]}]}`)
	if !containsImagePart(withImage) {
		t.Fatal("image part not detected")
	}
	plain := []byte(`{"messages":[{"role":"user","content":"just text"}]}`)
	if containsImagePart(plain) {
		t.Fatal("plain text misdetected as an image")
	}
}

func TestCheckImageSupportRejectsUnsupportedModel(t *testing.T) {
	caps := []Capability{
		{ID: "vision", Modalities: []string{"text", "image"}},
		{ID: "blind", Modalities: []string{"text"}},
	}
	img := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}]}]}`)
	if err := checkImageSupport("vision", img, caps); err != nil {
		t.Fatalf("vision model rejected: %v", err)
	}
	if err := checkImageSupport("blind", img, caps); err == nil {
		t.Fatal("a text-only model accepted an image silently")
	} else if !strings.Contains(err.Error(), "不支持图片") {
		t.Fatalf("error does not explain the refusal: %v", err)
	}
	// An unknown model must not be blocked: a models.dev outage would otherwise
	// break every image request.
	if err := checkImageSupport("unknown", img, caps); err != nil {
		t.Fatalf("unknown model blocked: %v", err)
	}
}

func TestStoreDefaultsToAnonymousKey(t *testing.T) {
	dir := t.TempDir()
	st, err := newStore(dir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	creds := st.current()
	if creds.effectiveKey() != anonymousKey {
		t.Fatalf("key = %q, want the anonymous literal", creds.effectiveKey())
	}
	if !creds.anonymous() {
		t.Fatal("a fresh plugin must report the anonymous lane")
	}
	// configured is false: nothing was stored, and the anonymous literal is not
	// a real login.
	if st.configured() {
		t.Fatal("a fresh plugin must not claim to be configured")
	}
}

func TestStoreRoundTripAndRotation(t *testing.T) {
	dir := t.TempDir()
	st, err := newStore(dir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := st.set(credentials{APIKey: "sk-test-key-0123456789abcdef", Label: "k1"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	creds, ok := st.get()
	if !ok || creds.APIKey != "sk-test-key-0123456789abcdef" {
		t.Fatalf("get = %+v/%v", creds, ok)
	}
	if !st.configured() {
		t.Fatal("a stored sk- key must count as configured")
	}
	before := st.current().projectIDFor()
	generation, err := st.rotateFingerprint()
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if generation != 1 {
		t.Fatalf("generation = %d, want 1", generation)
	}
	if st.current().projectIDFor() == before {
		t.Fatal("rotation did not change the project fingerprint")
	}
	// The key must survive the rotation.
	if st.current().APIKey != "sk-test-key-0123456789abcdef" {
		t.Fatalf("key changed during rotation: %q", st.current().APIKey)
	}
	// Re-opening the store must read the sealed file back.
	again, err := newStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if again.current().APIKey != "sk-test-key-0123456789abcdef" {
		t.Fatalf("reopened key = %q", again.current().APIKey)
	}
}

func TestStoreRejectsEmptyKey(t *testing.T) {
	st, err := newStore(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	// Writing an empty key would silently wipe a working login.
	if err := st.set(credentials{APIKey: "   "}); err == nil {
		t.Fatal("expected an error for an empty key")
	}
}

func TestStoreFileIsNotPlaintext(t *testing.T) {
	dir := t.TempDir()
	st, err := newStore(dir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	const key = "sk-plaintext-check-0123456789"
	if err := st.set(credentials{APIKey: key}); err != nil {
		t.Fatalf("set: %v", err)
	}
	raw, err := readFileLimited(filepath.Join(dir, accountsFile), 1<<20)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(raw), key) {
		t.Fatal("the credential file contains the key in plaintext")
	}
	if st.sealKind != "dpapi-user" {
		t.Fatalf("sealKind = %q, want dpapi-user on Windows", st.sealKind)
	}
}

func TestCapabilityCacheFallsBackToDisk(t *testing.T) {
	dir := t.TempDir()
	c := newCapabilityCache(dir)
	entries := []Capability{{ID: "m", Name: "M", Context: 1000}}
	c.Set(entries)
	c.saveDisk(entries)

	// A second cache with an expired timestamp must serve the disk copy rather
	// than returning nothing: "could not refresh" is not "no capabilities".
	fresh := newCapabilityCache(dir)
	loaded := fresh.loadDisk()
	if len(loaded) != 1 || loaded[0].ID != "m" {
		t.Fatalf("disk load = %+v", loaded)
	}
}

func TestFoldSystemIntoMessagesMovesTheGateNote(t *testing.T) {
	out, _, err := BuildPayload([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`), nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var before map[string]any
	if err := json.Unmarshal(out, &before); err != nil {
		t.Fatalf("payload: %v", err)
	}
	note, _ := before["system"].(string)
	if note == "" {
		t.Fatal("the injected tool note did not land in a top-level system field")
	}

	folded, ok := FoldSystemIntoMessages(out)
	if !ok {
		t.Fatal("FoldSystemIntoMessages refused a payload that has a system field")
	}
	var after map[string]any
	if err := json.Unmarshal(folded, &after); err != nil {
		t.Fatalf("folded payload: %v", err)
	}
	if _, still := after["system"]; still {
		t.Fatal("the folded payload still carries a top-level system field")
	}
	msgs, _ := after["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("no messages after folding")
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != note {
		t.Fatalf("first message = %+v, want the gate note as a system message", first)
	}
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want the note plus the caller's one message", len(msgs))
	}
}
