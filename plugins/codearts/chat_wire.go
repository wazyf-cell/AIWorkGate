package main

import (
	"encoding/json"
	"regexp"
	"strings"
)

type chatMessage struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`
	Name             string          `json:"name,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	ToolCalls        []wireToolCall  `json:"tool_calls,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
}

type wireToolCall struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

type chatRequest struct {
	Model               string         `json:"model"`
	Messages            []chatMessage  `json:"messages"`
	Stream              bool           `json:"stream"`
	StreamOptions       *streamOptions `json:"stream_options"`
	Tools               []chatTool     `json:"tools"`
	MaxTokens           *int           `json:"max_tokens"`
	MaxCompletionTokens *int           `json:"max_completion_tokens"`
	ReasoningEffort     string         `json:"reasoning_effort"`
	Thinking            map[string]any `json:"thinking"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type upstreamBody struct {
	Model            string           `json:"model"`
	Messages         []map[string]any `json:"messages"`
	Stream           bool             `json:"stream"`
	PromptCacheKey   string           `json:"prompt_cache_key"`
	Include          []string         `json:"include"`
	ReasoningSummary string           `json:"reasoning_summary"`
	Thinking         map[string]any   `json:"thinking,omitempty"`
	ToolStream       bool             `json:"tool_stream"`
	MaxTokens        int              `json:"max_tokens"`
	Tools            []map[string]any `json:"tools,omitempty"`
}

var deepseekV4Pattern = regexp.MustCompile(`^deepseek-v4-(flash|pro)$`)

func needsDsmlToolMode(model string) bool { return deepseekV4Pattern.MatchString(model) }

func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, part := range parts {
			if part.Type == "text" && part.Text != "" {
				b.WriteString(part.Text)
			}
		}
		return b.String()
	}
	return ""
}

// buildWireMessages flattens OpenAI content into text-only messages (CodeArts
// has no vision model), applies the host rule packs to every text block and
// preserves tool calls / results / reasoning content.
func (p *Plugin) buildWireMessages(req *chatRequest) ([]map[string]any, int, int) {
	out := make([]map[string]any, 0, len(req.Messages))
	ruleHits, stripHits := 0, 0
	for _, m := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		text := contentText(m.Content)
		if text != "" {
			var h, s int
			text, h, s = p.applyRules(text)
			ruleHits += h
			stripHits += s
		}
		switch role {
		case "system", "developer":
			if text != "" {
				out = append(out, map[string]any{"role": "system", "content": text})
			}
		case "assistant":
			calls := make([]map[string]any, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				calls = append(calls, map[string]any{
					"id":   tc.ID,
					"type": "function",
					"function": map[string]any{
						"name":      tc.Function.Name,
						"arguments": tc.Function.Arguments,
					},
				})
			}
			if text == "" && len(calls) == 0 {
				continue
			}
			msg := map[string]any{
				"role":              "assistant",
				"content":           text,
				"reasoning_content": m.ReasoningContent,
			}
			if len(calls) > 0 {
				msg["tool_calls"] = calls
			}
			out = append(out, msg)
		case "tool":
			content := text
			if strings.TrimSpace(content) == "" {
				content = "(no output)"
			}
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": m.ToolCallID,
				"content":      content,
			})
		default:
			if text != "" {
				out = append(out, map[string]any{"role": "user", "content": text})
			}
		}
	}
	return out, ruleHits, stripHits
}

func convertTools(tools []chatTool) []map[string]any {
	if len(tools) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		if strings.TrimSpace(t.Function.Name) == "" {
			continue
		}
		fn := map[string]any{"name": t.Function.Name}
		if t.Function.Description != "" {
			fn["description"] = t.Function.Description
		}
		if len(t.Function.Parameters) > 0 {
			var params any
			if err := json.Unmarshal(t.Function.Parameters, &params); err == nil {
				fn["parameters"] = params
			}
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

// applyDsmlMode injects the DSML system prompt immediately after the first
// system message (inserted at the front when none exists) and drops the native
// tools array, exactly like the reference adapter.
func applyDsmlMode(messages []map[string]any, tools []map[string]any) []map[string]any {
	idx := -1
	for i, m := range messages {
		if m["role"] == "system" {
			idx = i
			break
		}
	}
	prompt := buildDsmlSystemPrompt(tools)
	insertAt := idx + 1
	out := make([]map[string]any, 0, len(messages)+1)
	out = append(out, messages[:insertAt]...)
	out = append(out, map[string]any{"role": "system", "content": prompt})
	out = append(out, messages[insertAt:]...)
	return out
}

func buildUpstreamBody(req *chatRequest, model string, messages []map[string]any, tools []map[string]any, sessionID string) upstreamBody {
	thinking := req.Thinking
	effort := strings.ToLower(strings.TrimSpace(req.ReasoningEffort))
	if effort == "off" || effort == "none" || effort == "disabled" {
		thinking = map[string]any{"type": "disabled"}
	}
	if t, ok := thinking["type"].(string); ok && strings.EqualFold(t, "enabled") {
		thinking = nil
	}
	maxTokens := 65536
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		maxTokens = *req.MaxTokens
	} else if req.MaxCompletionTokens != nil && *req.MaxCompletionTokens > 0 {
		maxTokens = *req.MaxCompletionTokens
	}
	return upstreamBody{
		Model:            model,
		Messages:         messages,
		Stream:           true,
		PromptCacheKey:   sessionID,
		Include:          []string{"reasoning.encrypted_content"},
		ReasoningSummary: "auto",
		Thinking:         thinking,
		ToolStream:       true,
		MaxTokens:        maxTokens,
		Tools:            tools,
	}
}

func normalizeChatModel(model string) string {
	model = strings.TrimSpace(model)
	if strings.HasPrefix(model, "codearts/") {
		model = strings.TrimPrefix(model, "codearts/")
	}
	return model
}
