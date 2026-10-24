package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// The protocol layer is the part most likely to break silently: a wrong mapping
// still produces valid JSON, so only assertions catch it.

// decodeEnvelope splits the translated body into its envelope and inner request.
func decodeEnvelope(t *testing.T, out []byte) (map[string]any, map[string]any) {
	t.Helper()
	var env struct {
		Model     string         `json:"model"`
		Project   string         `json:"project"`
		Request   map[string]any `json:"request"`
		RequestID string         `json:"requestId"`
		UserAgent string         `json:"userAgent"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, out)
	}
	if env.Model == "" {
		t.Fatalf("envelope has no model: %s", out)
	}
	if env.Project == "" {
		t.Fatalf("envelope has no project: %s", out)
	}
	if env.RequestID == "" {
		t.Fatalf("envelope has no requestId: %s", out)
	}
	if env.UserAgent != "antigravity" {
		t.Fatalf("envelope userAgent = %q, want antigravity", env.UserAgent)
	}
	return map[string]any{"model": env.Model, "project": env.Project}, env.Request
}

func TestTranslateRequestBuildsEnvelope(t *testing.T) {
	spec, err := modelSpecFor("gemini-3.8-flash", "high")
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	in := []byte(`{
		"model":"gemini-3.8-flash",
		"messages":[
			{"role":"system","content":"be terse"},
			{"role":"user","content":"hi"}
		]
	}`)
	out, err := translateRequest(in, requestOptions{ModelID: "gemini-3.8-flash", Spec: spec})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	env, inner := decodeEnvelope(t, out)
	// The upstream model id carries the effort tier as a suffix.
	if env["model"] != "gemini-3.8-flash-high" {
		t.Fatalf("model = %q, want gemini-3.8-flash-high", env["model"])
	}
	if env["project"] != defaultProject {
		t.Fatalf("project = %q, want %q", env["project"], defaultProject)
	}
	// system becomes systemInstruction, never an inline message.
	si, ok := inner["systemInstruction"].(map[string]any)
	if !ok {
		t.Fatalf("systemInstruction missing: %v", inner)
	}
	if si["role"] != "system" {
		t.Fatalf("systemInstruction.role = %v", si["role"])
	}
	contents, _ := inner["contents"].([]any)
	if len(contents) != 1 {
		t.Fatalf("contents = %v, want only the user turn", contents)
	}
	if contents[0].(map[string]any)["role"] != "user" {
		t.Fatalf("contents[0].role = %v", contents[0].(map[string]any)["role"])
	}
}

func TestTranslateRequestMapsAssistantToModelRole(t *testing.T) {
	spec, _ := modelSpecFor("gemini-3.8-flash", "medium")
	in := []byte(`{
		"model":"gemini-3.8-flash",
		"messages":[
			{"role":"user","content":"hello"},
			{"role":"assistant","content":"hi there"},
			{"role":"user","content":"again"}
		]
	}`)
	out, _ := translateRequest(in, requestOptions{ModelID: "gemini-3.8-flash", Spec: spec})
	_, inner := decodeEnvelope(t, out)
	contents, _ := inner["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("contents = %d, want 3", len(contents))
	}
	// assistant is "model" in the Gemini vocabulary; everything else is "user".
	if got := contents[1].(map[string]any)["role"]; got != "model" {
		t.Fatalf("assistant role = %v, want model", got)
	}
	if got := contents[2].(map[string]any)["role"]; got != "user" {
		t.Fatalf("user role = %v, want user", got)
	}
}

func TestTranslateRequestMapsToolCallsAndResults(t *testing.T) {
	spec, _ := modelSpecFor("gemini-3.8-flash", "medium")
	in := []byte(`{
		"model":"gemini-3.8-flash",
		"messages":[
			{"role":"user","content":"q"},
			{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]},
			{"role":"tool","tool_call_id":"c1","content":"done"}
		]
	}`)
	out, _ := translateRequest(in, requestOptions{ModelID: "gemini-3.8-flash", Spec: spec})
	_, inner := decodeEnvelope(t, out)
	contents, _ := inner["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("contents = %d, want 3", len(contents))
	}
	modelParts, _ := contents[1].(map[string]any)["parts"].([]any)
	if len(modelParts) != 1 {
		t.Fatalf("model parts = %v", modelParts)
	}
	fc, ok := modelParts[0].(map[string]any)["functionCall"].(map[string]any)
	if !ok {
		t.Fatalf("no functionCall part: %v", modelParts[0])
	}
	if fc["name"] != "f" {
		t.Fatalf("functionCall.name = %v", fc["name"])
	}
	args, _ := fc["args"].(map[string]any)
	if args["a"] != float64(1) {
		t.Fatalf("functionCall.args = %v", fc["args"])
	}
	// A tool result becomes functionResponse carrying {name, response:{content}}.
	userParts, _ := contents[2].(map[string]any)["parts"].([]any)
	if len(userParts) != 1 {
		t.Fatalf("user parts = %v", userParts)
	}
	fr, ok := userParts[0].(map[string]any)["functionResponse"].(map[string]any)
	if !ok {
		t.Fatalf("no functionResponse part: %v", userParts[0])
	}
	if fr["name"] != "f" {
		t.Fatalf("functionResponse.name = %v, want the resolved call name", fr["name"])
	}
	resp, _ := fr["response"].(map[string]any)
	if resp["content"] != "done" {
		t.Fatalf("functionResponse.response = %v", fr["response"])
	}
}

func TestTranslateRequestSanitizesToolSchemas(t *testing.T) {
	spec, _ := modelSpecFor("gemini-3.8-flash", "medium")
	in := []byte(`{
		"model":"gemini-3.8-flash",
		"messages":[{"role":"user","content":"go"}],
		"tools":[{"type":"function","function":{"name":"f","description":"d","parameters":{
			"type":"object",
			"$schema":"http://json-schema.org/draft-07/schema#",
			"additionalProperties":false,
			"properties":{
				"a":{"type":["string","null"],"description":"x","default":"z"}
			}
		}}}]
	}`)
	out, _ := translateRequest(in, requestOptions{ModelID: "gemini-3.8-flash", Spec: spec})
	_, inner := decodeEnvelope(t, out)
	tools, _ := inner["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", tools)
	}
	decls, _ := tools[0].(map[string]any)["functionDeclarations"].([]any)
	if len(decls) != 1 {
		t.Fatalf("functionDeclarations = %v", decls)
	}
	params, _ := decls[0].(map[string]any)["parameters"].(map[string]any)
	// Keys outside the allow-list make the upstream answer 400, so they are
	// dropped rather than forwarded.
	for _, banned := range []string{"$schema", "additionalProperties"} {
		if _, present := params[banned]; present {
			t.Fatalf("%s must be stripped: %v", banned, params)
		}
	}
	props, _ := params["properties"].(map[string]any)
	child, _ := props["a"].(map[string]any)
	// A ["string","null"] type collapses to "string" + nullable.
	if child["type"] != "string" {
		t.Fatalf("child.type = %v, want string", child["type"])
	}
	if child["nullable"] != true {
		t.Fatalf("child.nullable = %v, want true", child["nullable"])
	}
	// "default" is not in the allow-list either.
	if _, present := child["default"]; present {
		t.Fatalf("default must be stripped: %v", child)
	}
}

func TestTranslateRequestGenerationConfig(t *testing.T) {
	t.Run("tiered omits thinkingBudget", func(t *testing.T) {
		spec, _ := modelSpecFor("gemini-3.8-flash", "tiered")
		if spec.ThinkingBudget != budgetTiered {
			t.Fatalf("tiered budget = %d, want %d", spec.ThinkingBudget, budgetTiered)
		}
		out, _ := translateRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"x"}]}`),
			requestOptions{ModelID: "gemini-3.8-flash", Spec: spec})
		_, inner := decodeEnvelope(t, out)
		gc, _ := inner["generationConfig"].(map[string]any)
		tc, _ := gc["thinkingConfig"].(map[string]any)
		if _, present := tc["thinkingBudget"]; present {
			t.Fatalf("thinkingBudget must be omitted for tiered: %v", tc)
		}
		if tc["includeThoughts"] != true {
			t.Fatalf("includeThoughts = %v, want true", tc["includeThoughts"])
		}
	})
	t.Run("low sends its budget", func(t *testing.T) {
		spec, _ := modelSpecFor("gemini-3.8-flash", "low")
		out, _ := translateRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"x"}]}`),
			requestOptions{ModelID: "gemini-3.8-flash", Spec: spec})
		_, inner := decodeEnvelope(t, out)
		gc, _ := inner["generationConfig"].(map[string]any)
		tc, _ := gc["thinkingConfig"].(map[string]any)
		if tc["thinkingBudget"] != float64(budgetLow) {
			t.Fatalf("thinkingBudget = %v, want %d", tc["thinkingBudget"], budgetLow)
		}
	})
	t.Run("default maxOutputTokens", func(t *testing.T) {
		spec, _ := modelSpecFor("gemini-3.8-flash", "medium")
		out, _ := translateRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"x"}]}`),
			requestOptions{ModelID: "gemini-3.8-flash", Spec: spec})
		_, inner := decodeEnvelope(t, out)
		gc, _ := inner["generationConfig"].(map[string]any)
		// The definition's maxOutputTokens.
		if gc["maxOutputTokens"] != float64(defaultMaxOutputTokens) {
			t.Fatalf("maxOutputTokens = %v, want %d", gc["maxOutputTokens"], defaultMaxOutputTokens)
		}
	})
}

