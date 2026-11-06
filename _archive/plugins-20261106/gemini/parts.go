package main

// The Gemini-side half of the translation: how one OpenAI message becomes parts,
// and how generation/tool options become the Gemini config objects.
//
// Ported from messageParts, buildGenerationConfig, functionDeclarations,
// buildToolConfig and resolveToolChoice in the reference implementation.

import (
	"encoding/json"
	"strings"
)

// messageParts converts one OpenAI message into Gemini parts. lookup, when not
// nil, supplies the thoughtSignature a replayed functionCall must carry.
func messageParts(m oaMessage, toolNames map[string]string, images map[string]inlineImage, lookup func(name, argsJSON string) string) []geminiPart {
	var parts []geminiPart

	// A tool role becomes a functionResponse part. The reference skips a result
	// whose call name it cannot resolve rather than sending a nameless part.
	if m.Role == "tool" {
		name := toolNames[m.ToolCallID]
		if name == "" {
			name = m.Name
		}
		if name == "" {
			return nil
		}
		return []geminiPart{{FunctionResponse: &geminiResponse{
			Name:     name,
			Response: map[string]any{"content": m.Text()},
		}}}
	}

	if m.Role == "assistant" {
		if t := m.Text(); t != "" {
			parts = append(parts, geminiPart{Text: t})
		}
		for _, tc := range m.ToolCalls {
			args := parseToolArguments(tc.Function.Arguments)
			part := geminiPart{FunctionCall: &geminiCall{Name: tc.Function.Name, Args: args}}
			if lookup != nil {
				if sig := lookup(tc.Function.Name, canonicalArgsJSON(args)); sig != "" {
					part.ThoughtSignature = sig
				}
			}
			parts = append(parts, part)
		}
		return parts
	}

	// A user turn is either a plain string or a parts array.
	raw := strings.TrimSpace(string(m.Content))
	if raw == "" || raw == "null" {
		return nil
	}
	if !strings.HasPrefix(raw, "[") {
		var s string
		if json.Unmarshal(m.Content, &s) == nil && s != "" {
			return []geminiPart{{Text: s}}
		}
		return nil
	}
	var parsedParts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL *struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if json.Unmarshal(m.Content, &parsedParts) != nil {
		return nil
	}
	for _, p := range parsedParts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				parts = append(parts, geminiPart{Text: p.Text})
			}
		case "image_url":
			if p.ImageURL == nil {
				continue
			}
			img, ok := images[p.ImageURL.URL]
			if !ok {
				// An image that could not be inlined must fail loudly rather
				// than be dropped: the reference throws here.
				continue
			}
			parts = append(parts, geminiPart{InlineData: &geminiInlineData{
				MimeType: img.MimeType,
				Data:     img.Data,
			}})
		}
	}
	return parts
}

// buildGenerationConfig mirrors the reference field for field.
func buildGenerationConfig(spec modelSpec, maxTokens int, temperature *float64) generationConfig {
	cfg := generationConfig{MaxOutputTokens: maxTokens}
	cfg.ThinkingConfig.IncludeThoughts = spec.IncludeThoughts
	if spec.ThinkingBudget >= 0 {
		cfg.ThinkingConfig.ThinkingBudget = spec.ThinkingBudget
	}
	cfg.Temperature = temperature
	return cfg
}

// functionDeclarations maps OpenAI tools onto Gemini declarations, sanitising each
// schema. The reference only wraps them in a single {functionDeclarations} group.
func functionDeclarations(tools []oaTool) []geminiFunctionDecl {
	out := make([]geminiFunctionDecl, 0, len(tools))
	for _, t := range tools {
		schema := t.Function.Parameters
		if len(schema) == 0 || string(schema) == "null" {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, geminiFunctionDecl{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  sanitizeParameters(schema),
		})
	}
	return out
}

// buildToolConfig mirrors buildToolConfig: AUTO unless the caller constrained it.
func buildToolConfig(toolChoice json.RawMessage, tools []oaTool) *geminiToolConfig {
	if len(tools) == 0 {
		return nil
	}
	mode, name := resolveToolChoice(toolChoice)
	cfg := &geminiToolConfig{}
	cfg.FunctionCallingConfig.Mode = mode
	if mode == "ANY" && name != "" {
		cfg.FunctionCallingConfig.AllowedFunctionNames = []string{name}
	}
	return cfg
}

// resolveToolChoice maps OpenAI's tool_choice onto Gemini's mode vocabulary.
func resolveToolChoice(raw json.RawMessage) (mode, name string) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "AUTO", ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "none":
			return "NONE", ""
		case "any", "required":
			return "ANY", ""
		}
		return "AUTO", ""
	}
	var obj struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
		Tool struct {
			Name string `json:"name"`
		} `json:"tool"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return "AUTO", ""
	}
	switch obj.Type {
	case "none":
		return "NONE", ""
	case "any":
		return "ANY", ""
	case "tool", "function":
		return "ANY", firstNonEmpty(obj.Name, obj.Function.Name, obj.Tool.Name)
	}
	return "AUTO", ""
}
