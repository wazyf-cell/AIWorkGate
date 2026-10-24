package main

import (
	"encoding/json"
	"testing"
)

func TestOpenAIToAnthropicKeepsSamplingAndToolChoice(t *testing.T) {
	raw := []byte(`{"model":"MiniMax-M3","stream":false,"temperature":0,"top_p":0.5,
		"tool_choice":"required",
		"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],
		"messages":[{"role":"user","content":"hi"}]}`)
	out, err := openAIToAnthropicWith(raw, reasoningParams{})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, ok := body["temperature"]; !ok || got != float64(0) {
		t.Fatalf("temperature = %v (present=%v), want 0", got, ok)
	}
	if got := body["top_p"]; got != 0.5 {
		t.Fatalf("top_p = %v, want 0.5", got)
	}
	choice, _ := body["tool_choice"].(map[string]any)
	if choice == nil || choice["type"] != "any" {
		t.Fatalf("tool_choice = %v, want type=any", body["tool_choice"])
	}
}
