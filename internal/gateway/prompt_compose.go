package gateway

import (
	"encoding/json"
	"math"
	"strings"
)

// promptSections is the fixed order of the composition breakdown (D-55).
var promptSections = []string{"system", "tools", "memory", "history", "user_input", "other"}

// PromptSection is one slice of a request's estimated input tokens.
type PromptSection struct {
	Key    string  `json:"key"`
	Tokens int     `json:"tokens"`
	Share  float64 `json:"share"`
}

// PromptComposition is the six-section, estimated token split of a request
// body. Estimated is always true: no tokenizer runs here (D-55 chose the same
// char-weighted estimate the context-window guard uses).
type PromptComposition struct {
	TotalTokens int             `json:"total_tokens"`
	Sections    []PromptSection `json:"sections"`
	Estimated   bool            `json:"estimated"`
}

// AnalyzePrompt splits a JSON request body into system, tools, memory,
// history, user_input and other. Unknown top-level fields land in other, so the
// six sections always cover the whole body.
func AnalyzePrompt(body []byte) (PromptComposition, error) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return PromptComposition{}, err
	}
	buckets := map[string]float64{}
	consumed := map[string]bool{}

	for _, key := range []string{"system", "systemInstruction", "system_instruction", "instructions"} {
		if value, ok := obj[key]; ok {
			buckets["system"] += estimateMessageContent(value)
			consumed[key] = true
		}
	}
	for _, key := range []string{"tools", "functions"} {
		if value, ok := obj[key]; ok {
			buckets["tools"] += estimateJSONTokens(value)
			consumed[key] = true
		}
	}
	for _, key := range []string{"memory", "memories"} {
		if value, ok := obj[key]; ok {
			buckets["memory"] += estimateJSONTokens(value)
			consumed[key] = true
		}
	}
	for _, key := range []string{"messages", "input", "contents"} {
		if value, ok := obj[key]; ok {
			buckets = addPromptMessages(buckets, value)
			consumed[key] = true
		}
	}
	if value, ok := obj["prompt"]; ok {
		buckets["user_input"] += estimateJSONTokens(value)
		consumed["prompt"] = true
	}

	rest := map[string]any{}
	for key, value := range obj {
		if !consumed[key] {
			rest[key] = value
		}
	}
	if len(rest) > 0 {
		buckets["other"] += estimateJSONTokens(rest)
	}

	composition := PromptComposition{Estimated: true, Sections: make([]PromptSection, 0, len(promptSections))}
	for _, key := range promptSections {
		tokens := int(math.Ceil(buckets[key]))
		composition.TotalTokens += tokens
		composition.Sections = append(composition.Sections, PromptSection{Key: key, Tokens: tokens})
	}
	if composition.TotalTokens > 0 {
		for i := range composition.Sections {
			share := float64(composition.Sections[i].Tokens) / float64(composition.TotalTokens)
			composition.Sections[i].Share = math.Round(share*10000) / 10000
		}
	}
	return composition, nil
}

// addPromptMessages routes every message to system/history/user_input. The last
// user message is the current turn; earlier turns are history whatever their
// role (assistant, tool results, earlier user turns).
func addPromptMessages(buckets map[string]float64, raw any) map[string]float64 {
	if text, ok := raw.(string); ok {
		buckets["user_input"] += estimateTextTokens(text)
		return buckets
	}
	list, ok := raw.([]any)
	if !ok {
		buckets["user_input"] += estimateJSONTokens(raw)
		return buckets
	}
	lastUser := -1
	for i, rawMessage := range list {
		message, _ := rawMessage.(map[string]any)
		role := strings.ToLower(jsonStringValue(message["role"]))
		if role == "user" {
			lastUser = i
		}
	}
	for i, rawMessage := range list {
		message, _ := rawMessage.(map[string]any)
		tokens := 4 + estimateMessageTokens(message)
		role := strings.ToLower(jsonStringValue(message["role"]))
		switch {
		case role == "system" || role == "developer":
			buckets["system"] += tokens
		case i == lastUser:
			buckets["user_input"] += tokens
		default:
			buckets["history"] += tokens
		}
	}
	return buckets
}

func estimateMessageTokens(message map[string]any) float64 {
	total := estimateMessageContent(message["content"])
	total += estimateMessageContent(message["parts"])
	if calls, ok := message["tool_calls"].([]any); ok {
		for _, rawCall := range calls {
			call, _ := rawCall.(map[string]any)
			fn, _ := call["function"].(map[string]any)
			total += estimateTextTokens(jsonStringValue(fn["name"]))
			total += estimateTextTokens(jsonStringValue(fn["arguments"]))
		}
	}
	if fn, ok := message["function_call"].(map[string]any); ok {
		total += estimateTextTokens(jsonStringValue(fn["name"]))
		total += estimateTextTokens(jsonStringValue(fn["arguments"]))
	}
	total += estimateTextTokens(jsonStringValue(message["name"]))
	total += estimateMessageContent(message["output"])
	return total
}

// estimateMessageContent understands the spellings in use: a plain string, a
// content/parts array of blocks (OpenAI parts, Anthropic blocks, Gemini parts),
// or one Content object wrapping parts.
func estimateMessageContent(content any) float64 {
	switch value := content.(type) {
	case string:
		return estimateTextTokens(value)
	case []any:
		total := 0.0
		for _, rawPart := range value {
			switch part := rawPart.(type) {
			case string:
				total += estimateTextTokens(part)
			case map[string]any:
				if text, ok := part["text"].(string); ok {
					total += estimateTextTokens(text)
					continue
				}
				switch strings.ToLower(jsonStringValue(part["type"])) {
				case "image_url", "image", "input_image":
					total += estimateImageTokens
				case "tool_use":
					total += estimateTextTokens(jsonStringValue(part["name"]))
					total += estimateJSONTokens(part["input"])
				case "tool_result":
					total += estimateMessageContent(part["content"])
				default:
					total += estimateJSONTokens(part)
				}
			}
		}
		return total
	case map[string]any:
		if parts, ok := value["parts"]; ok {
			return estimateMessageContent(parts)
		}
		if text, ok := value["text"].(string); ok {
			return estimateTextTokens(text)
		}
		return estimateJSONTokens(value)
	default:
		return 0
	}
}

func estimateJSONTokens(value any) float64 {
	if value == nil {
		return 0
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return 0
	}
	return estimateTextTokens(string(encoded))
}
