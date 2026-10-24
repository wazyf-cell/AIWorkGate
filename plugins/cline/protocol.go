// Package main implements the protocol layer for Cline's gateway
// (https://api.cline.bot), ported from the reference implementation's
// vendor/channel-pack/src/cline.ts, cline-product.ts and cline-adapter.ts.
//
// Cline speaks plain OpenAI chat/completions, so there is no protocol
// translation here. What there IS, and what this file holds, is the set of
// Cline-specific envelopes:
//
//   - the Authorization value must be `workos:<accessToken>`: the token prefix is
//     part of the wire format, not decoration (clineBearerValue).
//   - every request carries the four client headers.
//   - tool parameter schemas must be sanitized: an empty-string member inside an
//     `enum` is rejected by some routed upstreams with a 400.
//   - max_tokens is clamped to the channel's hard output ceiling.
//   - reasoning effort is a whitelist of five names.
package main

import (
	"encoding/json"
	"strings"
)

// Client headers, ported verbatim from def-cline.js / cline-product.ts.
// These identify the client to the gateway; the reference sends them on every
// call, including the auth ones.
var clineClientHeaders = [][2]string{
	{"HTTP-Referer", "https://cline.bot"},
	{"X-Title", "Cline"},
	{"X-IS-MULTIROOT", "false"},
	{"X-CLIENT-TYPE", "cline-sdk"},
}

// tokenPrefix must be prepended to the access token. The reference is explicit:
// "the Authorization header must carry the workos: prefix".
const tokenPrefix = "workos:"

// bearerValue renders the Authorization value for one access token.
//
// An already-prefixed token is returned unchanged, so storing either form works
// and a re-login cannot produce `workos:workos:...`.
func bearerValue(accessToken string) string {
	token := strings.TrimSpace(accessToken)
	if token == "" {
		return ""
	}
	if strings.HasPrefix(token, tokenPrefix) {
		return token
	}
	return tokenPrefix + token
}

// bareToken strips the prefix, for storage and for the WorkOS refresh body.
func bareToken(accessToken string) string {
	return strings.TrimPrefix(strings.TrimSpace(accessToken), tokenPrefix)
}

// clineReasoningEfforts is the effort whitelist the channel declares, in the
// reference's order (CLINE_REASONING_EFFORTS). Note "max" is displayed as
// "Extra" in the reference UI but the wire value is "max".
var clineReasoningEfforts = []string{"none", "low", "medium", "high", "max"}

// defaultReasoningEffort mirrors CLINE_DEFAULT_REASONING_EFFORT.
const defaultReasoningEffort = "high"

// maxOutputTokens is the channel's hard ceiling (CLINE_MAX_OUTPUT_TOKENS).
const maxOutputTokens = 943718

// IsValidEffort reports whether effort is one the channel accepts.
func IsValidEffort(effort string) bool {
	for _, e := range clineReasoningEfforts {
		if e == effort {
			return true
		}
	}
	return false
}

// SanitizeToolParameters drops empty-string enum members recursively.
//
// The reference calls this out as a real defect source: the harness's tool
// schemas can carry an empty `enum` entry, and a routed Gemini-family upstream
// answers 400 for it. A member list that becomes empty is dropped entirely
// rather than sent as `"enum": []`.
func SanitizeToolParameters(value any) any {
	switch v := value.(type) {
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = SanitizeToolParameters(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, raw := range v {
			if key == "enum" {
				list, ok := raw.([]any)
				if !ok {
					out[key] = SanitizeToolParameters(raw)
					continue
				}
				cleaned := make([]any, 0, len(list))
				for _, item := range list {
					if s, ok := item.(string); ok && strings.TrimSpace(s) == "" {
						continue
					}
					cleaned = append(cleaned, SanitizeToolParameters(item))
				}
				if len(cleaned) > 0 {
					out[key] = cleaned
				}
				continue
			}
			out[key] = SanitizeToolParameters(raw)
		}
		return out
	}
	return value
}

// ClampMaxTokens bounds max_tokens to the channel ceiling. A value that is not a
// positive integer is dropped (returns 0), because 0 or a negative makes the
// gateway reject the whole turn.
func ClampMaxTokens(requested int) int {
	if requested <= 0 {
		return 0
	}
	if requested > maxOutputTokens {
		return maxOutputTokens
	}
	return requested
}

// BuildBody rewrites an OpenAI chat/completions body into the shape Cline
// accepts. Unlike opencode this is a light touch: Cline already speaks OpenAI,
// so only the sanitizing and clamping above apply, plus forcing stream:true so a
// caller asking for a non-streaming answer still gets a stream we can fold (the
// reference always sets stream:true).
func BuildBody(raw []byte, defaultEffort bool) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, err
	}
	top["stream"] = json.RawMessage(`true`)

	// max_tokens / max_completion_tokens.
	var maxTok int
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		if rawVal, ok := top[key]; ok {
			var n int
			if json.Unmarshal(rawVal, &n) == nil {
				maxTok = n
				delete(top, key)
			}
		}
	}
	if clamped := ClampMaxTokens(maxTok); clamped > 0 {
		encoded, err := json.Marshal(clamped)
		if err != nil {
			return nil, err
		}
		top["max_tokens"] = encoded
	}

	// reasoning_effort: only a declared value may go out. An undeclared value is
	// replaced by the channel's default when one is wanted, and dropped otherwise:
	// sending it would be a 400.
	if rawEffort, ok := top["reasoning_effort"]; ok {
		var effort string
		valid := json.Unmarshal(rawEffort, &effort) == nil && IsValidEffort(effort)
		if !valid {
			if defaultEffort {
				encoded, err := json.Marshal(defaultReasoningEffort)
				if err != nil {
					return nil, err
				}
				top["reasoning_effort"] = encoded
			} else {
				delete(top, "reasoning_effort")
			}
		}
	} else if defaultEffort {
		encoded, err := json.Marshal(defaultReasoningEffort)
		if err != nil {
			return nil, err
		}
		top["reasoning_effort"] = encoded
	}

	// tools[].function.parameters needs sanitizing.
	if rawTools, ok := top["tools"]; ok {
		var tools []map[string]any
		if json.Unmarshal(rawTools, &tools) == nil {
			for _, tool := range tools {
				fn, ok := tool["function"].(map[string]any)
				if !ok {
					continue
				}
				if params, ok := fn["parameters"]; ok {
					fn["parameters"] = SanitizeToolParameters(params)
				}
			}
			encoded, err := json.Marshal(tools)
			if err != nil {
				return nil, err
			}
			top["tools"] = encoded
		}
	}
	return json.Marshal(top)
}
