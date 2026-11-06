package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// The protocol layer is the part most likely to break silently: the AutoGLM
// proxy channel accepts an OpenAI body more or less as-is, so a wrong rewrite
// still produces valid JSON and only assertions catch it.

func TestValidateChatBodyRejectsMissingMessages(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"no messages", `{"model":"m"}`, "messages must be a non-empty array"},
		{"empty array", `{"model":"m","messages":[]}`, "messages must be a non-empty array"},
		{"only system", `{"model":"m","messages":[{"role":"system","content":"x"}]}`,
			"messages must contain at least one user message"},
		{"truncated json", `{"model":`, "invalid JSON body"},
	}
	for _, tc := range cases {
		if got := validateChatBody([]byte(tc.body)); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestValidateChatBodyAcceptsUserTurn(t *testing.T) {
	if got := validateChatBody([]byte(`{"messages":[{"role":"user","content":"hi"}]}`)); got != "" {
		t.Fatalf("got %q, want acceptance", got)
	}
}

func TestIsAgentTargetAndNormalizeModel(t *testing.T) {
	for _, m := range []string{"openclaw", "openclaw/default", "openclaw/x", "openclaw:y", "agent:z", "OPENCLAW"} {
		if !isAgentTarget(m) {
			t.Errorf("%q must be an agent target", m)
		}
	}
	// A backend model is not an agent target and must be moved to the
	// x-openclaw-model header instead of the body.
	if isAgentTarget("zai_auto") {
		t.Fatal("zai_auto must not be an agent target")
	}
	agent, backend := normalizeModel("zai_auto")
	if agent != "openclaw" || backend != "zai_auto" {
		t.Fatalf("normalizeModel(zai_auto) = (%q,%q), want (openclaw,zai_auto)", agent, backend)
	}
	agent, backend = normalizeModel("openclaw")
	if agent != "openclaw" || backend != "" {
		t.Fatalf("normalizeModel(openclaw) = (%q,%q), want (openclaw,\"\")", agent, backend)
	}
	agent, backend = normalizeModel("")
	if agent != "openclaw" || backend != "" {
		t.Fatalf("normalizeModel(\"\") = (%q,%q)", agent, backend)
	}
}

func TestPrepareModelBodyMovesBackendModelOut(t *testing.T) {
	in := []byte(`{"model":"zai_auto","messages":[{"role":"user","content":"x"}]}`)
	out := prepareModelBody(in)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["model"] != "openclaw" {
		t.Fatalf("model = %v, want openclaw", got["model"])
	}
	// An agent target keeps its name.
	agentBody := []byte(`{"model":"openclaw/default","messages":[]}`)
	var kept map[string]any
	if err := json.Unmarshal(prepareModelBody(agentBody), &kept); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if kept["model"] != "openclaw/default" {
		t.Fatalf("agent model = %v, want it untouched", kept["model"])
	}
}

func TestPrepareProxyBodySetsModelAndDropsUser(t *testing.T) {
	in := []byte(`{"model":"old","user":"someone","messages":[{"role":"user","content":"x"}]}`)
	out := prepareProxyBody(in, "zai_auto")
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["model"] != "zai_auto" {
		t.Fatalf("model = %v, want zai_auto", got["model"])
	}
	// The proxy rejects the OpenAI "user" field, so it must be gone.
	if _, present := got["user"]; present {
		t.Fatalf("user must be dropped: %v", got)
	}
	if _, ok := got["messages"]; !ok {
		t.Fatal("messages must survive the rewrite")
	}
}

func TestStripPrefixes(t *testing.T) {
	if got := stripRoutePrefix("global/zai_auto"); got != "zai_auto" {
		t.Fatalf("stripRoutePrefix = %q", got)
	}
	if got := stripRoutePrefix("zai_auto"); got != "zai_auto" {
		t.Fatalf("stripRoutePrefix = %q", got)
	}
	for in, want := range map[string]string{
		"global/m": "m", "cn/m": "m", "global:m": "m", "cn:m": "m", "m": "m",
	} {
		if got := stripRegionPrefix(in); got != want {
			t.Fatalf("stripRegionPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRequestModelHeaderSkipsAgentTargets(t *testing.T) {
	if got := requestModelHeader([]byte(`{"model":"zai_auto"}`)); got != "zai_auto" {
		t.Fatalf("got %q, want zai_auto", got)
	}
	if got := requestModelHeader([]byte(`{"model":"global/zai_auto"}`)); got != "zai_auto" {
		t.Fatalf("got %q, want the route prefix stripped", got)
	}
	// An agent target must not pin a backend model: the sandbox picks it.
	if got := requestModelHeader([]byte(`{"model":"openclaw"}`)); got != "" {
		t.Fatalf("got %q, want empty for an agent target", got)
	}
}

func TestNotEnabledClassification(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{404, "", true},
		{501, "", true},
		{400, `{"error":"chatCompletions not enabled"}`, true},
		{400, `{"error":"this endpoint is disabled"}`, true},
		{400, `{"error":"bad temperature"}`, false},
		{500, "", false},
	}
	for _, tc := range cases {
		if got := notEnabled(tc.status, tc.body); got != tc.want {
			t.Errorf("notEnabled(%d,%q) = %v, want %v", tc.status, tc.body, got, tc.want)
		}
	}
}

func TestBuildRelayChatReqPicksLastUserText(t *testing.T) {
	body := []byte(`{
		"user":"sess-1",
		"reasoning_effort":"high",
		"messages":[
			{"role":"user","content":"first"},
			{"role":"assistant","content":"reply"},
			{"role":"user","content":[{"type":"text","text":"second"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA"}}]}
		]
	}`)
	req, err := buildRelayChatReq(body)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if req["sessionKey"] != "sess-1" {
		t.Fatalf("sessionKey = %v", req["sessionKey"])
	}
	// The relay carries a single message, so the last user turn must win.
	if req["message"] != "second" {
		t.Fatalf("message = %v, want second", req["message"])
	}
	if req["thinking"] != "high" {
		t.Fatalf("thinking = %v", req["thinking"])
	}
}

func TestBuildRelayChatReqDefaultsSessionAndRejectsNoUserText(t *testing.T) {
	req, err := buildRelayChatReq([]byte(`{"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if req["sessionKey"] != "main" {
		t.Fatalf("sessionKey = %v, want main", req["sessionKey"])
	}
	if _, err := buildRelayChatReq([]byte(`{"messages":[{"role":"assistant","content":"x"}]}`)); err == nil {
		t.Fatal("expected an error when there is no user text")
	}
}

func TestParseModelCatalogHasBothRegions(t *testing.T) {
	models := staticModels()
	if len(models) != len(cnStaticModels)+len(globalStaticModels) {
		t.Fatalf("catalog size = %d", len(models))
	}
	seen := map[string]bool{}
	for _, m := range models {
		id, _ := m["id"].(string)
		seen[id] = true
	}
	// The reference advertises these two ids verbatim, including the slash in
	// "openclaw/default": the host drops ids containing "/" from its catalog
	// view, which is why manifest.models stays empty and this endpoint is the
	// source of truth.
	if !seen["openclaw"] || !seen["openclaw/default"] {
		t.Fatalf("catalog is missing the agent targets: %v", seen)
	}
	if !seen["global/zai_auto"] {
		t.Fatalf("catalog is missing the global region prefix: %v", seen)
	}
	// Every global id must carry the prefix the region router strips.
	for _, m := range models {
		id, _ := m["id"].(string)
		if strings.HasPrefix(id, "global/") {
			continue
		}
		if strings.Contains(id, "global") {
			t.Fatalf("a global model id lost its prefix: %q", id)
		}
	}
}
