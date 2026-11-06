package main

// Folding a chat SSE stream into one chat.completion, for the non-streaming path.
// Cline is always asked to stream (BuildBody forces stream:true), so a plain
// request still arrives here as OpenAI SSE frames.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// completion accumulates a whole OpenAI SSE stream into one chat.completion.
type completion struct {
	ID      string
	Model   string
	Created int64
	Content strings.Builder
	Reason  strings.Builder
	Calls   []toolCall
	Usage   *usage
	Finish  string
}

type toolCall struct {
	Index     int
	ID        string
	Name      string
	Arguments strings.Builder
}

type usage struct {
	Prompt     int `json:"prompt_tokens"`
	Completion int `json:"completion_tokens"`
	Total      int `json:"total_tokens"`
}

// consumeStream folds an SSE body into a completion.
func consumeStream(r io.Reader) (*completion, error) {
	c := &completion{}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == sseDoneFrame {
			continue
		}
		var frame struct {
			ID      string `json:"id"`
			Model   string `json:"model"`
			Created int64  `json:"created"`
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    *int   `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *usage `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &frame) != nil {
			continue
		}
		if c.ID == "" {
			c.ID = frame.ID
		}
		if frame.Model != "" {
			c.Model = frame.Model
		}
		if frame.Created != 0 {
			c.Created = frame.Created
		}
		if frame.Usage != nil {
			c.Usage = frame.Usage
		}
		for _, ch := range frame.Choices {
			c.Content.WriteString(ch.Delta.Content)
			c.Reason.WriteString(ch.Delta.ReasoningContent)
			for _, tc := range ch.Delta.ToolCalls {
				c.mergeCall(tc.Index, tc.ID, tc.Function.Name, tc.Function.Arguments)
			}
			if ch.FinishReason != nil && *ch.FinishReason != "" {
				c.Finish = *ch.FinishReason
			}
		}
	}
	return c, scanner.Err()
}

// mergeCall folds one streamed tool-call fragment into the accumulator.
func (c *completion) mergeCall(index *int, id, name, args string) {
	target := -1
	if index != nil && *index >= 0 {
		for i := range c.Calls {
			if c.Calls[i].Index == *index {
				target = i
				break
			}
		}
		if target < 0 {
			c.Calls = append(c.Calls, toolCall{Index: *index})
			target = len(c.Calls) - 1
		}
	} else if id != "" || name != "" || len(c.Calls) == 0 {
		c.Calls = append(c.Calls, toolCall{Index: len(c.Calls)})
		target = len(c.Calls) - 1
	} else {
		target = len(c.Calls) - 1
	}
	if id != "" && c.Calls[target].ID == "" {
		c.Calls[target].ID = id
	}
	if name != "" && c.Calls[target].Name == "" {
		c.Calls[target].Name = name
	}
	if args != "" {
		c.Calls[target].Arguments.WriteString(args)
	}
}

// json renders the accumulated stream as one chat.completion body.
func (c *completion) json(model string) ([]byte, error) {
	msg := map[string]any{"role": "assistant", "content": c.Content.String()}
	if reason := c.Reason.String(); reason != "" {
		msg["reasoning_content"] = reason
	}
	if len(c.Calls) > 0 {
		calls := make([]any, 0, len(c.Calls))
		for i, tc := range c.Calls {
			args := tc.Arguments.String()
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			id := tc.ID
			if id == "" {
				id = fmt.Sprintf("call_%d", i)
			}
			calls = append(calls, map[string]any{
				"id":   id,
				"type": "function",
				"function": map[string]any{
					"name": tc.Name, "arguments": args,
				},
			})
		}
		msg["tool_calls"] = calls
	}
	finish := c.Finish
	if finish == "" {
		if len(c.Calls) > 0 {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}
	if len(c.Calls) > 0 && finish == "stop" {
		finish = "tool_calls"
	}
	out := map[string]any{
		"id":      firstNonEmpty(c.ID, "chatcmpl-cline"),
		"object":  "chat.completion",
		"created": c.Created,
		"model":   firstNonEmpty(model, c.Model),
		"choices": []any{map[string]any{
			"index": 0, "message": msg, "finish_reason": finish,
		}},
	}
	if c.Usage != nil {
		out["usage"] = c.Usage
	}
	return json.Marshal(out)
}
