package gateway

import (
	"encoding/json"
	"strings"
)

// This file translates chat completions requests into native Gemini
// generateContent bodies. Responses live in upstream_gemini_stream.go.

type geminiPart struct {
	Text             string              `json:"text,omitempty"`
	Thought          bool                `json:"thought,omitempty"`
	InlineData       map[string]string   `json:"inlineData,omitempty"`
	FunctionCall     *geminiFunctionCall `json:"functionCall,omitempty"`
	FunctionResponse map[string]any      `json:"functionResponse,omitempty"`
}

type geminiFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}
type geminiContent struct {
	Role  string       `json:"role"`
	Parts []geminiPart `json:"parts"`
}
type geminiInputMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Name       string          `json:"name"`
	ToolCallID string          `json:"tool_call_id"`
	ToolCalls  []wireToolCall  `json:"tool_calls"`
}

type geminiInputItem struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL struct {
		URL string `json:"url"`
	} `json:"image_url"`
}

// chatToGeminiRequest builds a generateContent body. reasoningTier wins; when
// it is empty the body's reasoning_effort is used instead.
func chatToGeminiRequest(chatBody []byte, upstreamModel, reasoningTier string) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(chatBody, &top); err != nil {
		return nil, &envelopeError{"request body must be a JSON object"}
	}
	var messages []geminiInputMessage
	if err := json.Unmarshal(top["messages"], &messages); err != nil {
		return nil, &envelopeError{"messages must be an array"}
	}
	contents, system, err := chatToGeminiMessages(messages)
	if err != nil {
		return nil, err
	}
	if len(contents) == 0 {
		return nil, &envelopeError{"messages contain no user or assistant turns"}
	}
	body := map[string]any{"contents": contents}
	if system != "" {
		body["systemInstruction"] = map[string]any{"parts": []geminiPart{{Text: system}}}
	}
	tier := firstNonEmpty(reasoningTier, jsonString(top["reasoning_effort"]))
	if gen := chatToGeminiGeneration(top, tier); len(gen) > 0 {
		body["generationConfig"] = gen
	}
	if tools, ok := chatToGeminiTools(top["tools"]); ok {
		body["tools"] = tools
	}
	if config, ok := chatToGeminiToolConfig(top["tool_choice"]); ok {
		body["toolConfig"] = config
	}
	return json.Marshal(body)
}

// chatToGeminiMessages walks the history once, collecting system text, contents
// and the tool_call_id -> function name map needed to resolve tool results.
func chatToGeminiMessages(messages []geminiInputMessage) ([]geminiContent, string, error) {
	var contents []geminiContent
	var system []string
	names := map[string]string{}
	for _, msg := range messages {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		switch {
		case role == "system" || role == "developer":
			if text := chatToGeminiText(msg.Content); strings.TrimSpace(text) != "" {
				system = append(system, text)
			}
		case role == "tool":
			name := firstNonEmpty(names[msg.ToolCallID], msg.Name)
			if name == "" {
				return nil, "", &envelopeError{"tool message needs a function name resolved from a preceding tool_calls entry"}
			}
			contents = chatToGeminiAppend(contents, "user", []geminiPart{{FunctionResponse: map[string]any{
				"name":     name,
				"response": map[string]string{"content": chatToGeminiText(msg.Content)},
			}}})
		default:
			parts, err := chatToGeminiParts(msg.Content)
			if err != nil {
				return nil, "", err
			}
			target := "user"
			if role == "assistant" {
				target = "model"
				for _, call := range msg.ToolCalls {
					if call.Function.Name == "" {
						continue
					}
					if call.ID != "" {
						names[call.ID] = call.Function.Name
					}
					parts = append(parts, geminiPart{FunctionCall: &geminiFunctionCall{
						Name: call.Function.Name,
						Args: geminiArgsObject(call.Function.Arguments),
					}})
				}
			}
			contents = chatToGeminiAppend(contents, target, parts)
		}
	}
	return contents, strings.Join(system, "\n\n"), nil
}

// chatToGeminiAppend merges adjacent same-role turns, which Gemini requires.
func chatToGeminiAppend(contents []geminiContent, role string, parts []geminiPart) []geminiContent {
	if n := len(contents); n > 0 && contents[n-1].Role == role {
		contents[n-1].Parts = append(contents[n-1].Parts, parts...)
	} else if len(parts) > 0 {
		contents = append(contents, geminiContent{Role: role, Parts: parts})
	}
	return contents
}

func chatToGeminiText(raw json.RawMessage) string {
	parts, _ := chatToGeminiParts(raw)
	var out strings.Builder
	for _, part := range parts {
		out.WriteString(part.Text)
	}
	return out.String()
}

