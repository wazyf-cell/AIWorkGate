package main

import (
	"encoding/json"
	"testing"
)

// Anthropic reports cache reads/creations outside input_tokens; OpenAI's
// prompt_tokens is the whole billed input, so the cache counters must be added.
func TestAnthropicToOpenAIIncludesCacheTokens(t *testing.T) {
	body := []byte(`{
		"id":"msg_3","stop_reason":"end_turn",
		"content":[{"type":"text","text":"ok"}],
		"usage":{"input_tokens":7,"cache_creation_input_tokens":2,"cache_read_input_tokens":3,"output_tokens":4}
	}`)
	out, err := anthropicToOpenAI(body, "m")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var got struct {
		Usage struct {
			Prompt     int `json:"prompt_tokens"`
			Completion int `json:"completion_tokens"`
			Total      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Usage.Prompt != 12 || got.Usage.Completion != 4 || got.Usage.Total != 16 {
		t.Fatalf("usage = %+v, want prompt 12 completion 4 total 16", got.Usage)
	}
}
