package main

// Relay converter output side: text/tool/usage chunk emission.
//
// Split out of relay_converter.go purely for the 300-line file budget; the
// emitting rules are the reference's emitText / emitThinking / emitToolUse /
// emitUsageDone / emitError / emitParts / chunk / finish.

import (
	"encoding/json"
	"fmt"
	"strings"
)

// emitText sends only the not-yet-sent suffix of the cumulative text.
func (c *relayConverter) emitText(text string) {
	if text == "" || text == c.lastText {
		return
	}
	delta := text
	if strings.HasPrefix(text, c.lastText) {
		delta = text[len(c.lastText):]
	}
	c.lastText = text
	if delta != "" {
		c.chunk(map[string]any{
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": delta}, "finish_reason": nil}},
		})
	}
}

func (c *relayConverter) emitThinking(text string) {
	if text == "" {
		return
	}
	c.chunk(map[string]any{
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{"reasoning_content": text}, "finish_reason": nil}},
	})
}

func (c *relayConverter) emitToolUse(tid, name string, input any) {
	if tid == "" {
		tid = fmt.Sprintf("%s-%d", name, c.toolSeq)
	}
	if c.seenTools[tid] {
		return
	}
	c.seenTools[tid] = true
	args := ""
	if input != nil {
		if s, ok := input.(string); ok {
			args = s
		} else if raw, err := json.Marshal(input); err == nil {
			args = string(raw)
		}
	}
	idx := c.toolSeq
	c.toolSeq++
	c.chunk(map[string]any{
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{"tool_calls": []map[string]any{
			{"index": idx, "id": tid, "type": "function", "function": map[string]any{"name": name, "arguments": args}},
		}}, "finish_reason": nil}},
	})
}

func (c *relayConverter) emitUsageDone(usage map[string]any) {
	chunk := map[string]any{
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
	}
	if usage != nil {
		in := usage["input"]
		if in == nil {
			in = usage["prompt_tokens"]
		}
		out := usage["output"]
		if out == nil {
			out = usage["completion_tokens"]
		}
		total := usage["total"]
		if total == nil {
			total = usage["total_tokens"]
		}
		chunk["usage"] = map[string]any{
			"prompt_tokens":     toInt64(in),
			"completion_tokens": toInt64(out),
			"total_tokens":      toInt64(total),
		}
	}
	c.chunk(chunk)
	c.finish()
}

func (c *relayConverter) emitError(msg string) {
	if msg != "" {
		c.emitText(msg)
	}
	c.chunk(map[string]any{
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
	})
	c.finish()
}

func (c *relayConverter) emitParts(parts []any) {
	for _, p := range parts {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		switch toString(pm["type"]) {
		case "text":
			c.emitText(toString(pm["text"]))
		case "thinking":
			c.emitThinking(toString(pm["thinking"]))
		case "tool_use", "toolCall":
			if name := toString(pm["name"]); name != "" {
				in := pm["input"]
				if in == nil {
					in = pm["arguments"]
				}
				c.emitToolUse(toString(pm["id"]), name, in)
			}
		}
	}
}

func (c *relayConverter) chunk(chunk map[string]any) {
	chunk["id"] = c.id
	chunk["object"] = "chat.completion.chunk"
	chunk["created"] = c.created
	chunk["model"] = c.model
	raw, err := json.Marshal(chunk)
	if err != nil {
		return
	}
	c.out.writeData(string(raw))
}

func (c *relayConverter) finish() {
	if c.done {
		return
	}
	c.done = true
	c.out.writeDone()
}