// chatToGeminiParts converts content parts, including data-URL images. Remote
// images are rejected rather than fetched: generateContent has no URL input.
func chatToGeminiParts(raw json.RawMessage) ([]geminiPart, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if text == "" {
			return nil, nil
		}
		return []geminiPart{{Text: text}}, nil
	}
	var items []geminiInputItem
	if json.Unmarshal(raw, &items) != nil {
		return nil, &envelopeError{"message content must be a string or an array of content parts"}
	}
	parts := make([]geminiPart, 0, len(items))
	for _, item := range items {
		switch item.Type {
		case "text":
			if item.Text != "" {
				parts = append(parts, geminiPart{Text: item.Text})
			}
		case "image_url":
			part, err := chatToGeminiImagePart(item.ImageURL.URL)
			if err != nil {
				return nil, err
			}
			parts = append(parts, part)
		}
	}
	return parts, nil
}

func chatToGeminiImagePart(url string) (geminiPart, error) {
	rest, ok := strings.CutPrefix(url, "data:")
	if !ok {
		return geminiPart{}, &envelopeError{"gemini generateContent requires images as data URLs (data:<mime>;base64,...); remote image URLs are not supported"}
	}
	meta, data, found := strings.Cut(rest, ",")
	mime, encoded := strings.CutSuffix(meta, ";base64")
	if !found || data == "" || !encoded || mime == "" {
		return geminiPart{}, &envelopeError{"image data URL is malformed; want data:<mime>;base64,<data>"}
	}
	return geminiPart{InlineData: map[string]string{"mimeType": mime, "data": data}}, nil
}

// geminiArgsObject turns function-call arguments into the JSON object Gemini
// expects. Both a quoted string and a bare object are accepted.
func geminiArgsObject(raw string) json.RawMessage {
	text := strings.TrimSpace(raw)
	if strings.HasPrefix(text, `"`) {
		text = strings.TrimSpace(jsonString(json.RawMessage(text)))
	}
	if !strings.HasPrefix(text, "{") || !json.Valid([]byte(text)) {
		return json.RawMessage("{}")
	}
	return json.RawMessage(text)
}

func chatToGeminiTools(raw json.RawMessage) ([]map[string]any, bool) {
	var tools []struct {
		Type     string `json:"type"`
		Function *struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &tools) != nil {
		return nil, false
	}
	decls := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		fn := tool.Function
		if fn == nil || fn.Name == "" || (tool.Type != "" && tool.Type != "function") {
			continue
		}
		decl := map[string]any{"name": fn.Name}
		if fn.Description != "" {
			decl["description"] = fn.Description
		}
		if len(fn.Parameters) > 0 && string(fn.Parameters) != "null" {
			decl["parameters"] = fn.Parameters
		}
		decls = append(decls, decl)
	}
	return []map[string]any{{"functionDeclarations": decls}}, len(decls) > 0
}

func chatToGeminiToolConfig(raw json.RawMessage) (map[string]any, bool) {
	var mode string
	if json.Unmarshal(raw, &mode) == nil {
		modes := map[string]string{"auto": "AUTO", "required": "ANY", "any": "ANY", "none": "NONE"}
		if mode, ok := modes[strings.ToLower(strings.TrimSpace(mode))]; ok {
			return map[string]any{"functionCallingConfig": map[string]any{"mode": mode}}, true
		}
		return nil, false
	}
	var choice struct {
		Type     string `json:"type"`
		Function *struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &choice) != nil || choice.Function == nil ||
		!strings.EqualFold(choice.Type, "function") || choice.Function.Name == "" {
		return nil, false
	}
	config := map[string]any{"mode": "ANY", "allowedFunctionNames": []string{choice.Function.Name}}
	return map[string]any{"functionCallingConfig": config}, true
}

// chatToGeminiGeneration maps sampling and reasoning fields. Reasoning tiers
// use Gemini's documented thinking budgets; off disables thinking entirely.
func chatToGeminiGeneration(top map[string]json.RawMessage, tier string) map[string]any {
	gen := map[string]any{}
	if raw := top["temperature"]; len(raw) > 0 && string(raw) != "null" {
		gen["temperature"] = raw
	}
	if raw := top["top_p"]; len(raw) > 0 && string(raw) != "null" {
		gen["topP"] = raw
	}
	for _, key := range []string{"max_completion_tokens", "max_tokens"} {
		var n int
		if raw := top[key]; len(raw) > 0 && json.Unmarshal(raw, &n) == nil && n > 0 {
			gen["maxOutputTokens"] = n
			break
		}
	}
	if raw := top["stop"]; len(raw) > 0 && string(raw) != "null" {
		stops := []string{}
		if json.Unmarshal(raw, &stops) != nil {
			stops = []string{jsonString(raw)}
		}
		if len(stops) > 0 && stops[0] != "" {
			gen["stopSequences"] = stops
		}
	}
	budgets := map[string]int{"minimal": 512, "low": 1024, "medium": 4096,
		"high": 8192, "xhigh": 16384, "max": 24576}
	switch tier = strings.ToLower(strings.TrimSpace(tier)); tier {
	case "off":
		gen["thinkingConfig"] = map[string]any{"thinkingBudget": 0}
	case "auto":
		gen["thinkingConfig"] = map[string]any{"thinkingBudget": -1, "includeThoughts": true}
	default:
		if budget, ok := budgets[tier]; ok {
			gen["thinkingConfig"] = map[string]any{"thinkingBudget": budget, "includeThoughts": true}
		}
	}
	return gen
}
