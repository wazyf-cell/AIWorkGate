package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// effortRank orders the reasoning-effort names the upstream understands. It is
// the reference's REASONING_EFFORT_RANK verbatim; TranslateEffort uses it to pick
// the nearest declared level instead of sending a value the model rejects (400).
var effortRank = map[string]int{
	"none": 0, "off": 0, "minimal": 1, "low": 3, "medium": 4, "high": 5,
	"on": 5, "extra_high": 6, "xhigh": 7, "max": 8, "ultra": 9,
}

// TranslateEffort maps a requested reasoning effort onto the levels a model
// actually declares. exact reports an unchanged passthrough; "" means the
// parameter must be dropped (the model declares no levels at all).
func TranslateEffort(requested string, declared []string) (effort string, exact bool) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", false
	}
	for _, d := range declared {
		if d == requested {
			return requested, true
		}
	}
	rank, ok := effortRank[requested]
	if !ok || len(declared) == 0 {
		return "", false
	}
	bestRank, best := -1, ""
	for _, candidate := range declared {
		cr, ok := effortRank[candidate]
		if !ok || (cr == 0) != (rank == 0) {
			continue
		}
		dist := cr - rank
		if dist < 0 {
			dist = -dist
		}
		if best == "" {
			bestRank, best = cr, candidate
			continue
		}
		curDist := bestRank - rank
		if curDist < 0 {
			curDist = -curDist
		}
		if dist < curDist || (dist == curDist && cr > bestRank) {
			bestRank, best = cr, candidate
		}
	}
	return best, false
}

