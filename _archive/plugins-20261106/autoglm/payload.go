package main

// Relay/converter payload helpers: usage extraction and error-message shaping.
//
// Ported from chat.go (orInput / payloadMsg / hasUsage / extractUsage). Split out
// of aggregate.go for the 300-line file budget; no behaviour changed.

import (
	"strings"
)

// orInput is the reference's payload.get("input", payload.get("arguments")).
func orInput(m map[string]any) any {
	if v, ok := m["input"]; ok {
		return v
	}
	return m["arguments"]
}

// payloadMsg pulls a readable message out of an error payload.
func payloadMsg(m map[string]any) string {
	if len(m) == 0 {
		return "agent request failed (upstream error event, no detail)"
	}
	for _, k := range []string{"message", "error", "msg", "detail", "text", "delta"} {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	typ := toString(m["type"])
	if typ == "" {
		typ = "error"
	}
	parts := []string{"agent request failed", typ}
	for _, k := range []string{"code", "phase", "runId"} {
		if v := toString(m[k]); v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	return strings.Join(parts, " ")
}

// hasUsage reports whether a map carries any token counter.
func hasUsage(m map[string]any) bool {
	for _, k := range []string{"input", "output", "total", "inputTokens", "outputTokens", "totalTokens",
		"prompt_tokens", "completion_tokens", "total_tokens"} {
		if _, ok := m[k]; ok {
			return true
		}
	}
	return false
}

// extractUsage digs a usage map out of an event payload.
func extractUsage(m any) map[string]any {
	mm, ok := m.(map[string]any)
	if !ok {
		return nil
	}
	if u, ok := mm["usage"].(map[string]any); ok && hasUsage(u) {
		return u
	}
	if hasUsage(mm) {
		return mm
	}
	if r, ok := mm["result"].(map[string]any); ok {
		if hasUsage(r) {
			return r
		}
		if u, ok := r["usage"].(map[string]any); ok {
			return u
		}
	}
	return nil
}
