// Package main — OpenAI wire handling for Raccoon.
//
// Raccoon serves the OpenAI chat shape and only streams, so the same strategy as
// the loomy plugin applies: force "stream": true upstream (the reference's
// buildBody always sets it), relay the frames verbatim for a streaming client,
// and aggregate them for a non-streaming one.
//
// Two Raccoon-specific rules from the reference are honoured here:
//   - the thinking switch goes inside `extra_body`, never at the top level
//     ("⚠️ 必须在 extra_body 内 —— 实测放顶层会被忽略"), and
//   - the gateway meters request-body BYTES at 10MB.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

func jsonUnmarshalObject(body []byte, dst any) error {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return fmt.Errorf("expected a JSON object")
	}
	return json.Unmarshal(trimmed, dst)
}

func jsonUnmarshalArray(body []byte, dst any) error {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return fmt.Errorf("expected a JSON array")
	}
	return json.Unmarshal(trimmed, dst)
}

// thinkingExtraBody ports raccoonThinkingExtraBody verbatim:
//
//	effort ""      -> no extra_body at all (upstream default = on)
//	effort "off"   -> {"thinking": {"type": "disabled"}}
//	anything else  -> {"thinking": {"type": "enabled"}}
//
// The reference only ever produces these two inner values (`effort === OFF ?
// "disabled" : "enabled"`), so the old behaviour of copying the raw
// reasoning_effort value into extra_body was wrong: the upstream reads
// extra_body.thinking.type, not extra_body.reasoning_effort.
func thinkingExtraBody(effort string) map[string]json.RawMessage {
	if strings.TrimSpace(effort) == "" {
		return nil
	}
	kind := "enabled"
	if effort == "off" {
		kind = "disabled"
	}
	inner, _ := json.Marshal(map[string]string{"type": kind})
	return map[string]json.RawMessage{"thinking": inner}
}

// moveReasoningIntoExtraBody rewrites a client's top-level reasoning_effort into
// the reference's nested extra_body.thinking.type and never leaves the top-level
// field behind (the upstream ignores it there).
func moveReasoningIntoExtraBody(m map[string]json.RawMessage) {
	raw, ok := m["reasoning_effort"]
	if !ok {
		return
	}
	delete(m, "reasoning_effort")
	var effort string
	_ = json.Unmarshal(raw, &effort)
	thinking := thinkingExtraBody(effort)
	if thinking == nil {
		return
	}
	var existing map[string]json.RawMessage
	if prev, ok := m["extra_body"]; ok {
		_ = json.Unmarshal(prev, &existing)
	}
	if existing == nil {
		existing = map[string]json.RawMessage{}
	}
	for k, v := range thinking {
		existing[k] = v
	}
	wrapped, err := json.Marshal(existing)
	if err != nil {
		return
	}
	m["extra_body"] = wrapped
}

// forceStream sets stream:true, applies the extra_body rule, and preserves every
// other field untouched.
func forceStream(body []byte) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := jsonUnmarshalObject(body, &m); err != nil {
		return nil, err
	}
	m["stream"] = json.RawMessage("true")
	moveReasoningIntoExtraBody(m)
	return json.Marshal(m)
}

func streamRequested(body []byte) bool {
	var probe struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	return probe.Stream
}

// forceUsage makes an aggregation request ask the upstream for token usage.
//
// Raccoon only streams, so a non-streaming client's single chat.completion is
// assembled from SSE frames. OpenAI-shaped upstreams put the final usage chunk
// on that stream only when the request carries
// stream_options.include_usage, so the aggregate would always lack usage even
// though aggregateSSE is ready to keep it. A streaming client's own
// stream_options are left untouched: there the usage chunk is the upstream's
// answer to the client's request, and OpenAI emits it only when asked.
func forceUsage(body []byte) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := jsonUnmarshalObject(body, &m); err != nil {
		return nil, err
	}
	opts := map[string]json.RawMessage{}
	if prev, ok := m["stream_options"]; ok {
		if err := json.Unmarshal(prev, &opts); err != nil || opts == nil {
			opts = map[string]json.RawMessage{}
		}
	}
	opts["include_usage"] = json.RawMessage("true")
	wrapped, err := json.Marshal(opts)
	if err != nil {
		return nil, err
	}
	m["stream_options"] = wrapped
	return json.Marshal(m)
}

// aggregateSSE consumes an OpenAI SSE stream and renders one chat.completion.
// Tool fragments merge by index; a non-empty function.name is never overwritten
// by a later empty string.
func aggregateSSE(up io.Reader, model string) ([]byte, error) {
	scanner := bufio.NewScanner(up)
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)

	var content strings.Builder
	reasoning := strings.Builder{}
	role := "assistant"
	finish := ""
	id := ""
	var created int64
	usage := map[string]any{}

	type toolAcc struct {
		ID   string
		Type string
		Name string
		Args strings.Builder
	}
	tools := map[int]*toolAcc{}
	var toolOrder []int
	gotAny := false

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(line[len("data:"):])
		if data == "" || data == "[DONE]" {
			continue
		}
		var frame struct {
			ID      string         `json:"id"`
			Created int64          `json:"created"`
			Usage   map[string]any `json:"usage"`
			Choices []struct {
				Delta struct {
					Role             string `json:"role"`
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason any `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &frame); err != nil {
			continue
		}
		gotAny = true
		if frame.ID != "" {
			id = frame.ID
		}
		if frame.Created != 0 {
			created = frame.Created
		}
		if len(frame.Usage) > 0 {
			usage = frame.Usage
		}
		for _, ch := range frame.Choices {
			if ch.Delta.Role != "" {
				role = ch.Delta.Role
			}
			content.WriteString(ch.Delta.Content)
			reasoning.WriteString(ch.Delta.ReasoningContent)
			for _, tc := range ch.Delta.ToolCalls {
				acc := tools[tc.Index]
				if acc == nil {
					acc = &toolAcc{}
					tools[tc.Index] = acc
					toolOrder = append(toolOrder, tc.Index)
				}
				if tc.ID != "" {
					acc.ID = tc.ID
				}
				if tc.Type != "" {
					acc.Type = tc.Type
				}
				if tc.Function.Name != "" {
					acc.Name = tc.Function.Name
				}
				acc.Args.WriteString(tc.Function.Arguments)
			}
			if fr, ok := ch.FinishReason.(string); ok && fr != "" {
				finish = fr
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if !gotAny {
		return nil, fmt.Errorf("raccoon: upstream sent no SSE data frames")
	}

	message := map[string]any{"role": role, "content": content.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolOrder) > 0 {
		calls := make([]map[string]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			acc := tools[idx]
			t := acc.Type
			if t == "" {
				t = "function"
			}
			args := acc.Args.String()
			if args == "" {
				args = "{}"
			}
			calls = append(calls, map[string]any{
				"id":   acc.ID,
				"type": t,
				"function": map[string]any{
					"name":      acc.Name,
					"arguments": args,
				},
			})
		}
		message["tool_calls"] = calls
		if finish == "" {
			finish = "tool_calls"
		}
	}
	if finish == "" {
		finish = "stop"
	}
	if id == "" {
		id = "chatcmpl-raccoon"
	}
	out := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
	}
	if len(usage) > 0 {
		out["usage"] = usage
	}
	return json.Marshal(out)
}