func TestTranslateRequestToolChoice(t *testing.T) {
	spec, _ := modelSpecFor("gemini-3.8-flash", "medium")
	in := []byte(`{
		"model":"gemini-3.8-flash",
		"messages":[{"role":"user","content":"go"}],
		"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],
		"tool_choice":{"type":"function","function":{"name":"f"}}
	}`)
	out, _ := translateRequest(in, requestOptions{ModelID: "gemini-3.8-flash", Spec: spec})
	_, inner := decodeEnvelope(t, out)
	tc, ok := inner["toolConfig"].(map[string]any)
	if !ok {
		t.Fatalf("toolConfig missing: %v", inner)
	}
	fcc, _ := tc["functionCallingConfig"].(map[string]any)
	if fcc["mode"] != "ANY" {
		t.Fatalf("mode = %v, want ANY", fcc["mode"])
	}
	allowed, _ := fcc["allowedFunctionNames"].([]any)
	if len(allowed) != 1 || allowed[0] != "f" {
		t.Fatalf("allowedFunctionNames = %v, want [f]", fcc["allowedFunctionNames"])
	}
}

func TestTranslateRequestConvertsDataURLImage(t *testing.T) {
	spec, _ := modelSpecFor("gemini-3.8-flash", "medium")
	in := []byte(`{
		"model":"gemini-3.8-flash",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"look"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}
		]}]
	}`)
	images := decodeInlineImages(in)
	if len(images) != 1 {
		t.Fatalf("decodeInlineImages = %v", images)
	}
	out, _ := translateRequest(in, requestOptions{
		ModelID: "gemini-3.8-flash", Spec: spec, Images: images,
	})
	_, inner := decodeEnvelope(t, out)
	contents, _ := inner["contents"].([]any)
	parts, _ := contents[0].(map[string]any)["parts"].([]any)
	if len(parts) != 2 {
		t.Fatalf("parts = %v", parts)
	}
	inline, ok := parts[1].(map[string]any)["inlineData"].(map[string]any)
	if !ok {
		t.Fatalf("no inlineData part: %v", parts[1])
	}
	if inline["mimeType"] != "image/png" || inline["data"] != "QUJD" {
		t.Fatalf("inlineData = %v", inline)
	}
}

