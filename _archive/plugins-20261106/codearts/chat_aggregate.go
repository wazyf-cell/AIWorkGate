package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// ---- aggregate sink (stream:false) ----

type aggToolCall struct {
	index int
	id    string
	name  string
	args  strings.Builder
}

type aggregateSink struct {
	content   strings.Builder
	reasoning strings.Builder
	calls     map[int]*aggToolCall
	order     []int
	finish    string
	usage     json.RawMessage
}

func (a *aggregateSink) ensure() {
	if a.calls == nil {
		a.calls = map[int]*aggToolCall{}
	}
}

func (a *aggregateSink) Text(text string)      { a.content.WriteString(text) }
func (a *aggregateSink) Reasoning(text string) { a.reasoning.WriteString(text) }
func (a *aggregateSink) Usage(raw json.RawMessage) {
	if len(raw) > 0 {
		a.usage = raw
	}
}
func (a *aggregateSink) Finish(reason string) {
	if reason != "" {
		a.finish = reason
	}
}

func (a *aggregateSink) ToolDelta(index int, id, name, args string, announce bool) {
	a.ensure()
	call, ok := a.calls[index]
	if !ok {
		call = &aggToolCall{index: index}
		a.calls[index] = call
		a.order = append(a.order, index)
	}
	if id != "" {
		call.id = id
	}
	if name != "" {
		call.name = name
	}
	call.args.WriteString(args)
}

func (a *aggregateSink) empty() bool {
	return a.content.Len() == 0 && a.reasoning.Len() == 0 && len(a.order) == 0
}

func (a *aggregateSink) toolList() []map[string]any {
	if len(a.order) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(a.order))
	for _, idx := range a.order {
		call := a.calls[idx]
		if strings.TrimSpace(call.name) == "" {
			continue
		}
		id := call.id
		if id == "" {
			id = fmt.Sprintf("call_%d", idx)
		}
		out = append(out, map[string]any{
			"id":   id,
			"type": "function",
			"function": map[string]any{
				"name":      call.name,
				"arguments": call.args.String(),
			},
		})
	}
	return out
}

func (p *Plugin) writeAggregate(w http.ResponseWriter, model string, agg *aggregateSink, outcome streamOutcome) {
	content := agg.content.String()
	toolCalls := agg.toolList()
	reasoning := agg.reasoning.String()
	if content == "" && len(toolCalls) == 0 && reasoning != "" {
		// The reference adapter surfaces reasoning text when it is the only
		// output, so clients do not show an empty reply.
		content = reasoning
	}
	msg := map[string]any{"role": "assistant", "content": content}
	if reasoning != "" {
		msg["reasoning_content"] = reasoning
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	finish := outcome.finish
	if finish == "" {
		finish = agg.finish
	}
	if finish == "" {
		finish = "stop"
	}
	if len(toolCalls) > 0 {
		finish = "tool_calls"
	}
	resp := map[string]any{
		"id":      "chatcmpl-" + randomHex(8),
		"object":  "chat.completion",
		"created": p.now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       msg,
			"finish_reason": finish,
		}},
	}
	if len(agg.usage) > 0 {
		var u any
		if json.Unmarshal(agg.usage, &u) == nil {
			resp["usage"] = u
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
