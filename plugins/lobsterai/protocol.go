// Package main — OpenAI wire handling for LobsterAI.
//
// LobsterAI serves the OpenAI chat shape, but the reference documents one hard
// constraint: "**恒为 true**：上游只支持 SSE，stream:false 会返回 500". So the
// upstream body always forces stream:true, and a client that asked for a complete
// answer gets the frames aggregated here.
//
// The reference also notes the upstream sometimes omits the space after "data:"
// ("龙虾上游实测无空格"), which the parser tolerates.
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

// forceStream sets stream:true, preserving every other field.
//
// "stream" is unconditionally true because the reference states stream:false
// makes the upstream return 500; there is no case where forwarding the client's
// false value could work.
func forceStream(body []byte) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := jsonUnmarshalObject(body, &m); err != nil {
		return nil, err
	}
	m["stream"] = json.RawMessage("true")
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

// aggregateSSE consumes an OpenAI SSE stream and renders one chat.completion.
//
// The accumulation rules are the reference's (lobsterai-adapter.ts consumeSse,
// which itself ports three behaviours from the Go side):
//   - tolerate "data:" without a space,
//   - reasoning_content becomes its own block,
//   - tool_calls merge by index, and a non-empty function.name is never
//     overwritten by a later empty fragment (that produces `unknown tool ""`).
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
		return nil, fmt.Errorf("lobsterai: upstream sent no SSE data frames")
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
		id = "chatcmpl-lobsterai"
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