func TestTranslateRequestRejectsEmptyConversation(t *testing.T) {
	spec, _ := modelSpecFor("gemini-3.8-flash", "medium")
	// A body whose only message is a system turn has no usable content, and the
	// reference throws rather than sending an empty request.
	_, err := translateRequest([]byte(`{"model":"m","messages":[{"role":"system","content":"s"}]}`),
		requestOptions{ModelID: "gemini-3.8-flash", Spec: spec})
	if err == nil {
		t.Fatal("expected an error for a conversation with no usable content")
	}
}

func TestTranslateRequestRejectsMalformedJSON(t *testing.T) {
	spec, _ := modelSpecFor("gemini-3.8-flash", "medium")
	if _, err := translateRequest([]byte(`{"model":`), requestOptions{Spec: spec}); err == nil {
		t.Fatal("expected an error for truncated JSON")
	}
}

// An unknown model is a deterministic 404, not a silent substitution.
func TestModelSpecRejectsUnknownModel(t *testing.T) {
	if _, err := modelSpecFor("gpt-4o", "medium"); err == nil {
		t.Fatal("expected an error for a model outside the catalog")
	}
	if _, err := modelSpecFor("gemini-3.8-flash", "medium"); err != nil {
		t.Fatalf("the catalog model must resolve: %v", err)
	}
}

