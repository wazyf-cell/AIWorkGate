package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// chatMessage keeps content raw because OpenAI allows either a string or an
// array of content parts.
type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Name       string          `json:"name,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type chatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
	Tools    []chatTool    `json:"tools"`
	// HelloToolCall is a template-only switch: when true and tools are
	// present, the plugin answers with a tool_calls message so the host
	// passthrough can be verified end to end.
	HelloToolCall bool `json:"hello_tool_call"`
}

func (s *server) handleChat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "messages is required")
		return
	}

	reply := buildReply(req.Messages)
	toolName, toolArgs := "", ""
	if req.HelloToolCall && len(req.Tools) > 0 {
		toolName = strings.TrimSpace(req.Tools[0].Function.Name)
		if toolName == "" {
			toolName = "hello_tool"
		}
		args, _ := json.Marshal(map[string]string{"echo": reply})
		toolArgs = string(args)
	}

	model := req.Model
	if model == "" {
		model = "hello-chat"
	}
	id := fmt.Sprintf("chatcmpl-hello-%d", time.Now().UnixNano())
	created := time.Now().Unix()
	promptTokens := estimateTokens(allText(req.Messages))
	completionTokens := estimateTokens(reply)

	if req.Stream {
		s.streamChat(w, r, id, model, created, reply, toolName, toolArgs, promptTokens, completionTokens)
		return
	}

	message := map[string]any{"role": "assistant", "content": reply}
	finish := "stop"
	if toolName != "" {
		message["content"] = nil
		message["tool_calls"] = []map[string]any{toolCall(toolName, toolArgs)}
		finish = "tool_calls"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
		"usage": usage(promptTokens, completionTokens),
	})
}

func (s *server) streamChat(w http.ResponseWriter, r *http.Request, id, model string, created int64, reply, toolName, toolArgs string, promptTokens, completionTokens int) {
	setSSEHeaders(w)
	w.WriteHeader(http.StatusOK)

	cancelled := func() bool { return r.Context().Err() != nil }
	send := func(chunk map[string]any) bool {
		if cancelled() {
			return false
		}
		data, err := json.Marshal(chunk)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return false
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return true
	}
	chunk := func(delta map[string]any, finish any, u map[string]any) map[string]any {
		out := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []map[string]any{{
				"index":         0,
				"delta":         delta,
				"finish_reason": finish,
			}},
		}
		if u != nil {
			out["usage"] = u
		}
		return out
	}

	if !send(chunk(map[string]any{"role": "assistant", "content": ""}, nil, nil)) {
		return
	}
	if toolName != "" {
		delta := map[string]any{
			"tool_calls": []map[string]any{{
				"index": 0,
				"id":    fmt.Sprintf("call_hello_%d", created),
				"type":  "function",
				"function": map[string]any{
					"name":      toolName,
					"arguments": toolArgs,
				},
			}},
		}
		if !send(chunk(delta, nil, nil)) {
			return
		}
		if !send(chunk(map[string]any{}, "tool_calls", usage(promptTokens, completionTokens))) {
			return
		}
	} else {
		for _, piece := range splitRunes(reply, 24) {
			if !send(chunk(map[string]any{"content": piece}, nil, nil)) {
				return
			}
		}
		if !send(chunk(map[string]any{}, "stop", usage(promptTokens, completionTokens))) {
			return
		}
	}
	if cancelled() {
		return
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func setSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
}

func toolCall(name, args string) map[string]any {
	return map[string]any{
		"id":   fmt.Sprintf("call_hello_%d", time.Now().UnixNano()),
		"type": "function",
		"function": map[string]any{
			"name":      name,
			"arguments": args,
		},
	}
}

func usage(prompt, completion int) map[string]any {
	return map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      prompt + completion,
	}
}

// buildReply echoes the last user message. A real plugin would call its
// upstream here, through the egress URL from egress.json if one is assigned.
func buildReply(messages []chatMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "user" {
			continue
		}
		text := messageText(messages[i])
		if text == "" {
			return "Hello! 这是一条空消息。"
		}
		return "Hello! 你刚才说：" + text
	}
	return "Hello! 没有找到 user 消息。"
}

func messageText(m chatMessage) string {
	if len(m.Content) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(m.Content, &text); err == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &parts); err == nil {
		var b strings.Builder
		for _, part := range parts {
			if part.Text == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(part.Text)
		}
		return b.String()
	}
	return ""
}

func allText(messages []chatMessage) string {
	var b strings.Builder
	for _, m := range messages {
		text := messageText(m)
		if text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(text)
	}
	return b.String()
}

// estimateTokens is a rough 4-bytes-per-token heuristic, good enough for a
// template that never calls a real model.
func estimateTokens(s string) int {
	n := len([]rune(s))
	if n == 0 {
		return 0
	}
	if t := (n + 3) / 4; t > 1 {
		return t
	}
	return 1
}

func splitRunes(s string, n int) []string {
	runes := []rune(s)
	var out []string
	for i := 0; i < len(runes); i += n {
		end := i + n
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[i:end]))
	}
	return out
}
