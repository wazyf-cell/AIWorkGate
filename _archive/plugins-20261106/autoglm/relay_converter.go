package main

// Relay event converter: /api/events frames to OpenAI chunks.
//
// Ported from chat.go relayConverter. The relay reports *cumulative* text, so a
// chunk's content is only the suffix that has not been sent yet (emitText keeps
// lastText for exactly that reason).

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ---- relay converter: /api/events frames -> OpenAI chunks ----

type relayConverter struct {
	runID     string
	session   string
	model     string
	out       chunkOut
	id        string
	created   int64
	lastText  string
	toolSeq   int
	seenTools map[string]bool
	done      bool
}

func newRelayConverter(runID, session string, body []byte, out chunkOut) *relayConverter {
	created := time.Now()
	return &relayConverter{
		runID:     runID,
		session:   session,
		model:     peekModel(body),
		out:       out,
		id:        fmt.Sprintf("chatcmpl-%d", created.UnixNano()),
		created:   created.Unix(),
		seenTools: map[string]bool{},
	}
}

// handleFrame decodes one SSE frame (blank-line delimited). The relay wraps
// every event as {"event":..., "payload":{...}}.
func (c *relayConverter) handleFrame(raw string) {
	if c.done {
		return
	}
	data := ""
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(line, "data:") {
			data += line[5:]
		}
	}
	if data == "" {
		return
	}
	var ev struct {
		Event   string         `json:"event"`
		Payload map[string]any `json:"payload"`
	}
	if json.Unmarshal([]byte(data), &ev) != nil {
		return
	}
	if ev.Payload == nil {
		return
	}
	c.handle(ev.Event, ev.Payload)
}

// handle filters out other sessions' traffic before dispatching.
func (c *relayConverter) handle(event string, payload map[string]any) {
	runID := toString(payload["runId"])
	sk := toString(payload["sessionKey"])
	to := toString(payload["to"])
	sameRun := runID != "" && c.runID != "" && runID == c.runID
	sameSession := (sk != "" && sk == c.session) || (to != "" && to == c.session)
	if !sameRun && !(sameSession && runID == "") {
		return
	}
	switch event {
	case "agent:stream", "stream", "chat:message", "chat.delta":
		c.handleAgentStream(payload)
	case "chat":
		c.handleChat(payload)
	case "agent":
		c.handleAgent(payload)
	}
}

func (c *relayConverter) handleAgentStream(payload map[string]any) {
	if c.done {
		return
	}
	switch toString(payload["type"]) {
	case "error":
		c.emitError(payloadMsg(payload))
	case "done":
		c.emitUsageDone(extractUsage(payload))
	case "text":
		delta := toString(payload["delta"])
		if delta == "" {
			delta = toString(payload["text"])
		}
		c.emitText(delta)
	case "thinking":
		t := toString(payload["text"])
		if t == "" {
			t = toString(payload["delta"])
		}
		c.emitThinking(t)
	case "tool_call", "tool_use":
		name := toString(payload["name"])
		if name == "" {
			name = toString(payload["tool"])
		}
		if name != "" {
			c.emitToolUse(toString(payload["id"]), name, orInput(payload))
		}
	}
	if msg, ok := payload["message"].(map[string]any); ok {
		switch ct := msg["content"].(type) {
		case string:
			c.emitText(ct)
		case []any:
			c.emitParts(ct)
		}
	}
}

func (c *relayConverter) handleChat(payload map[string]any) {
	state := toString(payload["state"])
	if state == "error" {
		c.emitError(payloadMsg(payload))
		return
	}
	msg, _ := payload["message"].(map[string]any)
	if msg == nil {
		if state == "final" {
			if u := extractUsage(payload); u != nil {
				c.emitUsageDone(u)
			}
		}
		return
	}
	switch ct := msg["content"].(type) {
	case string:
		c.emitText(ct)
	case []any:
		c.emitParts(ct)
	}
	if tcs, ok := msg["toolCalls"].([]any); ok {
		for _, tc := range tcs {
			tcm, ok := tc.(map[string]any)
			if !ok {
				continue
			}
			name := toString(tcm["name"])
			if name == "" {
				if fn, ok := tcm["function"].(map[string]any); ok {
					name = toString(fn["name"])
				}
			}
			if name != "" {
				c.emitToolUse(toString(tcm["id"]), name, orInput(tcm))
			}
		}
	}
	if state == "final" {
		if u := extractUsage(payload["usage"]); u != nil {
			c.emitUsageDone(u)
		} else if u := extractUsage(payload); u != nil {
			c.emitUsageDone(u)
		}
	}
}

func (c *relayConverter) handleAgent(payload map[string]any) {
	stream := toString(payload["stream"])
	data, _ := payload["data"].(map[string]any)
	if data == nil {
		data = payload
	}
	switch stream {
	case "assistant":
		parts, ok := data["content"].([]any)
		if !ok {
			parts, ok = data["blocks"].([]any)
		}
		if ok {
			c.emitParts(parts)
		} else {
			t := toString(data["text"])
			if t == "" {
				t = toString(data["delta"])
			}
			c.emitText(t)
		}
	case "tool":
		if toString(data["phase"]) == "start" {
			if name := toString(data["name"]); name != "" {
				in := data["args"]
				if in == nil {
					in = data["input"]
				}
				c.emitToolUse(toString(data["toolCallId"]), name, in)
			}
		}
	case "lifecycle":
		switch toString(data["phase"]) {
		case "end":
			if u := extractUsage(data["usage"]); u != nil {
				c.emitUsageDone(u)
			} else if u := extractUsage(payload); u != nil {
				c.emitUsageDone(u)
			}
		case "error":
			c.emitError(payloadMsg(data))
		}
	case "error":
		c.emitError(payloadMsg(data))
	}
}
