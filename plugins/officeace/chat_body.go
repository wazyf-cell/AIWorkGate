package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// buildMaaSChatBody maps an OpenAI chat request onto the Huawei MaaS body.
func (p *Plugin) buildMaaSChatBody(req *chatRequest) ([]byte, int, int, int, error) {
	vision, _ := p.modelVision(req.Model)
	var ruleHits, stripHits int
	strippedImages := 0

	messages := make([]map[string]any, 0, len(req.Messages))
	for _, raw := range req.Messages {
		var msg map[string]any
		if err := json.Unmarshal(raw, &msg); err != nil {
			return nil, 0, 0, 0, fmt.Errorf("invalid message: %w", err)
		}
		if content, ok := msg["content"]; ok && content != nil {
			switch v := content.(type) {
			case string:
				out, rh, sh := p.applyRules(v)
				ruleHits += rh
				stripHits += sh
				msg["content"] = out
			case []any:
				parts := make([]any, 0, len(v))
				for _, item := range v {
					part, ok := item.(map[string]any)
					if !ok {
						parts = append(parts, item)
						continue
					}
					typ, _ := part["type"].(string)
					if typ == "image_url" || typ == "image" {
						if !vision {
							strippedImages++
							parts = append(parts, map[string]any{
								"type": "text",
								"text": "[image omitted: model does not advertise vision support]",
							})
							continue
						}
					}
					if typ == "text" {
						if text, ok := part["text"].(string); ok {
							out, rh, sh := p.applyRules(text)
							ruleHits += rh
							stripHits += sh
							part["text"] = out
						}
					}
					parts = append(parts, part)
				}
				msg["content"] = parts
			}
		}
		messages = append(messages, msg)
	}

	out := map[string]any{
		"model":    req.Model,
		"messages": messages,
		"stream":   true, // MaaS is streamed; non-stream clients are aggregated locally
	}
	if len(req.Tools) > 0 && string(req.Tools) != "null" {
		var tools any
		if err := json.Unmarshal(req.Tools, &tools); err != nil {
			return nil, 0, 0, 0, fmt.Errorf("invalid tools: %w", err)
		}
		out["tools"] = tools
	}
	// MaaS documents tool_choice, stream_options, n, the two penalty coefficients
	// and seed as OpenAI-compatible chat parameters, so a caller-supplied value is
	// forwarded verbatim. Absent fields are never synthesized.
	for _, fwd := range []struct {
		key string
		raw json.RawMessage
	}{
		{"tool_choice", req.ToolChoice},
		{"stream_options", req.StreamOptions},
	} {
		var v any
		if len(fwd.raw) == 0 || string(fwd.raw) == "null" {
			continue
		}
		if err := json.Unmarshal(fwd.raw, &v); err != nil {
			return nil, 0, 0, 0, fmt.Errorf("invalid %s: %w", fwd.key, err)
		}
		out[fwd.key] = v
	}
	if req.N != nil {
		out["n"] = *req.N
	}
	if req.PresencePenalty != nil {
		out["presence_penalty"] = *req.PresencePenalty
	}
	if req.FrequencyPenalty != nil {
		out["frequency_penalty"] = *req.FrequencyPenalty
	}
	if req.Seed != nil {
		out["seed"] = *req.Seed
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if req.MaxTokens != nil {
		out["max_tokens"] = *req.MaxTokens
	} else if req.MaxCompletionTokens != nil {
		out["max_tokens"] = *req.MaxCompletionTokens
	}
	if len(req.Stop) > 0 && string(req.Stop) != "null" {
		var stop any
		if err := json.Unmarshal(req.Stop, &stop); err == nil {
			out["stop"] = stop
		}
	}
	if len(req.ResponseFormat) > 0 && string(req.ResponseFormat) != "null" {
		var rf any
		if err := json.Unmarshal(req.ResponseFormat, &rf); err == nil {
			out["response_format"] = rf
		}
	}
	// Forward thinking only when the caller asked for it; never invent it.
	if len(req.Thinking) > 0 && string(req.Thinking) != "null" {
		var thinking any
		if err := json.Unmarshal(req.Thinking, &thinking); err == nil {
			out["thinking"] = thinking
		}
	} else if req.ReasoningEffort != nil {
		if strings.EqualFold(strings.TrimSpace(*req.ReasoningEffort), "none") {
			out["thinking"] = map[string]any{"type": "disabled"}
		} else {
			out["thinking"] = map[string]any{"type": "enabled"}
		}
	}
	b, err := json.Marshal(out)
	return b, strippedImages, ruleHits, stripHits, err
}
