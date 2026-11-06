// Package main implements protocol conversion between the OpenAI wire format the
// host speaks and the Anthropic Messages format zcode.z.ai actually serves.
//
// Ported from the reference implementation's gateway/openai.go
// (workbuddy2api-panel, MIT). The upstream endpoint is
// /api/v1/zcode-plan/anthropic/v1/messages, so every request needs the OpenAI
// shape rewritten into Anthropic blocks before it goes out, and every response
// rewritten back.
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

// openAIToAnthropic rewrites an OpenAI chat/completions body into an Anthropic
// Messages body.
//
// The mapping that matters for correctness:
//   - system/developer messages are hoisted out of the conversation into the
//     top-level "system" block; Anthropic rejects them inline.
//   - OpenAI has no max_tokens default; Anthropic requires the field, so an
//     absent value becomes 4096.
//   - stop becomes stop_sequences.
//   - tool role becomes a user turn carrying a tool_result block; assistant
//     turns with tool_calls become text + tool_use blocks.
//   - tools[].function.parameters becomes input_schema, and tool_choice is
//     rewritten because Anthropic rejects the OpenAI spelling.
//   - temperature/top_p stay pointers so an explicit 0 is not lost.
//   - reasoning_effort and thinking become thinking/output_config.
func openAIToAnthropic(raw []byte) ([]byte, error) {
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
		Thinking         json.RawMessage `json:"thinking"`
		ReasoningEffort  string          `json:"reasoning_effort"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	out := map[string]any{
		"model":  req.Model,
		"stream": req.Stream,
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	maxTok := req.MaxTokens
	if maxTok <= 0 {
		maxTok = req.MaxCompletionTok
	}
	if maxTok > 0 {
		out["max_tokens"] = maxTok
	} else {
		out["max_tokens"] = 4096
	}
	if len(req.Stop) > 0 {
		out["stop_sequences"] = req.Stop
	}

	var system []any
	var msgs []any
	for _, m := range req.Messages {
		if m.Role == "system" || m.Role == "developer" {
			if text := m.Text(); text != "" {
				system = append(system, map[string]any{"type": "text", "text": text})
			}
			continue
		}
		msgs = append(msgs, openAIMsgToAnthropic(m))
	}
	if len(system) > 0 {
		out["system"] = system
	}
	out["messages"] = msgs

	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			schema := t.Function.Parameters
			if len(schema) == 0 || string(schema) == "null" {
				schema = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			tools = append(tools, map[string]any{
				"name":         t.Function.Name,
				"description":  t.Function.Description,
				"input_schema": json.RawMessage(schema),
			})
		}
		out["tools"] = tools
		if choice := anthropicToolChoice(req.ToolChoice); choice != nil {
			out["tool_choice"] = choice
		}
	}
	applyThinking(out, reasoningParams{Thinking: req.Thinking, Effort: req.ReasoningEffort})
	return json.Marshal(out)
}

// openAIMsgToAnthropic converts one non-system message.
func openAIMsgToAnthropic(m oaMessage) map[string]any {
	if m.Role == "tool" {
		return map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type":        "tool_result",
				"tool_use_id": m.ToolCallID,
				"content":     m.Text(),
			}},
		}
	}
	if m.Role == "assistant" && len(m.ToolCalls) > 0 {
		var blocks []any
		if t := m.Text(); t != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": t})
		}
		for _, tc := range m.ToolCalls {
			var input any
			if json.Unmarshal([]byte(tc.Function.Arguments), &input) != nil || input == nil {
				input = map[string]any{}
			}
			blocks = append(blocks, map[string]any{
				"type":  "tool_use",
				"id":    tc.ID,
				"name":  tc.Function.Name,
				"input": input,
			})
		}
		return map[string]any{"role": "assistant", "content": blocks}
	}
	if blocks := m.contentBlocks(); blocks != nil {
		return map[string]any{"role": m.Role, "content": blocks}
	}
	return map[string]any{"role": m.Role, "content": m.Text()}
}

// contentBlocks converts OpenAI multimodal parts into Anthropic blocks.
// A data-URL image becomes a base64 image block; a remote http image is skipped
// because Anthropic requires either base64 or a provider-specific file id.
// Returning nil means "plain string content", which the caller uses instead.
func (m oaMessage) contentBlocks() []any {
	raw := strings.TrimSpace(string(m.Content))
	if !strings.HasPrefix(raw, "[") {
		return nil
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL *struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if json.Unmarshal(m.Content, &parts) != nil {
		return nil
	}
	var blocks []any
	for _, p := range parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": p.Text})
			}
		case "image_url":
			if p.ImageURL == nil {
				continue
			}
			media, data, ok := strings.Cut(p.ImageURL.URL, ",")
			if !ok || !strings.HasPrefix(media, "data:") {
				continue
			}
			mediaType := strings.TrimPrefix(media, "data:")
			if i := strings.Index(mediaType, ";"); i >= 0 {
				mediaType = mediaType[:i]
			}
			blocks = append(blocks, map[string]any{
				"type": "image",
				"source": map[string]any{
					"type":       "base64",
					"media_type": mediaType,
					"data":       data,
				},
			})
		}
	}
	if len(blocks) == 0 {
		return nil
	}
	return blocks
}
