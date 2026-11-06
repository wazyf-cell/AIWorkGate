package main

import (
	"encoding/json"
	"testing"
)

// Anthropic rejects the OpenAI tool_choice spelling, so each form has to be
// rewritten, and only when tools were actually declared.
func TestOpenAIToAnthropicMapsToolChoice(t *testing.T) {
	tools := `"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]`
	cases := []struct{ name, choice, wantType, wantName string }{
		{"auto", `"auto"`, "auto", ""},
		{"required", `"required"`, "any", ""},
		{"none", `"none"`, "none", ""},
		{"function", `{"type":"function","function":{"name":"f"}}`, "tool", "f"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeConverted(t, `{"model":"m","messages":[{"role":"user","content":"x"}],`+tools+`,"tool_choice":`+tc.choice+`}`)
			choice, _ := got["tool_choice"].(map[string]any)
			if choice == nil || choice["type"] != tc.wantType {
				t.Fatalf("tool_choice = %v, want type %q", got["tool_choice"], tc.wantType)
			}
			if tc.wantName != "" && choice["name"] != tc.wantName {
				t.Fatalf("tool_choice name = %v, want %q", choice["name"], tc.wantName)
			}
		})
	}
	t.Run("without tools", func(t *testing.T) {
		got := decodeConverted(t, `{"model":"m","tool_choice":"required","messages":[{"role":"user","content":"x"}]}`)
		if _, ok := got["tool_choice"]; ok {
			t.Fatalf("tool_choice must not be sent without tools: %v", got["tool_choice"])
		}
	})
}

// Reasoning is carried by thinking{type} and, for a tier, output_config{effort}.
func TestOpenAIToAnthropicMapsReasoning(t *testing.T) {
	base := `"model":"m","messages":[{"role":"user","content":"x"}]`
	t.Run("explicit disabled", func(t *testing.T) {
		got := decodeConverted(t, `{`+base+`,"thinking":{"type":"disabled"}}`)
		thinking, _ := got["thinking"].(map[string]any)
		if thinking["type"] != "disabled" {
			t.Fatalf("thinking = %v, want disabled", got["thinking"])
		}
		if _, ok := got["output_config"]; ok {
			t.Fatalf("output_config must be absent: %v", got["output_config"])
		}
	})
	t.Run("explicit thinking keeps its budget", func(t *testing.T) {
		got := decodeConverted(t, `{`+base+`,"thinking":{"type":"enabled","budget_tokens":4096}}`)
		thinking, _ := got["thinking"].(map[string]any)
		if thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(4096) {
			t.Fatalf("thinking = %v, want enabled with budget 4096", got["thinking"])
		}
	})
	t.Run("effort tier", func(t *testing.T) {
		got := decodeConverted(t, `{`+base+`,"reasoning_effort":"high"}`)
		thinking, _ := got["thinking"].(map[string]any)
		if thinking["type"] != "adaptive" {
			t.Fatalf("thinking = %v, want adaptive", got["thinking"])
		}
		config, _ := got["output_config"].(map[string]any)
		if config["effort"] != "high" {
			t.Fatalf("output_config = %v, want effort high", got["output_config"])
		}
	})
	t.Run("absent", func(t *testing.T) {
		got := decodeConverted(t, `{`+base+`}`)
		if _, ok := got["thinking"]; ok {
			t.Fatalf("thinking must be absent: %v", got["thinking"])
		}
		if _, ok := got["output_config"]; ok {
			t.Fatalf("output_config must be absent: %v", got["output_config"])
		}
	})
}

// Sampling parameters are pointers on the OpenAI side: an explicit zero differs
// from "not sent", and Anthropic rejects temperature=0 only if it is omitted.
func TestOpenAIToAnthropicKeepsSamplingParams(t *testing.T) {
	got := decodeConverted(t, `{"model":"m","temperature":0,"top_p":0.5,"messages":[{"role":"user","content":"x"}]}`)
	if got["temperature"] != float64(0) {
		t.Fatalf("temperature = %v (present=%v), want 0", got["temperature"], got["temperature"] != nil)
	}
	if got["top_p"] != 0.5 {
		t.Fatalf("top_p = %v, want 0.5", got["top_p"])
	}
	bare := decodeConverted(t, `{"model":"m","messages":[{"role":"user","content":"x"}]}`)
	if _, ok := bare["temperature"]; ok {
		t.Fatalf("temperature must be absent: %v", bare["temperature"])
	}
	if _, ok := bare["top_p"]; ok {
		t.Fatalf("top_p must be absent: %v", bare["top_p"])
	}
}

func decodeConverted(t *testing.T, raw string) map[string]any {
	t.Helper()
	out, err := openAIToAnthropic([]byte(raw))
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}