func TestEffortTiersMapToBudgetsAndSuffixes(t *testing.T) {
	cases := []struct {
		effort   string
		budget   int
		upstream string
	}{
		{"low", budgetLow, "gemini-3.8-flash-low"},
		{"medium", budgetMedium, "gemini-3.8-flash-medium"},
		{"high", budgetHigh, "gemini-3.8-flash-high"},
		{"tiered", budgetTiered, "gemini-3.8-flash-tiered"},
		// An unknown effort clamps to medium rather than failing.
		{"bogus", budgetMedium, "gemini-3.8-flash-medium"},
	}
	for _, tc := range cases {
		spec, err := modelSpecFor("gemini-3.8-flash", tc.effort)
		if err != nil {
			t.Fatalf("spec(%q): %v", tc.effort, err)
		}
		if spec.Upstream != tc.upstream {
			t.Fatalf("spec(%q).Upstream = %q, want %q", tc.effort, spec.Upstream, tc.upstream)
		}
		if spec.ThinkingBudget != tc.budget {
			t.Fatalf("spec(%q).ThinkingBudget = %d, want %d", tc.effort, spec.ThinkingBudget, tc.budget)
		}
		if !spec.IncludeThoughts {
			t.Fatalf("spec(%q).IncludeThoughts must be true", tc.effort)
		}
	}
}

// A model id may pin a tier by suffix; the canonical id still resolves.
func TestCanonicalModelIDAndEffortSuffix(t *testing.T) {
	if got := canonicalModelID("gemini-3.8-flash-high"); got != "gemini-3.8-flash" {
		t.Fatalf("canonicalModelID = %q", got)
	}
	if got := canonicalModelID("gemini-3.8-flash"); got != "gemini-3.8-flash" {
		t.Fatalf("canonicalModelID = %q", got)
	}
	if got := effortFromModelID("gemini-3.8-flash-high"); got != "high" {
		t.Fatalf("effortFromModelID = %q", got)
	}
	if got := effortFromModelID("gemini-3.8-flash"); got != "" {
		t.Fatalf("effortFromModelID = %q, want empty", got)
	}
}

func TestDeriveSessionIDIsStable(t *testing.T) {
	a := deriveSessionID("proj", "hello", "infer")
	b := deriveSessionID("proj", "hello", "infer")
	if a != b {
		t.Fatalf("session id is not stable: %q vs %q", a, b)
	}
	if c := deriveSessionID("proj", "hello2", "infer"); c == a {
		t.Fatalf("a different first turn must change the id: %q", c)
	}
	// The value is a signed 64-bit decimal, as the reference renders it.
	if strings.ContainsAny(a, "xX+") || a == "" {
		t.Fatalf("session id = %q, want a decimal", a)
	}
}

