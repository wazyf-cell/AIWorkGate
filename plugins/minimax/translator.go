package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// HandleFrame consumes one raw SSE "data:" payload. It reports wantDone when the
// upstream said message_stop, and an error for an in-stream error frame.
func (t *translator) HandleFrame(payload string) (stop bool, err error) {
	payload = strings.TrimSpace(payload)
	if payload == "" || payload == "[DONE]" {
		return false, nil
	}
	var f sseFrame
	if json.Unmarshal([]byte(payload), &f) != nil {
		return false, nil
	}
	switch f.Type {
	case "error":
		return false, fmt.Errorf("upstream stream error: %s", errorMessage(f.Error))
	case "message_start":
		var msg struct {
			Usage json.RawMessage `json:"usage"`
		}
		if len(f.Message) > 0 {
			_ = json.Unmarshal(f.Message, &msg)
			u := minimaxUsage(msg.Usage)
			if u.InputTokens > 0 {
				t.state.usage.InputTokens = u.InputTokens
			}
		}
	case "content_block_start":
		var cb contentBlock
		if len(f.ContentBlock) == 0 || json.Unmarshal(f.ContentBlock, &cb) != nil {
			return false, nil
		}
		t.state.blocks[f.Index] = &blockState{kind: cb.Type, id: cb.ID, name: cb.Name}
	case "content_block_delta":
		var d blockDelta
		if len(f.Delta) == 0 || json.Unmarshal(f.Delta, &d) != nil {
			return false, nil
		}
		b := t.state.blocks[f.Index]
		switch d.Type {
		case "text_delta":
			if b != nil {
				b.text += d.Text
			}
			if d.Text != "" {
				t.textBuf.WriteString(d.Text)
				t.emit(map[string]any{"content": d.Text}, nil)
			}
		case "thinking_delta":
			// The host renders content only, so reasoning is accumulated for
			// completeness but never forwarded (same choice as the zcode port).
			if b != nil {
				b.text += d.Thinking
			}
		case "input_json_delta":
			if b != nil {
				b.toolArgs += d.PartialJSON
			}
		}
	case "content_block_stop":
		b := t.state.blocks[f.Index]
		if b == nil {
			return false, nil
		}
		if b.kind == "tool_use" {
			args := b.toolArgs
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			t.toolCall = append(t.toolCall, toolCall{ID: b.id, Name: b.name, Args: args})
			t.emit(map[string]any{"tool_calls": []any{map[string]any{
				"index": len(t.toolCall) - 1,
				"id":    b.id,
				"type":  "function",
				"function": map[string]any{
					"name":      b.name,
					"arguments": args,
				},
			}}}, nil)
		}
		delete(t.state.blocks, f.Index)
	case "message_delta":
		var d blockDelta
		if len(f.Delta) > 0 {
			_ = json.Unmarshal(f.Delta, &d)
			if d.StopReason != "" {
				t.state.stop = d.StopReason
			}
		}
		if u := minimaxUsage(f.Usage); u.OutputTokens > 0 || u.InputTokens > 0 {
			if u.InputTokens > 0 {
				t.state.usage.InputTokens = u.InputTokens
			}
			if u.OutputTokens > 0 {
				t.state.usage.OutputTokens = u.OutputTokens
			}
		}
	case "message_stop":
		return true, nil
	}
	return false, nil
}

func errorMessage(raw json.RawMessage) string {
	var e struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	}
	if len(raw) > 0 && json.Unmarshal(raw, &e) == nil {
		if e.Message != "" {
			return e.Message
		}
		if e.Type != "" {
			return e.Type
		}
	}
	return "unknown error"
}

// pump reads an Anthropic-shaped SSE body line by line and feeds the translator.
// It stops early when the upstream sends message_stop, and always returns the
// stop reason it observed ("" when the stream ended without one).
func pump(r io.Reader, t *translator) (string, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		done, err := t.HandleFrame(payload)
		if err != nil {
			return t.state.stop, err
		}
		if done {
			return t.state.stop, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return t.state.stop, err
	}
	return t.state.stop, nil
}
