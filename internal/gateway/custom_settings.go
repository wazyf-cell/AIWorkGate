package gateway

// Per-model request constraints for custom upstreams: output-token cap,
// approximate context check, input modalities and reasoning tiers. These run on
// the chat-shaped body after every inbound protocol has been converted, so the
// same rules apply to /v1/chat/completions, /v1/messages and /v1/responses.

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

type customRequestError struct {
	status int
	typ    string
	code   string
	msg    string
}

const estimateImageTokens = 1024

// applyCustomModelSettings enforces the model's configured limits and returns
// the chat body to send upstream plus the effective reasoning tier.
func applyCustomModelSettings(cm CustomModel, body []byte) ([]byte, string, *customRequestError) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return body, "", nil // downstream reports invalid_json as before
	}

	if !inputTypeAllowed(cm.InputTypes, "image") && chatContainsImage(obj) {
		return nil, "", &customRequestError{
			status: 400, typ: "invalid_request_error", code: "unsupported_input_type",
			msg: fmt.Sprintf("模型 %s 只接受文本输入，请求里包含图片", cm.Name),
		}
	}

	applyMaxOutput(obj, cm.MaxOutputTokens)

	tier, rerr := resolveReasoningTier(cm, obj)
	if rerr != nil {
		return nil, "", rerr
	}

	if cm.ContextWindow > 0 {
		if estimate := estimateChatTokens(obj); estimate > cm.ContextWindow {
			return nil, "", &customRequestError{
				status: 400, typ: "invalid_request_error", code: "context_length_exceeded",
				msg: fmt.Sprintf("估算输入约 %d tokens，超过模型 %s 的上下文上限 %d", estimate, cm.Name, cm.ContextWindow),
			}
		}
	}

	out, err := json.Marshal(obj)
	if err != nil {
		return body, tier, nil
	}
	return out, tier, nil
}

func inputTypeAllowed(types []string, want string) bool {
	if len(types) == 0 {
		return true
	}
	for _, item := range types {
		if strings.EqualFold(strings.TrimSpace(item), want) {
			return true
		}
	}
	return false
}

func applyMaxOutput(obj map[string]any, limit int) {
	if limit <= 0 {
		return
	}
	capped := func(key string) bool {
		value, ok := obj[key].(float64)
		if !ok {
			return false
		}
		if int(value) > limit || value <= 0 {
			obj[key] = limit
		}
		return true
	}
	hasCompletion := capped("max_completion_tokens")
	hasTokens := capped("max_tokens")
	if !hasCompletion && !hasTokens {
		obj["max_tokens"] = limit
	}
}

// resolveReasoningTier normalises the client's tier, enforces the model's
// allowed list and applies its default. The returned tier is "" when no
// reasoning setting should be sent upstream.
func resolveReasoningTier(cm CustomModel, obj map[string]any) (string, *customRequestError) {
	clientTier, present := chatReasoningTier(obj)
	if present {
		if normalized, ok := normalizeReasoningTier(clientTier); ok {
			clientTier = normalized
		} else {
			return "", &customRequestError{
				status: 400, typ: "invalid_request_error", code: "invalid_reasoning_tier",
				msg: fmt.Sprintf("不支持的推理档位 %q", clientTier),
			}
		}
	}
	if len(cm.ReasoningTiers) > 0 {
		if present && !tierAllowed(cm.ReasoningTiers, clientTier) {
			return "", &customRequestError{
				status: 400, typ: "invalid_request_error", code: "reasoning_tier_not_supported",
				msg: fmt.Sprintf("模型 %s 不支持推理档位 %q（可选：%s）",
					cm.Name, clientTier, strings.Join(cm.ReasoningTiers, ", ")),
			}
		}
		if !present {
			clientTier = strings.TrimSpace(cm.DefaultReasoning)
		}
	}
	// The chat body is also the OpenAI-upstream request. xhigh/max collapse to
	// high because the OpenAI API only defines minimal/low/medium/high.
	switch clientTier {
	case "", "auto", "off":
		delete(obj, "reasoning_effort")
	case "xhigh", "max":
		obj["reasoning_effort"] = "high"
	default:
		obj["reasoning_effort"] = clientTier
	}
	return clientTier, nil
}

func chatReasoningTier(obj map[string]any) (string, bool) {
	if raw, ok := obj["reasoning_effort"].(string); ok && strings.TrimSpace(raw) != "" {
		return strings.TrimSpace(raw), true
	}
	if reasoning, ok := obj["reasoning"].(map[string]any); ok {
		if raw, ok := reasoning["effort"].(string); ok && strings.TrimSpace(raw) != "" {
			return strings.TrimSpace(raw), true
		}
	}
	return "", false
}

func normalizeReasoningTier(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "none", "disabled", "off":
		return "off", true
	case "auto", "dynamic", "default":
		return "auto", true
	case "minimal", "low", "medium", "high", "xhigh", "max":
		return strings.ToLower(strings.TrimSpace(raw)), true
	default:
		return "", false
	}
}

func tierAllowed(tiers []string, want string) bool {
	for _, tier := range tiers {
		if strings.EqualFold(strings.TrimSpace(tier), want) {
			return true
		}
	}
	return false
}

// chatContainsImage reports whether any message content part is an image.
func chatContainsImage(obj map[string]any) bool {
	messages, _ := obj["messages"].([]any)
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		parts, ok := message["content"].([]any)
		if !ok {
			continue
		}
		for _, partRaw := range parts {
			part, _ := partRaw.(map[string]any)
			if strings.EqualFold(jsonStringValue(part["type"]), "image_url") ||
				strings.EqualFold(jsonStringValue(part["type"]), "image") ||
				part["image_url"] != nil {
				return true
			}
		}
	}
	return false
}

func jsonStringValue(v any) string {
	text, _ := v.(string)
	return text
}

// estimateChatTokens is deliberately approximate: it exists to catch a request
// that is wildly over the configured window, not to bill tokens. ASCII counts
// 0.3, non-ASCII 1.0, each image 1024 and each message 4 overhead.
func estimateChatTokens(obj map[string]any) int {
	total := 0.0
	messages, _ := obj["messages"].([]any)
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		total += 4
		total += estimateContentTokens(message["content"])
		if calls, ok := message["tool_calls"].([]any); ok {
			for _, callRaw := range calls {
				call, _ := callRaw.(map[string]any)
				fn, _ := call["function"].(map[string]any)
				total += estimateTextTokens(jsonStringValue(fn["name"]))
				total += estimateTextTokens(jsonStringValue(fn["arguments"]))
			}
		}
	}
	if tools, ok := obj["tools"]; ok {
		if encoded, err := json.Marshal(tools); err == nil {
			total += estimateTextTokens(string(encoded))
		}
	}
	return int(math.Ceil(total))
}

func estimateContentTokens(content any) float64 {
	switch value := content.(type) {
	case string:
		return estimateTextTokens(value)
	case []any:
		total := 0.0
		for _, partRaw := range value {
			part, _ := partRaw.(map[string]any)
			switch strings.ToLower(jsonStringValue(part["type"])) {
			case "text", "input_text":
				total += estimateTextTokens(jsonStringValue(part["text"]))
			case "image_url", "image", "input_image":
				total += estimateImageTokens
			}
		}
		return total
	default:
		return 0
	}
}

func estimateTextTokens(text string) float64 {
	total := 0.0
	for _, r := range text {
		if r < 128 {
			total += 0.3
		} else {
			total += 1
		}
	}
	return total
}
