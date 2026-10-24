// Package main — the streaming half of the zcode conversion.
//
// The upstream stream is Anthropic's named-event SSE (message_start,
// content_block_start/delta/stop, message_delta, message_stop). OpenAI clients
// expect bare data: frames carrying choices[].delta, and a streamed tool use
// arrives as content_block_start(type=tool_use) followed by input_json_delta
// fragments, which have to be re-emitted as tool_calls deltas.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// streamFrame is the subset of one Anthropic SSE payload the translation reads.
type streamFrame struct {
	Type         string `json:"type"`
	Index        int    `json:"index"`
	ContentBlock struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Message struct {
		Usage tokenUsage `json:"usage"`
	} `json:"message"`
	Usage tokenUsage `json:"usage"`
}

// streamOpenAI translates the Anthropic SSE stream into OpenAI chat.completion
// chunks and closes it with a usage chunk plus [DONE].
func (s *server) streamOpenAI(w http.ResponseWriter, up io.Reader, model string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming_unsupported", "writer cannot flush")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	id := "chatcmpl-zcode"
	usage := tokenUsage{}
	sentRole, sentFinish, sentTools := false, false, false
	toolIndex := map[int]int{}
	argsSeen := map[int]bool{}
	nextTool := 0

	write := func(chunk map[string]any) {
		b, err := json.Marshal(chunk)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	emit := func(delta map[string]any, finish any) {
		write(map[string]any{
			"id":     id,
			"object": "chat.completion.chunk",
			"model":  model,
			"choices": []any{map[string]any{
				"index":         0,
				"delta":         delta,
				"finish_reason": finish,
			}},
		})
	}
	emitToolCall := func(index int, callID, name, args string) {
		function := map[string]any{"arguments": args}
		call := map[string]any{"index": index, "function": function}
		if callID != "" {
			call["id"] = callID
			call["type"] = "function"
			function["name"] = name
		}
		emit(map[string]any{"tool_calls": []any{call}}, nil)
	}
	finish := func() {
		if !sentFinish {
			sentFinish = true
			reason := "stop"
			if sentTools {
				reason = "tool_calls"
			}
			emit(map[string]any{}, reason)
		}
	}
	done := func() {
		if usage.InputTokens > 0 || usage.OutputTokens > 0 ||
			usage.CacheCreationInputTokens > 0 || usage.CacheReadInputTokens > 0 {
			write(map[string]any{
				"id":      id,
				"object":  "chat.completion.chunk",
				"model":   model,
				"choices": []any{},
				"usage":   usage.openAI(),
			})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}

	scanner := bufio.NewScanner(up)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	event := ""
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var frame streamFrame
		if json.Unmarshal([]byte(data), &frame) != nil {
			continue
		}
		switch firstNonEmpty(frame.Type, event) {
		case "message_start":
			usage.merge(frame.Message.Usage)
			if !sentRole {
				sentRole = true
				emit(map[string]any{"role": "assistant", "content": ""}, nil)
			}
		case "content_block_start":
			if frame.ContentBlock.Type == "tool_use" {
				sentTools = true
				toolIndex[frame.Index] = nextTool
				emitToolCall(nextTool, frame.ContentBlock.ID, frame.ContentBlock.Name, "")
				nextTool++
			}
		case "content_block_delta":
			switch frame.Delta.Type {
			case "text_delta":
				if frame.Delta.Text != "" {
					emit(map[string]any{"content": frame.Delta.Text}, nil)
				}
			case "input_json_delta":
				if index, ok := toolIndex[frame.Index]; ok && frame.Delta.PartialJSON != "" {
					argsSeen[frame.Index] = true
					emitToolCall(index, "", "", frame.Delta.PartialJSON)
				}
			case "thinking_delta":
				// OpenAI has no reasoning field in the delta contract; the
				// host renders content only, so thinking is not forwarded.
			}
		case "content_block_stop":
			if index, ok := toolIndex[frame.Index]; ok {
				if !argsSeen[frame.Index] {
					emitToolCall(index, "", "", "{}")
				}
				delete(toolIndex, frame.Index)
				delete(argsSeen, frame.Index)
			}
		case "message_delta":
			usage.merge(frame.Usage)
			if frame.Delta.StopReason != "" && !sentFinish {
				sentFinish = true
				emit(map[string]any{}, mapStopReason(frame.Delta.StopReason))
			}
		case "message_stop":
			finish()
			done()
			return
		}
	}
	finish()
	done()
}
