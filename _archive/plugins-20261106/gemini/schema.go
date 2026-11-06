package main

// Tool-schema sanitisation for the Gemini upstream.
//
// The upstream rejects any JSON Schema key outside its allow-list with a 400, so
// an OpenAI tool declaration has to be filtered down before it is sent. Ported from
// sanitizeGeminiSchema in the reference implementation.

import (
	"encoding/json"
	"sort"
)

// geminiSchemaKeys is the allow-list the reference enforces. A key outside it
// makes the upstream answer 400, so unknown keys are dropped rather than
// forwarded.
var geminiSchemaKeys = map[string]struct{}{
	"type": {}, "format": {}, "description": {}, "nullable": {}, "enum": {},
	"items": {}, "minItems": {}, "maxItems": {}, "properties": {}, "required": {},
	"minProperties": {}, "maxProperties": {}, "minLength": {}, "maxLength": {},
	"pattern": {}, "anyOf": {}, "propertyOrdering": {}, "minimum": {}, "maximum": {},
}

// sanitizeParameters recursively filters a JSON Schema down to the allow-list.
//
// The special cases the reference pins down:
//   - type may be an array; the first non-null entry wins and nullable is set
//     when "null" was present;
//   - items and anyOf recurse;
//   - enum is only kept when every value is a string.
func sanitizeParameters(raw json.RawMessage) json.RawMessage {
	var schema map[string]any
	if json.Unmarshal(raw, &schema) != nil {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	out := sanitizeSchemaNode(schema)
	b, err := json.Marshal(out)
	if err != nil {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return b
}

func sanitizeSchemaNode(schema map[string]any) map[string]any {
	out := map[string]any{}
	// Sorted iteration keeps the output deterministic; object order is not
	// semantic but a stable byte order makes the port testable.
	keys := make([]string, 0, len(schema))
	for k := range schema {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, allowed := geminiSchemaKeys[key]; !allowed {
			continue
		}
		value := schema[key]
		switch key {
		case "properties":
			props, ok := value.(map[string]any)
			if !ok {
				continue
			}
			clean := map[string]any{}
			for name, child := range props {
				childMap, ok := child.(map[string]any)
				if !ok {
					continue
				}
				clean[name] = sanitizeSchemaNode(childMap)
			}
			out["properties"] = clean
		case "items":
			switch v := value.(type) {
			case map[string]any:
				out["items"] = sanitizeSchemaNode(v)
			case []any:
				var arr []any
				for _, item := range v {
					if m, ok := item.(map[string]any); ok {
						arr = append(arr, sanitizeSchemaNode(m))
					}
				}
				out["items"] = arr
			}
		case "anyOf":
			arr, ok := value.([]any)
			if !ok {
				continue
			}
			var clean []any
			for _, item := range arr {
				if m, ok := item.(map[string]any); ok {
					clean = append(clean, sanitizeSchemaNode(m))
				}
			}
			out["anyOf"] = clean
		case "type":
			switch v := value.(type) {
			case string:
				out["type"] = v
			case []any:
				var types []string
				for _, item := range v {
					if s, ok := item.(string); ok {
						types = append(types, s)
					}
				}
				var nonNull []string
				for _, s := range types {
					if s != "null" {
						nonNull = append(nonNull, s)
					}
				}
				if len(nonNull) > 0 {
					out["type"] = nonNull[0]
				}
				if len(nonNull) != len(types) {
					out["nullable"] = true
				}
			}
		case "enum":
			arr, ok := value.([]any)
			if !ok {
				continue
			}
			allStrings := true
			for _, item := range arr {
				if _, isString := item.(string); !isString {
					allStrings = false
					break
				}
			}
			if allStrings {
				out["enum"] = arr
			}
		default:
			out[key] = value
		}
	}
	return out
}
