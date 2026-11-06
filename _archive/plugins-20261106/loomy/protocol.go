// Package main — OpenAI wire handling for Loomy.
//
// Loomy serves the OpenAI chat shape, so a streaming request needs no rewriting:
// its SSE frames are already chat.completion.chunk. Two things do need work:
//
//  1. Loomy only streams. The reference's buildBody always sends "stream": true
//     (loomy-adapter.ts), and there is no non-streaming branch anywhere in it.
//     So a client that asked for a complete JSON answer is served by forcing the
//     upstream to stream and aggregating the frames here.
//  2. The host already rewrote `model` to the plugin-local id before calling us,
//     so the body's other fields are forwarded untouched.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// jsonUnmarshalObject is json.Unmarshal with an object-shape guard, so an array
// or scalar where an object is expected is reported as such instead of producing
// a zero value that silently looks like success.
func jsonUnmarshalObject(body []byte, dst any) error {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return fmt.Errorf("expected a JSON object")
	}
	return json.Unmarshal(trimmed, dst)
}

// jsonUnmarshalArray is the array-shaped sibling of jsonUnmarshalObject.
func jsonUnmarshalArray(body []byte, dst any) error {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return fmt.Errorf("expected a JSON array")
	}
	return json.Unmarshal(trimmed, dst)
}

// forceStream sets "stream": true in an OpenAI chat body, preserving every other
// field. The host hands us the already-rewritten model name, so nothing else is
// touched.
func forceStream(body []byte) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := jsonUnmarshalObject(body, &m); err != nil {
		return nil, err
	}
	m["stream"] = json.RawMessage("true")
	return json.Marshal(m)
}

// streamRequested reports whether the client asked for SSE. The host parses the
// same field before calling us.
func streamRequested(body []byte) bool {
	var probe struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	return probe.Stream
}

// aggregateSSE consumes an OpenAI SSE stream and renders one chat.completion.
//
// Chunk accumulation follows the reference's consumeOpenAiSse rules
// (lobsterai-adapter.ts documents the same three ported from the Go side):
//   - tolerate "data:" with or without a following space,
//   - tool_calls are merged by `index` (the first fragment carries id/name, the
//     rest only arguments),
//   - a non-empty function.name never gets overwritten by a later empty string,
//     which would otherwise produce `unknown tool ""`.
func aggregateSSE(up io.Reader, model string) ([]byte, error) {
	scanner := bufio.NewScanner(up)
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)

	var content strings.Builder
	reasoning := strings.Builder{}
	role := "assistant"
	finish := ""
	id := ""
	created := int64(0)
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
		// slice(5) then TrimSpace accepts both "data: {...}" and "data:{...}".
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
				// Only a non-empty name may overwrite: later fragments send "".
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
		return nil, fmt.Errorf("loomy: upstream sent no SSE data frames")
	}

	message := map[string]any{"role": role, "content": content.String()}
	if reasoning.Len() > 0 {
		// Not part of the OpenAI contract, but the host passes unknown message
		// fields through untouched; dropping it would lose the model's thinking.
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
		id = "chatcmpl-loomy"
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
