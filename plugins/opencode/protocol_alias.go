package main

import (
	"bytes"
	"encoding/json"
	"strings"
)

// toolChoiceRequestsNone reports whether tool_choice asks the model not to call
// any tool. The free tier answers 403 to either spelling (`"none"` or
// `{"type":"none"}`), so BuildPayload deletes the key instead of forwarding it.
// Ported from the reference's buildOpencodePayload, which deletes the field
// after shaping and never writes it back.
func toolChoiceRequestsNone(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return false
	}
	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal(trimmed, &s) != nil {
			return false
		}
		return strings.EqualFold(strings.TrimSpace(s), "none")
	}
	var obj map[string]any
	if json.Unmarshal(trimmed, &obj) != nil {
		return false
	}
	kind, _ := obj["type"].(string)
	return strings.EqualFold(strings.TrimSpace(kind), "none")
}

// aliasToolChoice rewrites a tool_choice that names an aliased tool.
func aliasToolChoice(raw json.RawMessage, aliases map[string]string) json.RawMessage {
	if len(aliases) == 0 {
		return raw
	}
	reverse := map[string]string{}
	for alias, original := range aliases {
		reverse[original] = alias
	}
	var choice map[string]any
	if json.Unmarshal(raw, &choice) != nil {
		return raw
	}
	if name, ok := choice["name"].(string); ok {
		if alias, ok := reverse[name]; ok {
			choice["name"] = alias
		}
	}
	if fn, ok := choice["function"].(map[string]any); ok {
		if name, ok := fn["name"].(string); ok {
			if alias, ok := reverse[name]; ok {
				fn["name"] = alias
			}
		}
	}
	out, err := json.Marshal(choice)
	if err != nil {
		return raw
	}
	return out
}

// aliasHistoryToolCalls rewrites assistant tool_calls in the history so a tool the
// model previously called by its original name now matches the aliased
// declaration (otherwise the upstream sees a call to an undeclared tool).
func aliasHistoryToolCalls(payload map[string]any, aliases map[string]string) {
	if len(aliases) == 0 {
		return
	}
	reverse := map[string]string{}
	for alias, original := range aliases {
		reverse[original] = alias
	}
	raw, ok := payload["messages"].(json.RawMessage)
	if !ok {
		return
	}
	var msgs []map[string]any
	if json.Unmarshal(raw, &msgs) != nil {
		return
	}
	changed := false
	for _, m := range msgs {
		calls, ok := m["tool_calls"].([]any)
		if !ok {
			continue
		}
		for _, c := range calls {
			entry, _ := c.(map[string]any)
			fn, _ := entry["function"].(map[string]any)
			name, _ := fn["name"].(string)
			if alias, ok := reverse[name]; ok {
				fn["name"] = alias
				changed = true
			}
		}
	}
	if !changed {
		return
	}
	if next, err := json.Marshal(msgs); err == nil {
		payload["messages"] = json.RawMessage(next)
	}
}
