package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// ---------------------------------------------------------------- response side

// StreamEvent is one parsed upstream SSE frame.
type StreamEvent struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Created int64  `json:"created"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string       `json:"role"`
			Content          string       `json:"content"`
			ReasoningContent string       `json:"reasoning_content"`
			ToolCalls        []oaToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

type oaToolCall struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Completion accumulates a whole SSE stream into one OpenAI chat.completion.
type Completion struct {
	ID        string
	Model     string
	Created   int64
	Content   strings.Builder
	Reason    strings.Builder
	ToolCalls []accumulatedToolCall
	usage     *struct {
		PromptTokens     int
		CompletionTokens int
		TotalTokens      int
	}
	finish string
}

type accumulatedToolCall struct {
	ID        string
	Name      string
	Arguments strings.Builder
	order     int
}

// Consume folds an SSE body into a Completion, restoring aliased tool names.
func Consume(r io.Reader, aliases map[string]string) (*Completion, error) {
	reverse := map[string]string{}
	for alias, original := range aliases {
		reverse[alias] = original
	}
	c := &Completion{}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var ev StreamEvent
		if json.Unmarshal([]byte(data), &ev) != nil {
			continue
		}
		c.absorb(&ev, reverse)
	}
	if err := scanner.Err(); err != nil {
		return c, err
	}
	return c, nil
}

func (c *Completion) absorb(ev *StreamEvent, reverse map[string]string) {
	if ev.ID != "" && c.ID == "" {
		c.ID = ev.ID
	}
	if ev.Model != "" {
		c.Model = ev.Model
	}
	if ev.Created != 0 {
		c.Created = ev.Created
	}
	if ev.Usage != nil {
		c.usage = &struct {
			PromptTokens     int
			CompletionTokens int
			TotalTokens      int
		}{ev.Usage.PromptTokens, ev.Usage.CompletionTokens, ev.Usage.TotalTokens}
	}
	for _, ch := range ev.Choices {
		if ch.Delta.Content != "" {
			c.Content.WriteString(ch.Delta.Content)
		}
		if ch.Delta.ReasoningContent != "" {
			c.Reason.WriteString(ch.Delta.ReasoningContent)
		}
		for _, tc := range ch.Delta.ToolCalls {
			c.absorbToolCall(tc, reverse)
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			c.finish = *ch.FinishReason
		}
	}
}

func (c *Completion) absorbToolCall(tc oaToolCall, reverse map[string]string) {
	name := tc.Function.Name
	if alias, ok := reverse[name]; ok {
		name = alias
	}
	if tc.Index != nil && *tc.Index >= 0 {
		for i := range c.ToolCalls {
			if c.ToolCalls[i].order == *tc.Index {
				c.mergeToolCall(i, tc, name)
				return
			}
		}
		c.ToolCalls = append(c.ToolCalls, accumulatedToolCall{order: *tc.Index})
		c.mergeToolCall(len(c.ToolCalls)-1, tc, name)
		return
	}
	// No index: append a new call only when this frame starts one, otherwise
	// append the argument fragment to the most recent call.
	if tc.ID != "" || name != "" || len(c.ToolCalls) == 0 {
		c.ToolCalls = append(c.ToolCalls, accumulatedToolCall{order: len(c.ToolCalls)})
		c.mergeToolCall(len(c.ToolCalls)-1, tc, name)
		return
	}
	c.mergeToolCall(len(c.ToolCalls)-1, tc, name)
}

// mergeToolCall folds one streamed fragment into an existing accumulator.
func (c *Completion) mergeToolCall(i int, tc oaToolCall, name string) {
	if tc.ID != "" && c.ToolCalls[i].ID == "" {
		c.ToolCalls[i].ID = tc.ID
	}
	if name != "" && c.ToolCalls[i].Name == "" {
		c.ToolCalls[i].Name = name
	}
	if tc.Function.Arguments != "" {
		c.ToolCalls[i].Arguments.WriteString(tc.Function.Arguments)
	}
}

// JSON renders the accumulated stream as one chat.completion body.
func (c *Completion) JSON(model string) ([]byte, error) {
	content := c.Content.String()
	msg := map[string]any{"role": "assistant", "content": content}
	if reason := c.Reason.String(); reason != "" {
		msg["reasoning_content"] = reason
	}
	if len(c.ToolCalls) > 0 {
		sort.SliceStable(c.ToolCalls, func(i, j int) bool { return c.ToolCalls[i].order < c.ToolCalls[j].order })
		calls := make([]any, 0, len(c.ToolCalls))
		for _, tc := range c.ToolCalls {
			args := tc.Arguments.String()
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			calls = append(calls, map[string]any{
				"id":       firstNonEmpty(tc.ID, "call_"+fmt.Sprint(tc.order)),
				"type":     "function",
				"function": map[string]any{"name": tc.Name, "arguments": args},
			})
		}
		msg["tool_calls"] = calls
	}
	finish := c.finish
	if finish == "" {
		if len(c.ToolCalls) > 0 {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}
	if len(c.ToolCalls) > 0 && finish == "stop" {
		finish = "tool_calls"
	}
	out := map[string]any{
		"id":      firstNonEmpty(c.ID, "chatcmpl-opencode"),
		"object":  "chat.completion",
		"created": c.Created,
		"model":   firstNonEmpty(model, c.Model),
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": finish,
		}},
	}
	if c.usage != nil {
		out["usage"] = map[string]any{
			"prompt_tokens":     c.usage.PromptTokens,
			"completion_tokens": c.usage.CompletionTokens,
			"total_tokens":      c.usage.TotalTokens,
		}
	}
	return json.Marshal(out)
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