// oaTool is one OpenAI tool declaration as it arrives from the host.
type oaTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// BuildPayload rewrites an OpenAI chat/completions body into the shape the free
// tier accepts. It returns the body to POST and the tool-name aliases it
// introduced (so the response side can put the caller's names back).
//
// The rewrites, each of which the reference calls out as load-bearing:
//   - stream is forced true (the gate).
//   - the caller's tools are kept, but a caller tool that happens to be named
//     read/shell is renamed to read_client/shell_client, because only the exact
//     official definition may occupy those names.
//   - any gate tool that is missing gets injected from the official definition,
//     plus a system note telling the model the placeholders do nothing.
//   - reasoning_effort is translated onto the model's declared levels.
//   - a tool_choice that requests no tool ("none" or {"type":"none"}) is
//     deleted: the upstream answers 403 to it.
//   - the max_tokens value is only forwarded when it is a positive integer: 0 or
//     a negative makes the gateway reject the whole turn.
func BuildPayload(raw []byte, capabilities []Capability) (out []byte, aliases map[string]string, err error) {
	var in struct {
		Model           string          `json:"model"`
		Messages        json.RawMessage `json:"messages"`
		System          json.RawMessage `json:"system"`
		Tools           []oaTool        `json:"tools"`
		Temperature     *float64        `json:"temperature"`
		MaxTokens       *int            `json:"max_tokens"`
		MaxCompletion   *int            `json:"max_completion_tokens"`
		ReasoningEffort string          `json:"reasoning_effort"`
		ToolChoice      json.RawMessage `json:"tool_choice"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, nil, err
	}
	aliases = map[string]string{}

	tools := buildTools(in.Tools, aliases)
	shaped := ensureGateShape(tools, aliases)

	payload := map[string]any{
		"model":    in.Model,
		"messages": json.RawMessage(in.Messages),
		// The gate: the free tier only serves a streaming call.
		"stream": true,
	}
	if b := bytes.TrimSpace(in.System); len(b) > 0 && string(b) != "null" {
		payload["system"] = json.RawMessage(in.System)
	}
	if len(in.Tools) > 0 || len(shaped.injected) > 0 {
		payload["tools"] = shaped.tools
	}
	if len(in.ToolChoice) > 0 && string(bytes.TrimSpace(in.ToolChoice)) != "null" && !toolChoiceRequestsNone(in.ToolChoice) {
		payload["tool_choice"] = aliasToolChoice(in.ToolChoice, aliases)
	}
	if in.Temperature != nil {
		payload["temperature"] = *in.Temperature
	}
	maxTok := 0
	if in.MaxTokens != nil && *in.MaxTokens > 0 {
		maxTok = *in.MaxTokens
	} else if in.MaxCompletion != nil && *in.MaxCompletion > 0 {
		maxTok = *in.MaxCompletion
	}
	if maxTok > 0 {
		payload["max_tokens"] = maxTok
	}
	if effort, ok := TranslateEffort(in.ReasoningEffort, declaredEfforts(capabilities, in.Model)); ok || effort != "" {
		payload["reasoning_effort"] = effort
	}
	if len(shaped.injected) > 0 {
		addGateNote(payload, shaped.injected)
	}
	aliasHistoryToolCalls(payload, aliases)

	body, err := json.Marshal(payload)
	return body, aliases, err
}

// buildTools converts the host's tool list into the upstream's array form,
// registering an alias for every read/shell name it has to move aside.
func buildTools(in []oaTool, aliases map[string]string) []any {
	taken := map[string]bool{}
	for _, t := range in {
		taken[t.Function.Name] = true
	}
	out := make([]any, 0, len(in))
	for _, t := range in {
		name := t.Function.Name
		if isGateTool(name) && !isOfficialGateTool(t, name) {
			alias := name + "_client"
			for i := 2; taken[alias]; i++ {
				alias = fmt.Sprintf("%s_client%d", name, i)
			}
			taken[alias] = true
			aliases[alias] = name
			name = alias
		}
		fn := map[string]any{"name": name}
		if t.Function.Description != "" {
			fn["description"] = t.Function.Description
		}
		if p := bytes.TrimSpace(t.Function.Parameters); len(p) > 0 && string(p) != "null" {
			fn["parameters"] = json.RawMessage(p)
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

// addGateNote tells the model the injected tools are placeholders, so it does not
// try to call them. Mirrors the reference's noteInjectedGateTools.
func addGateNote(payload map[string]any, injected []string) {
	quoted := make([]string, 0, len(injected))
	for _, n := range injected {
		quoted = append(quoted, "`"+n+"`")
	}
	note := fmt.Sprintf("Do not call the %s tools; %s placeholders that do nothing. Use the other tools you have been given instead.",
		strings.Join(quoted, " or "), plural(len(injected)))
	switch sys := payload["system"].(type) {
	case json.RawMessage:
		var s string
		if json.Unmarshal(sys, &s) == nil && s != "" {
			enc, _ := json.Marshal(s + "\n\n" + note)
			payload["system"] = json.RawMessage(enc)
			return
		}
		enc, _ := json.Marshal(note)
		payload["system"] = json.RawMessage(enc)
	case string:
		if sys != "" {
			payload["system"] = sys + "\n\n" + note
			return
		}
		payload["system"] = note
	default:
		// No system field at all: the note still has to reach the model, so it
		// becomes one. A top-level system string is accepted by the upstream
		// (measured 2026-10-21).
		payload["system"] = note
	}
}

func plural(n int) string {
	if n > 1 {
		return "they are"
	}
	return "it is"
}

// FoldSystemIntoMessages rewrites a payload that carries a top-level "system"
// string into the same request with that text prepended as a system-role
// message. Only needed for upstream routes that reject the top-level field with
// `json: unknown field "system"` (measured on glm-5.3-flash-free, 2026-10-22).
func FoldSystemIntoMessages(body []byte) ([]byte, bool) {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return nil, false
	}
	raw, ok := payload["system"]
	if !ok {
		return nil, false
	}
	var note string
	if json.Unmarshal(raw, &note) != nil || note == "" {
		return nil, false
	}
	var messages []any
	if prev, ok := payload["messages"]; ok {
		if json.Unmarshal(prev, &messages) != nil {
			return nil, false
		}
	}
	messages = append([]any{map[string]any{"role": "system", "content": note}}, messages...)
	enc, err := json.Marshal(messages)
	if err != nil {
		return nil, false
	}
	payload["messages"] = enc
	delete(payload, "system")
	out, err := json.Marshal(payload)
	if err != nil {
		return nil, false
	}
	return out, true
}

// declaredEfforts returns the reasoning levels models.dev declares for model.
func declaredEfforts(capabilities []Capability, model string) []string {
	for _, c := range capabilities {
		if c.ID == model {
			return c.Efforts
		}
	}
	return nil
}
