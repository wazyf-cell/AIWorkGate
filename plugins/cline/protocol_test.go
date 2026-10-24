package main

import (
	"encoding/json"
	"testing"
)

// The envelope is what breaks silently on this channel: a call without the
// workos: prefix, or without the four client headers, still looks like a valid
// OpenAI request but is treated as anonymous by the gateway. These assertions are
// the reference's stated rules turned into regression guards.

func TestBearerValueAlwaysCarriesWorkosPrefix(t *testing.T) {
	// The prefix is part of the wire format, not decoration.
	if got := bearerValue("abc123"); got != "workos:abc123" {
		t.Fatalf("bearerValue = %q", got)
	}
	// An already-prefixed token must not be double-prefixed on re-login.
	if got := bearerValue("workos:abc123"); got != "workos:abc123" {
		t.Fatalf("bearerValue re-prefixed: %q", got)
	}
	if got := bearerValue("  workos:abc123  "); got != "workos:abc123" {
		t.Fatalf("bearerValue did not trim: %q", got)
	}
	if got := bearerValue(""); got != "" {
		t.Fatalf("empty token -> %q, want empty", got)
	}
}

func TestBareTokenStripsPrefix(t *testing.T) {
	if got := bareToken("workos:abc"); got != "abc" {
		t.Fatalf("bareToken = %q", got)
	}
	if got := bareToken("abc"); got != "abc" {
		t.Fatalf("bareToken changed a bare token: %q", got)
	}
}

func TestClientHeadersAreTheFourDeclaredOnes(t *testing.T) {
	got := map[string]string{}
	for _, kv := range clineClientHeaders {
		got[kv[0]] = kv[1]
	}
	want := map[string]string{
		"HTTP-Referer":   "https://cline.bot",
		"X-Title":        "Cline",
		"X-IS-MULTIROOT": "false",
		"X-CLIENT-TYPE":  "cline-sdk",
	}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("%s = %q, want %q", key, got[key], value)
		}
	}
	if len(clineClientHeaders) != len(want) {
		t.Fatalf("header count = %d, want %d", len(clineClientHeaders), len(want))
	}
}

func TestAuthenticateHeadersComposeAuthAndClientSet(t *testing.T) {
	pairs := authenticateHeaders("tok", [2]string{"Content-Type", "application/json"})
	got := map[string]string{}
	for _, kv := range pairs {
		got[kv[0]] = kv[1]
	}
	// The Authorization value must be Bearer workos:<token>.
	if got["Authorization"] != "Bearer workos:tok" {
		t.Fatalf("Authorization = %q", got["Authorization"])
	}
	// The client set is mandatory on every call, auth calls included.
	if got["X-CLIENT-TYPE"] != "cline-sdk" || got["X-Title"] != "Cline" {
		t.Fatalf("client headers missing: %v", got)
	}
	if got["Content-Type"] != "application/json" {
		t.Fatalf("extra header dropped: %v", got)
	}
}

func TestSanitizeToolParametersDropsEmptyEnumMembers(t *testing.T) {
	// An empty-string enum member is rejected with a 400 by routed upstreams.
	in := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"mode": map[string]any{"type": "string", "enum": []any{"a", "", "  ", "b"}},
			"bad":  map[string]any{"type": "string", "enum": []any{"", " "}},
		},
	}
	out, ok := SanitizeToolParameters(in).(map[string]any)
	if !ok {
		t.Fatal("sanitize did not return a map")
	}
	props := out["properties"].(map[string]any)
	mode := props["mode"].(map[string]any)
	enum := mode["enum"].([]any)
	if len(enum) != 2 || enum[0] != "a" || enum[1] != "b" {
		t.Fatalf("enum = %v, want [a b]", enum)
	}
	// An enum that was entirely empty must be dropped, not sent as [].
	bad := props["bad"].(map[string]any)
	if _, present := bad["enum"]; present {
		t.Fatalf("empty enum survived: %v", bad)
	}
}

