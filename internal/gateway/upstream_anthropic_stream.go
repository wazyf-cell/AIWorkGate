package gateway

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// This file translates native Anthropic Messages replies into chat JSON and
// chat.completion.chunk SSE frames, and holds the shared block types.

type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	Source    map[string]any  `json:"source,omitempty"`
}

// anthropicTokenUsage is the wire usage object. The name avoids colliding with
// the existing anthropicUsage() builder in anthropic.go.
type anthropicTokenUsage struct {
	InputTokens          int `json:"input_tokens"`
	OutputTokens         int `json:"output_tokens"`
	CacheReadInputTokens int `json:"cache_read_input_tokens"`
}

// anthropicOutMessage is one outbound Messages turn.
type anthropicOutMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

type anthropicEvent struct {
	Index      int                  `json:"index"`
	ID         string               `json:"id"`
	Content    []anthropicBlock     `json:"content"`
	StopReason string               `json:"stop_reason"`
	Usage      *anthropicTokenUsage `json:"usage"`
	Message    *struct {
		ID    string              `json:"id"`
		Usage anthropicTokenUsage `json:"usage"`
	} `json:"message"`
	ContentBlock *anthropicBlock `json:"content_block"`
	Delta        *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type anthropicStreamState struct {
	id, model, stopReason string
	created               int64
	sentFinish, done      bool
	prompt, output, cache int
	tools                 map[int]int
}

var chatToAnthropicThinkingBudgets = map[string]int{"minimal": 1024, "low": 2048, "medium": 4096, "high": 8192, "xhigh": 16384, "max": 32768}

var anthropicFinishReasons = map[string]string{"max_tokens": "length", "tool_use": "tool_calls", "refusal": "content_filter"}

func anthropicResponseToChat(body []byte, clientModel string) ([]byte, error) {
	var resp anthropicEvent
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, &envelopeError{"upstream returned a non-Anthropic JSON body"}
	}
	var content, reasoning strings.Builder
	var calls []map[string]any
	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			content.WriteString(block.Text)
		case "thinking":
			reasoning.WriteString(block.Thinking)
		case "tool_use":
			args := "{}"
			if len(block.Input) > 0 && string(block.Input) != "null" {
				args = string(block.Input)
			}
			calls = append(calls, map[string]any{"id": block.ID, "type": "function",
				"function": map[string]any{"name": block.Name, "arguments": args}})
		}
	}
	message := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(calls) > 0 {
		message["tool_calls"] = calls
	}
	usage := anthropicTokenUsage{}
	if resp.Usage != nil {
		usage = *resp.Usage
	}
	reply := map[string]any{"id": firstNonEmpty(resp.ID, "chatcmpl-anthropic"), "object": "chat.completion", "created": time.Now().Unix(), "model": clientModel,
		"choices": []map[string]any{{"index": 0, "message": message, "finish_reason": anthropicFinishReason(resp.StopReason)}},
		"usage":   usage.chatUsage()}
	return json.Marshal(reply)
}

func anthropicFinishReason(reason string) string {
	if finish, ok := anthropicFinishReasons[strings.ToLower(strings.TrimSpace(reason))]; ok {
		return finish
	}
	return "stop"
}

func (u anthropicTokenUsage) chatUsage() map[string]any {
	usage := map[string]any{"prompt_tokens": u.InputTokens, "completion_tokens": u.OutputTokens, "total_tokens": u.InputTokens + u.OutputTokens}
	if u.CacheReadInputTokens > 0 {
		usage["prompt_tokens_details"] = map[string]any{"cached_tokens": u.CacheReadInputTokens}
	}
	return usage
}

// chatToAnthropicThinking clamps the budget because the upstream requires
// max_tokens > budget_tokens; a clamp below 1024 drops thinking.
func chatToAnthropicThinking(tier string, maxTokens int) (map[string]any, bool) {
	budget, ok := chatToAnthropicThinkingBudgets[strings.ToLower(strings.TrimSpace(tier))]
	if !ok {
		return nil, false
	}
	budget = min(budget, maxTokens-1)
	if budget < 1024 {
		return nil, false
	}
	return map[string]any{"type": "enabled", "budget_tokens": budget}, true
}

