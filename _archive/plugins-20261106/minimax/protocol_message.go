package main

import (
	"encoding/json"
	"strings"
)

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
			blocks = append(blocks, map[string]any{
				"type":  "tool_use",
				"id":    tc.ID,
				"name":  tc.Function.Name,
				"input": parseToolArguments(tc.Function.Arguments),
			})
		}
		return map[string]any{"role": "assistant", "content": blocks}
	}
	if blocks, ok := m.contentBlocks(); ok {
		return map[string]any{"role": m.Role, "content": blocks}
	}
	return map[string]any{"role": m.Role, "content": m.Text()}
}

// contentBlocks converts OpenAI multimodal parts into Anthropic blocks. A
// data-URL image becomes a base64 image block. A remote http image cannot be
// inlined, and the reference refuses to send a request that silently lost it,
// so the caller is told via the second result.
func (m oaMessage) contentBlocks() ([]any, bool) {
	raw := strings.TrimSpace(string(m.Content))
	if !strings.HasPrefix(raw, "[") {
		return nil, false
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL *struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if json.Unmarshal(m.Content, &parts) != nil {
		return nil, false
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
			if len(data) > maxImageBytesInline {
				return nil, false
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
		return nil, false
	}
	return blocks, true
}

// parseToolArguments decodes a tool call's JSON arguments into an object. A
// malformed or non-object value degrades to {} — never invent arguments.
func parseToolArguments(raw string) map[string]any {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return map[string]any{}
	}
	var parsed map[string]any
	if json.Unmarshal([]byte(raw), &parsed) != nil || parsed == nil {
		return map[string]any{}
	}
	return parsed
}

// dedupeStrings keeps the first occurrence of each non-empty value, matching
// the reference's [...new Set(...)] over the joined system text.
func dedupeStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
