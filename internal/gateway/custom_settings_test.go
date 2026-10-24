package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCustomSettingsCapAndDefaultOutput(t *testing.T) {
	cm := CustomModel{Name: "m", InputTypes: []string{"text"}, MaxOutputTokens: 100}
	body := []byte(`{"messages":[{"role":"user","content":"hi"}],"max_tokens":500}`)
	out, _, serr := applyCustomModelSettings(cm, body)
	if serr != nil {
		t.Fatalf("settings error: %+v", serr)
	}
	var obj map[string]any
	obj = nil
	_ = json.Unmarshal(out, &obj)
	if obj["max_tokens"] != float64(100) {
		t.Fatalf("max_tokens = %v, want capped 100", obj["max_tokens"])
	}

	out, _, serr = applyCustomModelSettings(cm, []byte(`{"messages":[]}`))
	if serr != nil {
		t.Fatalf("settings error: %+v", serr)
	}
	obj = nil
	_ = json.Unmarshal(out, &obj)
	if obj["max_tokens"] != float64(100) {
		t.Fatalf("default max_tokens = %v, want 100", obj["max_tokens"])
	}

	out, _, _ = applyCustomModelSettings(cm, []byte(`{"messages":[],"max_completion_tokens":900}`))
	obj = nil
	_ = json.Unmarshal(out, &obj)
	if obj["max_completion_tokens"] != float64(100) {
		t.Fatalf("max_completion_tokens = %v", obj["max_completion_tokens"])
	}
	if _, ok := obj["max_tokens"]; ok {
		t.Fatalf("host added max_tokens alongside max_completion_tokens: %v", obj)
	}
}

func TestCustomSettingsRejectsImageWhenTextOnly(t *testing.T) {
	cm := CustomModel{Name: "text-only", InputTypes: []string{"text"}}
	body := []byte(`{"messages":[{"role":"user","content":[
		{"type":"text","text":"what is this"},
		{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`)
	_, _, serr := applyCustomModelSettings(cm, body)
	if serr == nil || serr.status != 400 || serr.code != "unsupported_input_type" {
		t.Fatalf("error = %+v, want unsupported_input_type", serr)
	}

	cm.InputTypes = []string{"text", "image"}
	if _, _, serr := applyCustomModelSettings(cm, body); serr != nil {
		t.Fatalf("image allowed but rejected: %+v", serr)
	}
}

func TestCustomSettingsContextWindow(t *testing.T) {
	cm := CustomModel{Name: "small", ContextWindow: 20}
	body := []byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("汉", 30) + `"}]}`)
	_, _, serr := applyCustomModelSettings(cm, body)
	if serr == nil || serr.code != "context_length_exceeded" {
		t.Fatalf("error = %+v, want context_length_exceeded", serr)
	}
	if _, _, serr := applyCustomModelSettings(cm, []byte(`{"messages":[{"role":"user","content":"hi"}]}`)); serr != nil {
		t.Fatalf("short prompt rejected: %+v", serr)
	}
}

func TestCustomSettingsReasoningTiers(t *testing.T) {
	cm := CustomModel{
		Name: "tiered", InputTypes: []string{"text"},
		ReasoningTiers: []string{"low", "high"}, DefaultReasoning: "low",
	}
	out, tier, serr := applyCustomModelSettings(cm, []byte(`{"messages":[]}`))
	if serr != nil || tier != "low" {
		t.Fatalf("default tier = %q, err %+v", tier, serr)
	}
	var obj map[string]any
	obj = nil
	_ = json.Unmarshal(out, &obj)
	if obj["reasoning_effort"] != "low" {
		t.Fatalf("reasoning_effort = %v", obj["reasoning_effort"])
	}

	_, _, serr = applyCustomModelSettings(cm, []byte(`{"messages":[],"reasoning_effort":"medium"}`))
	if serr == nil || serr.code != "reasoning_tier_not_supported" {
		t.Fatalf("unsupported tier error = %+v", serr)
	}

	// No configured list: the client's tier passes through, xhigh becomes high
	// for the OpenAI-compatible upstream.
	free := CustomModel{Name: "free", InputTypes: []string{"text"}}
	out, tier, serr = applyCustomModelSettings(free, []byte(`{"messages":[],"reasoning_effort":"xhigh"}`))
	if serr != nil || tier != "xhigh" {
		t.Fatalf("free tier = %q, err %+v", tier, serr)
	}
	obj = nil
	_ = json.Unmarshal(out, &obj)
	if obj["reasoning_effort"] != "high" {
		t.Fatalf("openai effort = %v, want high", obj["reasoning_effort"])
	}

	out, tier, _ = applyCustomModelSettings(free, []byte(`{"messages":[],"reasoning_effort":"none"}`))
	if tier != "off" {
		t.Fatalf("none tier = %q, want off", tier)
	}
	obj = nil
	_ = json.Unmarshal(out, &obj)
	if _, ok := obj["reasoning_effort"]; ok {
		t.Fatalf("off tier left reasoning_effort: %v", obj)
	}
}
