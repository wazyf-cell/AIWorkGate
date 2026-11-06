// Package main implements protocol conversion between the OpenAI wire format the
// host speaks and the Anthropic Messages format the MiniMax Code agent backend
// actually serves.
//
// Everything here is ported from the reference implementation's
// vendor/channel-pack/src/minimax-messages.ts (buildMinimaxMessagesPayload and
// serializeMinimaxMessages). The upstream endpoint is
// /mavis/api/v1/llm/v1/messages, so every request needs the OpenAI shape
// rewritten into Anthropic blocks before it goes out, and every response
// rewritten back.
//
// The differences from a plain Anthropic body that the reference pins down and
// this port keeps:
//   - tools are {name, description, input_schema} — there is no "type" wrapper;
//   - stop becomes stop_sequences;
//   - reasoning is carried by thinking{type:"disabled"|"adaptive"} and, when a
//     tier is given, output_config{effort};
//   - an image that could not be inlined must fail the request rather than be
//     silently dropped (the reference throws).
package main

import (
	"encoding/json"
	"strings"
)

// oaTool is one OpenAI tool declaration.
type oaTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// oaToolCall is one OpenAI tool call emitted by the model.
type oaToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// oaMessage is one OpenAI message. Content is either a JSON string or the
// multimodal parts array, so it is kept raw and decoded on demand.
type oaMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []oaToolCall    `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
	Name       string          `json:"name"`
}

// Text returns the message's plain text, or "" when the content is a parts
// array whose text pieces are collected.
func (m oaMessage) Text() string {
	raw := strings.TrimSpace(string(m.Content))
	if raw == "" || raw == "null" {
		return ""
	}
	if !strings.HasPrefix(raw, "[") {
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			return s
		}
		return ""
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(m.Content, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// reasoningParams is what the host asks for on the OpenAI side. MiniMax carries
// both halves on the Anthropic side: thinking selects the mode, output_config
// selects a tier inside it.
type reasoningParams struct {
	// Thinking is the raw OpenAI "thinking" value, passed through when the
	// caller supplied one. The reference maps effort "none"/"on" to
	// disabled/adaptive; a tier below is used when no explicit mode arrives.
	Thinking json.RawMessage
	// Effort is the host's reasoning_effort, or "" when the caller did not ask.
	Effort string
}

// openAIToAnthropic rewrites an OpenAI chat/completions body into the MiniMax
// Anthropic-shaped body.
//
// The mapping that matters for correctness:
//   - system/developer messages are hoisted into the top-level "system" string
//     (MiniMax takes a plain string, not the Anthropic block array);
//   - miniMax requires no max_tokens, so an absent value stays absent rather
//     than being invented (unlike zcode, whose upstream requires it);
//   - stop becomes stop_sequences;
//   - tool role becomes a user turn carrying a tool_result block; assistant
//     turns with tool_calls become text + tool_use blocks;
//   - tools[].function.parameters becomes input_schema.
func openAIToAnthropicWith(raw []byte, rp reasoningParams) ([]byte, error) {
	var req struct {
		Model            string          `json:"model"`
		Stream           bool            `json:"stream"`
		MaxTokens        int             `json:"max_tokens"`
		MaxCompletionTok int             `json:"max_completion_tokens"`
		Temperature      *float64        `json:"temperature"`
		TopP             *float64        `json:"top_p"`
		Stop             []string        `json:"stop"`
		ToolChoice       json.RawMessage `json:"tool_choice"`
		Messages         []oaMessage     `json:"messages"`
		Tools            []oaTool        `json:"tools"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	out := map[string]any{
		"model":  req.Model,
		"stream": true,
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if len(req.Tools) > 0 {
		if choice := anthropicToolChoice(req.ToolChoice); choice != nil {
			out["tool_choice"] = choice
		}
	}
	maxTok := req.MaxTokens
	if maxTok <= 0 {
		maxTok = req.MaxCompletionTok
	}
	if maxTok > 0 {
		out["max_tokens"] = maxTok
	}
	if len(req.Stop) > 0 {
		out["stop_sequences"] = req.Stop
	}

	var system []string
	var msgs []any
	for _, m := range req.Messages {
		if m.Role == "system" || m.Role == "developer" {
			if text := m.Text(); text != "" {
				system = append(system, text)
			}
			continue
		}
		msgs = append(msgs, openAIMsgToAnthropic(m))
	}
	if len(system) > 0 {
		out["system"] = strings.Join(dedupeStrings(system), "\n\n")
	}
	out["messages"] = msgs

	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			schema := t.Function.Parameters
			if len(schema) == 0 || string(schema) == "null" {
				schema = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			// The reference sends exactly these three keys; a "type" wrapper
			// would be rejected.
			tools = append(tools, map[string]any{
				"name":         t.Function.Name,
				"description":  t.Function.Description,
				"input_schema": json.RawMessage(schema),
			})
		}
		out["tools"] = tools
	}
	applyThinking(out, rp)
	return json.Marshal(out)
}

// anthropicToolChoice maps OpenAI's tool_choice onto MiniMax's Anthropic shape;
// forwarding the OpenAI value verbatim is rejected upstream.
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

// applyThinking mirrors buildMinimaxMessagesPayload's thinking block:
//
//   - an explicit disabled/none request sends {"type":"disabled"};
//   - an explicit on request sends {"type":"adaptive"} with no tier;
//   - a tier (effort) sends adaptive plus output_config{effort};
//   - nothing at all leaves both keys out, which lets the model default apply.
func applyThinking(out map[string]any, rp reasoningParams) {
	if raw := strings.TrimSpace(string(rp.Thinking)); raw != "" && raw != "null" {
		var thinking map[string]any
		if json.Unmarshal(rp.Thinking, &thinking) == nil {
			if t, _ := thinking["type"].(string); t != "" {
				out["thinking"] = map[string]any{"type": t}
				return
			}
		}
	}
	if rp.Effort != "" {
		out["thinking"] = map[string]any{"type": "adaptive"}
		out["output_config"] = map[string]any{"effort": rp.Effort}
	}
}