// buildSSE renders frames the way the upstream does; payloads may be wrapped in
// {"response": {...}}.
func buildSSE(frames ...string) string {
	var b strings.Builder
	for _, f := range frames {
		b.WriteString("data: " + f + "\n\n")
	}
	return b.String()
}

func TestPumpGeminiWrappedFrames(t *testing.T) {
	body := buildSSE(
		`{"response":{"candidates":[{"content":{"parts":[{"text":"hel"}]}}]}}`,
		`{"response":{"candidates":[{"content":{"parts":[{"text":"lo"}]}}]}}`,
		`{"response":{"candidates":[{"content":{"parts":[{"text":"!"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":4}}}`,
	)
	tl := newGeminiTranslator(func(map[string]any, any) {})
	if err := pumpGemini(strings.NewReader(body), tl); err != nil {
		t.Fatalf("pump: %v", err)
	}
	var decoded struct {
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
			Total      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(completionFromTranslator(tl, "gemini-3.8-flash"), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Object != "chat.completion" {
		t.Fatalf("object = %q", decoded.Object)
	}
	if decoded.Choices[0].Message.Content != "hello!" {
		t.Fatalf("content = %q, want hello!", decoded.Choices[0].Message.Content)
	}
	if decoded.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q, want stop", decoded.Choices[0].FinishReason)
	}
	if decoded.Usage.Prompt != 10 || decoded.Usage.Completion != 4 || decoded.Usage.Total != 14 {
		t.Fatalf("usage = %+v", decoded.Usage)
	}
}

// The unwrapped shape must decode too: the reference accepts both.
func TestPumpGeminiUnwrappedFrames(t *testing.T) {
	body := buildSSE(
		`{"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}]}`,
	)
	tl := newGeminiTranslator(func(map[string]any, any) {})
	if err := pumpGemini(strings.NewReader(body), tl); err != nil {
		t.Fatalf("pump: %v", err)
	}
	var decoded struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	_ = json.Unmarshal(completionFromTranslator(tl, "m"), &decoded)
	if decoded.Choices[0].Message.Content != "hi" {
		t.Fatalf("content = %q, want hi", decoded.Choices[0].Message.Content)
	}
}

// A thought part must not leak into the visible content.
func TestPumpGeminiWithholdsThoughtParts(t *testing.T) {
	body := buildSSE(
		`{"response":{"candidates":[{"content":{"parts":[{"text":"thinking...","thought":true}]}}]}}`,
		`{"response":{"candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP"}]}}`,
	)
	tl := newGeminiTranslator(func(map[string]any, any) {})
	if err := pumpGemini(strings.NewReader(body), tl); err != nil {
		t.Fatalf("pump: %v", err)
	}
	var decoded struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	_ = json.Unmarshal(completionFromTranslator(tl, "m"), &decoded)
	if decoded.Choices[0].Message.Content != "answer" {
		t.Fatalf("content = %q, want only the visible answer", decoded.Choices[0].Message.Content)
	}
}

func TestPumpGeminiAssemblesFunctionCalls(t *testing.T) {
	body := buildSSE(
		`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"f","args":{"b":2,"a":1}}}]},"finishReason":"STOP"}]}}`,
	)
	tl := newGeminiTranslator(func(map[string]any, any) {})
	if err := pumpGemini(strings.NewReader(body), tl); err != nil {
		t.Fatalf("pump: %v", err)
	}
	var decoded struct {
		Choices []struct {
			Message struct {
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
	}
	if err := json.Unmarshal(completionFromTranslator(tl, "m"), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	tc := decoded.Choices[0].Message.ToolCalls
	if len(tc) != 1 || tc[0].Function.Name != "f" {
		t.Fatalf("tool_calls = %+v", tc)
	}
	// Arguments are re-marshalled with sorted keys (canonicalArgs).
	if tc[0].Function.Arguments != `{"a":1,"b":2}` {
		t.Fatalf("arguments = %q, want sorted keys", tc[0].Function.Arguments)
	}
	if !strings.HasPrefix(tc[0].ID, "gemini_tool_") {
		t.Fatalf("tool call id = %q, want the gemini_tool_ prefix", tc[0].ID)
	}
	// A turn with tool calls reports finish_reason tool_calls.
	if decoded.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", decoded.Choices[0].FinishReason)
	}
}

func TestMapFinish(t *testing.T) {
	cases := []struct {
		reason string
		tools  int
		want   string
	}{
		{"STOP", 0, "stop"},
		{"MAX_TOKENS", 0, "length"},
		{"STOP", 1, "tool_calls"},
		{"", 0, "stop"},
		{"WHATEVER", 0, "stop"},
	}
	for _, tc := range cases {
		if got := mapFinish(tc.reason, tc.tools); got != tc.want {
			t.Fatalf("mapFinish(%q, %d) = %q, want %q", tc.reason, tc.tools, got, tc.want)
		}
	}
}

// The usage mapping subtracts cached tokens from the prompt count, as
// readGeminiUsage does.
func TestGeminiUsageSubtractsCachedTokens(t *testing.T) {
	body := buildSSE(
		`{"response":{"candidates":[{"content":{"parts":[{"text":"x"}]},"finishReason":"STOP"}],` +
			`"usageMetadata":{"promptTokenCount":100,"cachedContentTokenCount":40,"candidatesTokenCount":5}}}`,
	)
	tl := newGeminiTranslator(func(map[string]any, any) {})
	_ = pumpGemini(strings.NewReader(body), tl)
	if tl.usage.InputTokens != 60 {
		t.Fatalf("input tokens = %d, want 60 (100 prompt - 40 cached)", tl.usage.InputTokens)
	}
	if tl.usage.OutputTokens != 5 {
		t.Fatalf("output tokens = %d, want 5", tl.usage.OutputTokens)
	}
}

// The quota parser averages the two buckets, as toGeminiCreditBalance does.
func TestParseQuotaSummaryAveragesBuckets(t *testing.T) {
	raw := []byte(`{"groups":[{"buckets":[
		{"bucketId":"gemini-5h","remainingFraction":0.5,"resetTime":"2026-10-22T05:00:00Z"},
		{"bucketId":"gemini-weekly","remainingFraction":1.0,"resetTime":"2026-10-27T00:00:00Z"},
		{"bucketId":"some-other","remainingFraction":0.0,"resetTime":"2026-10-22T05:00:00Z"}
	]}]}`)
	out, ok := parseQuotaSummary(raw)
	if !ok {
		t.Fatal("quota summary should parse")
	}
	// (0.5 + 1.0) / 2 * 100 = 75; the unrelated bucket must be ignored.
	if out["remaining"] != float64(75) {
		t.Fatalf("remaining = %v, want 75", out["remaining"])
	}
	if out["total"] != 100 {
		t.Fatalf("total = %v, want 100", out["total"])
	}
}

// A quota payload without the recognised buckets is "unknown", never a zero.
func TestParseQuotaSummaryRejectsUnknownShape(t *testing.T) {
	if _, ok := parseQuotaSummary([]byte(`{"groups":[{"buckets":[{"bucketId":"other","remainingFraction":0.5}]}]}`)); ok {
		t.Fatal("a payload with no recognised bucket must not be reported as quota")
	}
	if _, ok := parseQuotaSummary([]byte(`not json`)); ok {
		t.Fatal("garbage must not be reported as quota")
	}
}

func TestIdentityFromIDToken(t *testing.T) {
	// A minimal unsigned JWT-shaped id_token: header.payload.
	payload := `{"sub":"sub-1","email":"a@example.com"}`
	encoded := base64.RawURLEncoding.EncodeToString([]byte(payload))
	sub, email := identityFromIDToken("h." + encoded + ".sig")
	if sub != "sub-1" || email != "a@example.com" {
		t.Fatalf("identity = (%q, %q)", sub, email)
	}
	if sub, email := identityFromIDToken("garbage"); sub != "" || email != "" {
		t.Fatalf("garbage id_token must yield no identity, got (%q, %q)", sub, email)
	}
}
