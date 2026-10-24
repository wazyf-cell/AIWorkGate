package gateway

import (
	"encoding/json"
	"net/url"
	"strings"
)

// Adapts the native Anthropic Messages API to the internal chat JSON.
// reasoningTier wins; an empty tier falls back to the body's reasoning_effort.

type chatToAnthropicMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"tool_call_id"`
	ToolCalls  []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

func chatToAnthropicRequest(chatBody []byte, upstreamModel, reasoningTier string) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(chatBody, &top); err != nil {
		return nil, &envelopeError{"request body must be a JSON object"}
	}
	var messages []chatToAnthropicMessage
	if raw, ok := top["messages"]; ok {
		if err := json.Unmarshal(raw, &messages); err != nil {
			return nil, &envelopeError{"messages must be an array"}
		}
	}
	converted, system, err := chatToAnthropicMessages(messages)
	if err != nil {
		return nil, err
	}
	if len(converted) == 0 {
		return nil, &envelopeError{"messages contain no user or assistant turns"}
	}
	model := strings.TrimSpace(firstNonEmpty(upstreamModel, jsonString(top["model"])))
	if model == "" {
		return nil, &envelopeError{"model is required"}
	}
	maxTokens := int(jsonNumber(top["max_tokens"], jsonNumber(top["max_completion_tokens"], 4096)))
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	tier := firstNonEmpty(reasoningTier, jsonString(top["reasoning_effort"]))
	request := map[string]any{"model": model, "messages": converted, "max_tokens": maxTokens}
	if system != "" {
		request["system"] = system
	}
	if raw, ok := top["stream"]; ok && string(raw) != "null" {
		var stream bool
		if json.Unmarshal(raw, &stream) == nil {
			request["stream"] = stream
		}
	}
	if tools, ok := chatToAnthropicTools(top["tools"]); ok {
		request["tools"] = tools
	}
	if choice, ok := chatToAnthropicToolChoice(top["tool_choice"]); ok {
		request["tool_choice"] = choice
	}
	if stops := chatToAnthropicStops(top); len(stops) > 0 {
		request["stop_sequences"] = stops
	}
	if thinking, ok := chatToAnthropicThinking(tier, maxTokens); ok {
		request["thinking"] = thinking
		request["temperature"] = 1 // Anthropic requires temperature 1 with thinking.
	} else {
		for _, key := range []string{"temperature", "top_p"} {
			if value := jsonNumber(top[key], -1); value >= 0 {
				request[key] = value
			}
		}
	}
	return json.Marshal(request)
}

// chatToAnthropicMessages walks the history once, collecting system text and
// merging adjacent same-role turns because Anthropic requires alternation.
func chatToAnthropicMessages(messages []chatToAnthropicMessage) ([]anthropicOutMessage, string, error) {
	var out []anthropicOutMessage
	var system []string
	for _, msg := range messages {
		switch strings.ToLower(strings.TrimSpace(msg.Role)) {
		case "system", "developer":
			if text := chatToAnthropicText(msg.Content); strings.TrimSpace(text) != "" {
				system = append(system, text)
			}
		case "assistant":
			blocks, err := chatToAnthropicBlocks(msg.Content)
			if err != nil {
				return nil, "", err
			}
			for _, call := range msg.ToolCalls {
				if call.Function.Name == "" {
					continue
				}
				input, err := chatToAnthropicToolInput(call.Function.Arguments)
				if err != nil {
					return nil, "", err
				}
				blocks = append(blocks, anthropicBlock{
					Type: "tool_use", ID: call.ID, Name: call.Function.Name, Input: mustJSON(input),
				})
			}
			out = chatToAnthropicAppend(out, "assistant", blocks)
		case "tool":
			out = chatToAnthropicAppend(out, "user", []anthropicBlock{{
				Type: "tool_result", ToolUseID: msg.ToolCallID, Content: chatToAnthropicText(msg.Content),
			}})
		default:
			blocks, err := chatToAnthropicBlocks(msg.Content)
			if err != nil {
				return nil, "", err
			}
			out = chatToAnthropicAppend(out, "user", blocks)
		}
	}
	return out, strings.Join(system, "\n\n"), nil
}

func chatToAnthropicAppend(out []anthropicOutMessage, role string, blocks []anthropicBlock) []anthropicOutMessage {
	if len(blocks) == 0 {
		return out
	}
	if n := len(out); n > 0 && out[n-1].Role == role {
		out[n-1].Content = append(out[n-1].Content, blocks...)
		return out
	}
	return append(out, anthropicOutMessage{Role: role, Content: blocks})
}
func chatToAnthropicText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []map[string]json.RawMessage
	json.Unmarshal(raw, &parts)
	var out strings.Builder
	for _, part := range parts {
		if jsonString(part["type"]) == "text" {
			out.WriteString(jsonString(part["text"]))
		}
	}
	return out.String()
}
func chatToAnthropicBlocks(raw json.RawMessage) ([]anthropicBlock, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if text == "" {
			return nil, nil
		}
		return []anthropicBlock{{Type: "text", Text: text}}, nil
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return nil, &envelopeError{"message content must be a string or an array of content parts"}
	}
	blocks := make([]anthropicBlock, 0, len(parts))
	for _, part := range parts {
		switch jsonString(part["type"]) {
		case "text":
			if text := jsonString(part["text"]); text != "" {
				blocks = append(blocks, anthropicBlock{Type: "text", Text: text})
			}
		case "image_url":
			block, err := chatToAnthropicImage(part["image_url"])
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, block)
		}
	}
	return blocks, nil
}

func chatToAnthropicImage(raw json.RawMessage) (anthropicBlock, error) {
	var address string
	if json.Unmarshal(raw, &address) != nil {
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) != nil {
			return anthropicBlock{}, &envelopeError{"image_url part is malformed"}
		}
		address = jsonString(obj["url"])
	}
	address = strings.TrimSpace(address)
	if rest, ok := strings.CutPrefix(address, "data:"); ok {
		meta, data, found := strings.Cut(rest, ",")
		mime, encoded := strings.CutSuffix(meta, ";base64")
		if !found || data == "" || !encoded || mime == "" {
			return anthropicBlock{}, &envelopeError{"image data URL is malformed; want data:<mime>;base64,<data>"}
		}
		return anthropicBlock{Type: "image", Source: map[string]any{"type": "base64", "media_type": mime, "data": data}}, nil
	}
	parsed, err := url.Parse(address)
	if err != nil || parsed.Host == "" ||
		(!strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https")) {
		return anthropicBlock{}, &envelopeError{"image URL must be a data URL or an absolute http(s) URL"}
	}
	return anthropicBlock{Type: "image", Source: map[string]any{"type": "url", "url": address}}, nil
}
func chatToAnthropicToolInput(raw json.RawMessage) (map[string]any, error) {
	text := strings.TrimSpace(string(raw))
	var quoted string
	if json.Unmarshal(raw, &quoted) == nil {
		text = strings.TrimSpace(quoted)
	}
	if text == "" || text == "null" {
		return map[string]any{}, nil
	}
	var input map[string]any
	if json.Unmarshal([]byte(text), &input) != nil || input == nil {
		return nil, &envelopeError{"tool call arguments must be a JSON object"}
	}
	return input, nil
}

func chatToAnthropicStops(top map[string]json.RawMessage) []string {
	raw := top["stop"]
	if len(raw) == 0 || string(raw) == "null" {
		raw = top["stop_sequences"]
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		if one == "" {
			return nil
		}
		return []string{one}
	}
	var many []string
	if json.Unmarshal(raw, &many) != nil {
		return nil
	}
	out := many[:0]
	for _, stop := range many {
		if stop != "" {
			out = append(out, stop)
		}
	}
	return out
}

func chatToAnthropicTools(raw json.RawMessage) ([]map[string]any, bool) {
	var tools []map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &tools) != nil {
		return nil, false
	}
	out := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		var function map[string]json.RawMessage
		if json.Unmarshal(tool["function"], &function) != nil {
			continue
		}
		name := jsonString(function["name"])
		kind := strings.ToLower(jsonString(tool["type"]))
		if name == "" || (kind != "" && kind != "function") {
			continue
		}
		entry := map[string]any{"name": name}
		if description := jsonString(function["description"]); description != "" {
			entry["description"] = description
		}
		schema := function["parameters"]
		if len(schema) == 0 || string(schema) == "null" {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		entry["input_schema"] = schema
		out = append(out, entry)
	}
	return out, len(out) > 0
}
func chatToAnthropicToolChoice(raw json.RawMessage) (map[string]any, bool) {
	var mode string
	if json.Unmarshal(raw, &mode) == nil {
		modes := map[string]string{"auto": "auto", "required": "any", "any": "any", "none": "none"}
		if kind, ok := modes[strings.ToLower(strings.TrimSpace(mode))]; ok {
			return map[string]any{"type": kind}, true
		}
		return nil, false
	}
	var choice map[string]json.RawMessage
	if json.Unmarshal(raw, &choice) != nil || strings.ToLower(jsonString(choice["type"])) != "function" {
		return nil, false
	}
	var function map[string]json.RawMessage
	if json.Unmarshal(choice["function"], &function) != nil {
		return nil, false
	}
	name := jsonString(function["name"])
	if name == "" {
		return nil, false
	}
	return map[string]any{"type": "tool", "name": name}, true
}
