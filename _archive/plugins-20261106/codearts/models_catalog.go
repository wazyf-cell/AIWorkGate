package main

import (
	"strings"
)

// normalizeModelID strips the "-NNNN" build suffix the catalog appends.
func normalizeModelID(id string) string {
	if len(id) > 5 {
		suffix := id[len(id)-5:]
		if suffix[0] == '-' {
			allDigits := true
			for _, r := range suffix[1:] {
				if r < '0' || r > '9' {
					allDigits = false
					break
				}
			}
			if allDigits {
				return id[:len(id)-5]
			}
		}
	}
	return id
}

func isVisionOnlyModel(id string) bool {
	return strings.Contains(id, "-VL-") || strings.HasSuffix(id, "-VL")
}

func parseRemoteModel(raw map[string]any, seen map[string]bool) *ModelInfo {
	rawID, _ := raw["model_id"].(string)
	if rawID == "" {
		return nil
	}
	id := normalizeModelID(rawID)
	if isVisionOnlyModel(id) {
		return nil
	}
	name, _ := raw["model_name"].(string)
	if name == "" {
		name = id
	} else {
		name = normalizeModelID(name)
	}
	if seen[id] {
		return nil
	}
	seen[id] = true
	ctx := contextLength(raw)
	if ctx == 0 {
		ctx = contextWindows[id]
	}
	vision, visionKnown := visionSupported(raw)
	if !visionKnown {
		vision = isVisionModelID(id)
	}
	multiplier, free := priceFactor(raw)
	if explicit, ok := boolField(raw, "is_free", "isFree", "free"); ok {
		free = explicit
	}
	tools, toolsKnown := boolField(raw, "tool_call", "supports_tools", "supportsTools", "function_calling")
	if !toolsKnown {
		tools = true
	}
	return &ModelInfo{
		ID:            id,
		Name:          name,
		ContextLength: ctx,
		Tools:         tools,
		Vision:        vision,
		Vendor:        firstStringField(raw, "vendor", "provider", "developer", "model_series", "modelSeries"),
		Multiplier:    multiplier,
		Thinking:      thinkingTiers(raw),
		Free:          free,
	}
}

func isVisionModelID(id string) bool {
	lower := strings.ToLower(id)
	return strings.Contains(lower, "-vl-") || strings.HasSuffix(lower, "-vl")
}

func firstStringField(raw map[string]any, keys ...string) string {
	for _, key := range keys {
		if s, ok := raw[key].(string); ok {
			if s = strings.TrimSpace(s); s != "" {
				return s
			}
		}
	}
	return ""
}
