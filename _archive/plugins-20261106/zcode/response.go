// Package main — the response half of the zcode conversion.
//
// zcode serves Anthropic Messages, so the upstream body is a message object with
// typed content blocks and a usage object whose cache counters are reported
// outside input_tokens. The host speaks chat.completion, where all billed input
// is prompt_tokens.
package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// tokenUsage is the Anthropic usage block. Cache reads and creations are extra
// counters, not a breakdown of input_tokens, so they have to be added back.
type tokenUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// promptTokens is the full billed input: uncached plus cache writes plus reads.
func (u tokenUsage) promptTokens() int {
	return u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
}

func (u tokenUsage) openAI() map[string]any {
	prompt := u.promptTokens()
	return map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": u.OutputTokens,
		"total_tokens":      prompt + u.OutputTokens,
	}
}

// merge keeps the last non-zero value per counter: message_start carries the
// input side while message_delta carries the final output count.
func (u *tokenUsage) merge(other tokenUsage) {
	if other.InputTokens > 0 {
		u.InputTokens = other.InputTokens
	}
	if other.OutputTokens > 0 {
		u.OutputTokens = other.OutputTokens
	}
	if other.CacheCreationInputTokens > 0 {
		u.CacheCreationInputTokens = other.CacheCreationInputTokens
	}
	if other.CacheReadInputTokens > 0 {
		u.CacheReadInputTokens = other.CacheReadInputTokens
	}
}

// anthropicToOpenAI converts a complete (non-streaming) Anthropic response body
// into a chat.completion body.
func anthropicToOpenAI(body []byte, model string) ([]byte, error) {
	var res struct {
		ID         string `json:"id"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		Usage tokenUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, err
	}
	var text strings.Builder
	var toolCalls []map[string]any
	for _, c := range res.Content {
		switch c.Type {
		case "text":
			text.WriteString(c.Text)
		case "tool_use":
			args := "{}"
			if len(c.Input) > 0 {
				args = string(c.Input)
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   c.ID,
				"type": "function",
				"function": map[string]any{
					"name":      c.Name,
					"arguments": args,
				},
			})
		}
	}
	msg := map[string]any{"role": "assistant", "content": text.String()}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	out := map[string]any{
		"id":     firstNonEmpty(res.ID, "chatcmpl-zcode"),
		"object": "chat.completion",
		"model":  model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": mapStopReason(res.StopReason),
		}},
		"usage": res.Usage.openAI(),
	}
	return json.Marshal(out)
}

// mapStopReason translates an Anthropic stop_reason into OpenAI's finish_reason.
func mapStopReason(r string) string {
	switch r {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "end_turn", "stop_sequence", "":
		return "stop"
	default:
		return "stop"
	}
}

// openAIErrorBody renders an OpenAI-shaped error for a non-2xx upstream result.
func openAIErrorBody(status int, message string) []byte {
	if strings.TrimSpace(message) == "" {
		message = fmt.Sprintf("upstream returned HTTP %d", status)
	}
	b, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "upstream_error",
			"code":    status,
		},
	})
	return b
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