func anthropicStreamToChat(src io.Reader, dst io.Writer, clientModel string) error {
	state := &anthropicStreamState{id: "chatcmpl-anthropic", model: clientModel, created: time.Now().Unix(), stopReason: "stop", tools: map[int]int{}}
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var event string
	var data []string
	dispatch := func() error {
		payload, kind := strings.Join(data, "\n"), event
		event, data = "", nil
		if kind == "" || payload == "" {
			return nil
		}
		return state.consume(dst, kind, []byte(payload))
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event = strings.TrimSpace(value)
		case "data":
			data = append(data, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if err := dispatch(); err != nil {
		return err
	}
	return state.finish(dst)
}

func (s *anthropicStreamState) consume(dst io.Writer, kind string, data []byte) error {
	var event anthropicEvent
	if json.Unmarshal(data, &event) != nil {
		return nil
	}
	switch kind {
	case "message_start":
		if event.Message != nil {
			s.id = firstNonEmpty(event.Message.ID, s.id)
			s.prompt = event.Message.Usage.InputTokens
			s.cache = event.Message.Usage.CacheReadInputTokens
		}
		return s.emit(dst, map[string]any{"role": "assistant"}, nil, nil)
	case "content_block_start":
		if event.ContentBlock == nil {
			return nil
		}
		switch event.ContentBlock.Type {
		case "text":
			return s.delta(dst, "content", event.ContentBlock.Text)
		case "thinking":
			return s.delta(dst, "reasoning_content", event.ContentBlock.Thinking)
		case "tool_use":
			index := len(s.tools)
			s.tools[event.Index] = index
			call := map[string]any{"index": index, "id": event.ContentBlock.ID, "type": "function",
				"function": map[string]any{"name": event.ContentBlock.Name, "arguments": ""}}
			return s.emit(dst, map[string]any{"tool_calls": []map[string]any{call}}, nil, nil)
		}
	case "content_block_delta":
		if event.Delta == nil {
			return nil
		}
		switch event.Delta.Type {
		case "text_delta":
			return s.delta(dst, "content", event.Delta.Text)
		case "thinking_delta":
			return s.delta(dst, "reasoning_content", event.Delta.Thinking)
		case "input_json_delta":
			call := map[string]any{"index": s.tools[event.Index],
				"function": map[string]any{"arguments": event.Delta.PartialJSON}}
			return s.emit(dst, map[string]any{"tool_calls": []map[string]any{call}}, nil, nil)
		}
	case "message_delta":
		if event.Delta != nil {
			s.stopReason = anthropicFinishReason(event.Delta.StopReason)
		}
		if event.Usage != nil {
			s.output = event.Usage.OutputTokens
			if event.Usage.CacheReadInputTokens > 0 {
				s.cache = event.Usage.CacheReadInputTokens
			}
		}
		return s.emitFinish(dst)
	case "message_stop":
		return s.finish(dst)
	case "error":
		return s.fail(dst, &event)
	}
	return nil
}

func (s *anthropicStreamState) delta(dst io.Writer, key, text string) error {
	if text == "" {
		return nil
	}
	return s.emit(dst, map[string]any{key: text}, nil, nil)
}

func (s *anthropicStreamState) emit(dst io.Writer, delta map[string]any, finish any, usage map[string]any) error {
	frame := map[string]any{"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
		"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}}}
	if usage != nil {
		frame["usage"] = usage
	}
	return anthropicWriteFrame(dst, frame)
}

func (s *anthropicStreamState) emitFinish(dst io.Writer) error {
	if s.sentFinish {
		return nil
	}
	s.sentFinish = true
	usage := anthropicTokenUsage{InputTokens: s.prompt, OutputTokens: s.output, CacheReadInputTokens: s.cache}
	return s.emit(dst, map[string]any{}, s.stopReason, usage.chatUsage())
}

func (s *anthropicStreamState) finish(dst io.Writer) error {
	if s.done {
		return nil
	}
	if !s.sentFinish {
		if err := s.emitFinish(dst); err != nil {
			return err
		}
	}
	s.done = true
	_, err := io.WriteString(dst, "data: [DONE]\n\n")
	return err
}

func (s *anthropicStreamState) fail(dst io.Writer, event *anthropicEvent) error {
	message := "upstream error"
	if event.Error != nil {
		message = firstNonEmpty(event.Error.Message, event.Error.Type, message)
	}
	frame := map[string]any{"error": map[string]any{"message": message, "type": "upstream_error"}}
	if err := anthropicWriteFrame(dst, frame); err != nil {
		return err
	}
	if _, err := io.WriteString(dst, "data: [DONE]\n\n"); err != nil {
		return err
	}
	s.done = true
	return &envelopeError{"anthropic stream error: " + message}
}

func anthropicWriteFrame(dst io.Writer, frame map[string]any) error {
	data, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(dst, "data: %s\n\n", data)
	return err
}
