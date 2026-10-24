package main

import (
	"encoding/json"
	"testing"
)

func decodeObject(t *testing.T, text string) map[string]any {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return raw
}

func TestThinkingTiersCatalogShapes(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{"thinking_config efforts object",
			`{"thinking_config":{"disabled":{},"enabled":{"efforts":{"high":{},"low":{},"max":{}}}}}`,
			"off low high max"},
		{"thinking_config efforts array",
			`{"thinking_config":{"enabled":{"efforts":[{"name":"medium"},{"name":"xhigh"}]}}}`,
			"medium xhigh"},
		{"thinking_config without efforts",
			`{"thinking_config":{"disabled":{},"enabled":{"is_default":true}}}`,
			"off on"},
		{"supported_efforts",
			`{"supported_efforts":["low","high","max"]}`,
			"low high max"},
		{"reasoning.supportedEfforts",
			`{"reasoning":{"supportedEfforts":["medium"],"defaultEffort":"medium"}}`,
			"medium"},
		{"capabilities.efforts",
			`{"capabilities":{"efforts":["low","max"]}}`,
			"low max"},
		{"no tiers",
			`{"reasoning":{"supportedEfforts":[]},"thinking_config":{"disabled":{}}}`,
			"off"},
	}
	for _, tc := range cases {
		got := thinkingTiers(decodeObject(t, tc.text))
		if joinTiers(got) != tc.want {
			t.Errorf("%s: thinkingTiers = %v, want %q", tc.name, got, tc.want)
		}
	}
}

func joinTiers(in []string) string {
	out := ""
	for i, tier := range in {
		if i > 0 {
			out += " "
		}
		out += tier
	}
	return out
}

func TestCatalogNumericAndCapabilityFields(t *testing.T) {
	zeroPrice := decodeObject(t, `{"price_factor":0}`)
	if factor, free := priceFactor(zeroPrice); factor != 0 || !free {
		t.Fatalf("zero price = %v,%v", factor, free)
	}
	rated := decodeObject(t, `{"price_factor":0.8,"is_free":false,"supports_image":true,"max_input_tokens":262144}`)
	factor, free := priceFactor(rated)
	if factor != 0.8 || free {
		t.Fatalf("price = %v,%v", factor, free)
	}
	if n := contextLength(rated); n != 262144 {
		t.Fatalf("context = %d", n)
	}
	if v, known := visionSupported(rated); !known || !v {
		t.Fatalf("vision = %v,%v", v, known)
	}

	tiered := decodeObject(t, `{"model_id":"glm-5.3-0001","thinking_config":{"enabled":{"efforts":{"high":{},"max":{}}}}}`)
	mi := parseRemoteModel(tiered, map[string]bool{})
	if mi == nil {
		t.Fatal("model dropped")
	}
	if joinTiers(mi.Thinking) != "high max" {
		t.Fatalf("thinking = %v", mi.Thinking)
	}
	if !mi.Tools {
		t.Fatal("tools must default to true when the catalog is silent")
	}
	if mi.ContextLength != 0 {
		t.Fatalf("context invented: %d", mi.ContextLength)
	}
}

func TestParseRemoteModelPrefersCatalogContext(t *testing.T) {
	raw := decodeObject(t, `{"model_id":"GLM-5.2","context_config":{"200K":{},"1M":{"tokens":1048576}},"tool_call":false,"provider":"智谱"}`)
	mi := parseRemoteModel(raw, map[string]bool{})
	if mi == nil {
		t.Fatal("model dropped")
	}
	if mi.ContextLength != 1048576 {
		t.Fatalf("context = %d, want the largest advertised tier", mi.ContextLength)
	}
	if mi.Tools {
		t.Fatal("tool_call:false must be honoured")
	}
	if mi.Vendor != "智谱" {
		t.Fatalf("vendor = %q", mi.Vendor)
	}
}
