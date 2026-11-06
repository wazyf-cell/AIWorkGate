// Package main — request parameters that the OpenAI and Anthropic shapes spell
// differently: tool_choice and the reasoning controls.
package main

import (
	"encoding/json"
	"strings"
)

// reasoningParams is what the host asks for on the OpenAI side. Anthropic
// carries both halves: thinking selects the mode, output_config selects a tier
// inside it.
type reasoningParams struct {
	// Thinking is the raw OpenAI "thinking" object, passed through when the
	// caller supplied one.
	Thinking json.RawMessage
	// Effort is the host's reasoning_effort, or "" when the caller did not ask.
	Effort string
}

// anthropicToolChoice maps OpenAI's tool_choice onto Anthropic's shape;
// forwarding the OpenAI value verbatim is rejected upstream. nil means "send
// nothing", which lets the model decide.
func anthropicToolChoice(raw json.RawMessage) map[string]any {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	var name string
	if json.Unmarshal(raw, &name) == nil {
		switch name {
		case "auto":
			return map[string]any{"type": "auto"}
		case "required":
			return map[string]any{"type": "any"}
		case "none":
			return map[string]any{"type": "none"}
		}
		return nil
	}
	var obj struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &obj) == nil && strings.TrimSpace(obj.Function.Name) != "" {
		return map[string]any{"type": "tool", "name": obj.Function.Name}
	}
	return nil
}

// applyThinking maps the host's reasoning controls onto the Anthropic thinking
// block:
//
//   - an explicit thinking object is forwarded whole, so a budget survives;
//   - otherwise an effort tier sends adaptive plus output_config{effort};
//   - nothing at all leaves both keys out, which lets the model default apply.
func applyThinking(out map[string]any, rp reasoningParams) {
	if raw := strings.TrimSpace(string(rp.Thinking)); raw != "" && raw != "null" {
		var thinking any
		if json.Unmarshal(rp.Thinking, &thinking) == nil && thinking != nil {
			out["thinking"] = thinking
			return
		}
	}
	if rp.Effort != "" {
		out["thinking"] = map[string]any{"type": "adaptive"}
		out["output_config"] = map[string]any{"effort": rp.Effort}
	}
}
