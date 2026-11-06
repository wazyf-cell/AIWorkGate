package main

import (
	"encoding/json"
	"fmt"
	"net/http"
)

type chatSink interface {
	Text(string)
	Reasoning(string)
	ToolDelta(index int, id, name, args string, announce bool)
	Usage(json.RawMessage)
	Finish(string)
}

// ---- streaming sink ----

type sseSink struct {
	p       *Plugin
	w       http.ResponseWriter
	r       *http.Request
	id      string
	model   string
	created int64

	started    bool
	roleSent   bool
	finishSent bool
	lastFinish string
	usage      json.RawMessage
	wroteAny   bool
}

func (s *sseSink) writeChunk(delta map[string]any, finish any) bool {
	if !s.started {
		setSSEHeaders(s.w)
		s.w.WriteHeader(http.StatusOK)
		s.started = true
	}
	if !s.roleSent {
		delta["role"] = "assistant"
		s.roleSent = true
	}
	chunk := map[string]any{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []map[string]any{{
			"index":         0,
			"delta":         delta,
			"finish_reason": finish,
		}},
	}
	if len(s.usage) > 0 && finish != nil {
		var u any
		if json.Unmarshal(s.usage, &u) == nil {
			chunk["usage"] = u
		}
	}
	b, err := json.Marshal(chunk)
	if err != nil {
		return false
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", b); err != nil {
		return false
	}
	flushWriter(s.w)
	s.wroteAny = true
	return true
}

func (s *sseSink) Text(text string) {
	if text == "" {
		return
	}
	s.writeChunk(map[string]any{"content": text}, nil)
}

func (s *sseSink) Reasoning(text string) {
	if text == "" {
		return
	}
	s.writeChunk(map[string]any{"reasoning_content": text}, nil)
}

func (s *sseSink) ToolDelta(index int, id, name, args string, announce bool) {
	call := map[string]any{
		"index": index,
		"type":  "function",
		"function": map[string]any{
			"arguments": args,
		},
	}
	if id != "" {
		call["id"] = id
	}
	if name != "" && (announce || id == "") {
		call["function"].(map[string]any)["name"] = name
	}
	s.writeChunk(map[string]any{"tool_calls": []map[string]any{call}}, nil)
}

func (s *sseSink) Usage(raw json.RawMessage) {
	if len(raw) > 0 {
		s.usage = raw
	}
}

func (s *sseSink) Finish(reason string) {
	if reason != "" {
		s.lastFinish = reason
	}
}

func (s *sseSink) finalize(outcome streamOutcome) {
	if s.finishSent {
		return
	}
	if !s.started {
		// Empty but successful response: still emit a role chunk.
		s.writeChunk(map[string]any{"content": ""}, nil)
	}
	finish := outcome.finish
	if finish == "" {
		finish = s.lastFinish
	}
	if outcome.hasTools {
		finish = "tool_calls"
	}
	if finish == "" {
		finish = "stop"
	}
	if !s.writeChunk(map[string]any{}, finish) {
		return
	}
	s.finishSent = true
	_, _ = fmt.Fprint(s.w, "data: [DONE]\n\n")
	flushWriter(s.w)
}

func (s *sseSink) writeError(code, message string) {
	payload, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": message, "type": code, "code": code,
	}})
	if !s.started {
		writeError(s.w, http.StatusBadGateway, code, message)
		return
	}
	_, _ = fmt.Fprintf(s.w, "data: %s\n\n", payload)
	_, _ = fmt.Fprint(s.w, "data: [DONE]\n\n")
	flushWriter(s.w)
	s.finishSent = true
}