func TestSanitizeToolParametersRecursesIntoArrays(t *testing.T) {
	in := []any{
		map[string]any{"enum": []any{"x", ""}},
		"plain",
	}
	out, ok := SanitizeToolParameters(in).([]any)
	if !ok || len(out) != 2 {
		t.Fatalf("sanitize = %v", out)
	}
	enum := out[0].(map[string]any)["enum"].([]any)
	if len(enum) != 1 || enum[0] != "x" {
		t.Fatalf("nested enum = %v", enum)
	}
}

func TestClampMaxTokens(t *testing.T) {
	if got := ClampMaxTokens(0); got != 0 {
		t.Fatalf("0 -> %d, want 0 (dropped)", got)
	}
	if got := ClampMaxTokens(-7); got != 0 {
		t.Fatalf("-7 -> %d, want 0 (dropped)", got)
	}
	if got := ClampMaxTokens(4096); got != 4096 {
		t.Fatalf("4096 -> %d", got)
	}
	// Above the channel ceiling it must be clamped, not forwarded: the gateway
	// rejects an over-limit value outright.
	if got := ClampMaxTokens(maxOutputTokens + 1); got != maxOutputTokens {
		t.Fatalf("over-ceiling -> %d, want %d", got, maxOutputTokens)
	}
}

func TestBuildBodyForcesStreamAndClamps(t *testing.T) {
	out, err := BuildBody([]byte(`{"model":"m","stream":false,"max_tokens":1000000,"messages":[{"role":"user","content":"hi"}]}`), false)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	if got["stream"] != true {
		t.Fatalf("stream = %v, want true", got["stream"])
	}
	if got["max_tokens"] != float64(maxOutputTokens) {
		t.Fatalf("max_tokens = %v, want the clamp %d", got["max_tokens"], maxOutputTokens)
	}
	// The messages must survive untouched: this channel needs no translation.
	msgs, ok := got["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages = %v", got["messages"])
	}
}

func TestBuildBodyDropsUndeclaredEffortAndFillsDefault(t *testing.T) {
	// Each case decodes into a fresh map: reusing one would leave the previous
	// case's key in place, because json.Unmarshal does not clear absent keys.
	decode := func(body []byte) map[string]any {
		out, err := BuildBody(body, false)
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return got
	}
	decodeDefault := func(body []byte) map[string]any {
		out, err := BuildBody(body, true)
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return got
	}
	// An undeclared effort would be a 400, so with a default wanted it must be
	// replaced rather than forwarded.
	if got := decodeDefault([]byte(`{"model":"m","reasoning_effort":"xhigh","messages":[{"role":"user","content":"x"}]}`)); got["reasoning_effort"] != defaultReasoningEffort {
		t.Fatalf("undeclared effort -> %v, want the default %q", got["reasoning_effort"], defaultReasoningEffort)
	}
	// Without a default wanted it must simply be dropped.
	if got := decode([]byte(`{"model":"m","reasoning_effort":"xhigh","messages":[{"role":"user","content":"x"}]}`)); got["reasoning_effort"] != nil {
		t.Fatalf("undeclared effort kept: %v", got["reasoning_effort"])
	}
	// A declared value must pass through unchanged in both modes.
	if got := decodeDefault([]byte(`{"model":"m","reasoning_effort":"low","messages":[{"role":"user","content":"x"}]}`)); got["reasoning_effort"] != "low" {
		t.Fatalf("declared effort -> %v, want low", got["reasoning_effort"])
	}
	// With defaultEffort off and no request value, the field must stay absent.
	if got := decode([]byte(`{"model":"m","messages":[{"role":"user","content":"x"}]}`)); got["reasoning_effort"] != nil {
		t.Fatalf("effort injected against the caller's silence: %v", got["reasoning_effort"])
	}
}

func TestIsValidEffort(t *testing.T) {
	for _, e := range []string{"none", "low", "medium", "high", "max"} {
		if !IsValidEffort(e) {
			t.Fatalf("%q must be accepted", e)
		}
	}
	// xhigh is declared by other channels but NOT by Cline.
	for _, e := range []string{"xhigh", "minimal", "ultra", ""} {
		if IsValidEffort(e) {
			t.Fatalf("%q must be rejected", e)
		}
	}
}
