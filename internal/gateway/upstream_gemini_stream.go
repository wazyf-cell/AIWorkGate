package gateway

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// This file translates Gemini generateContent replies and
// streamGenerateContent?alt=sse feeds back into chat completions JSON and
// chat.completion.chunk frames. Gemini can deliver a whole function call in one
// event, so each functionCall becomes one complete tool_calls delta.

type geminiUsage struct {
	PromptTokenCount        int `json:"promptTokenCount"`
	CandidatesTokenCount    int `json:"candidatesTokenCount"`
	CachedContentTokenCount int `json:"cachedContentTokenCount"`
	ThoughtsTokenCount      int `json:"thoughtsTokenCount"`
	TotalTokenCount         int `json:"totalTokenCount"`
}

type geminiCandidate struct {
	Content struct {
		Parts []geminiPart `json:"parts"`
	} `json:"content"`
	FinishReason string `json:"finishReason"`
}

type geminiResponse struct {
	Candidates     []geminiCandidate `json:"candidates"`
	PromptFeedback struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
	UsageMetadata *geminiUsage `json:"usageMetadata"`
}

func geminiResponseToChat(body []byte, clientModel string) ([]byte, error) {
	var resp geminiResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, &envelopeError{"upstream returned a non-Gemini JSON body"}
	}
	reply := map[string]any{
		"id":      "chatcmpl-gemini-" + newReqID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   clientModel,
	}
	if len(resp.Candidates) == 0 {
		if resp.PromptFeedback.BlockReason == "" {
			return nil, &envelopeError{"gemini response contains no candidates"}
		}
		reply["choices"] = []map[string]any{geminiChatChoice("", "", nil, "content_filter")}
	} else {
		candidate := resp.Candidates[0]
		content, reasoning, calls := geminiCollectParts(candidate.Content.Parts)
		toolCalls := make([]map[string]any, 0, len(calls))
		for i, call := range calls {
			toolCalls = append(toolCalls, geminiToolCall(i, call))
		}
		reply["choices"] = []map[string]any{
			geminiChatChoice(content, reasoning, toolCalls, geminiFinishReason(candidate.FinishReason)),
		}
	}
	if usage := resp.UsageMetadata.chatUsage(); usage != nil {
		reply["usage"] = usage
	}
	return json.Marshal(reply)
}

// geminiCollectParts splits parts into text, thought text and function calls.
func geminiCollectParts(parts []geminiPart) (string, string, []*geminiFunctionCall) {
	var content, reasoning strings.Builder
	var calls []*geminiFunctionCall
	for _, part := range parts {
		switch {
		case part.FunctionCall != nil:
			calls = append(calls, part.FunctionCall)
		case part.Thought:
			reasoning.WriteString(part.Text)
		default:
			content.WriteString(part.Text)
		}
	}
	return content.String(), reasoning.String(), calls
}

func geminiChatChoice(content, reasoning string, calls []map[string]any, finish string) map[string]any {
	message := map[string]any{"role": "assistant", "content": content}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	if len(calls) > 0 {
		message["tool_calls"] = calls
	}
	return map[string]any{"index": 0, "message": message, "finish_reason": finish}
}

func geminiToolCall(index int, call *geminiFunctionCall) map[string]any {
	return map[string]any{
		"id":   fmt.Sprintf("call_%d", index),
		"type": "function",
		"function": map[string]any{
			"name":      call.Name,
			"arguments": string(geminiArgsObject(string(call.Args))),
		},
	}
}

func geminiFinishReason(reason string) string {
	switch strings.ToUpper(strings.TrimSpace(reason)) {
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return "content_filter"
	default:
		return "stop"
	}
}

// chatUsage maps Gemini token counters onto the chat usage object. Details are
// emitted only when the upstream reports them.
func (u *geminiUsage) chatUsage() map[string]any {
	if u == nil {
		return nil
	}
	total := u.TotalTokenCount
	if total <= 0 {
		total = u.PromptTokenCount + u.CandidatesTokenCount + u.ThoughtsTokenCount
	}
	usage := map[string]any{
		"prompt_tokens":     u.PromptTokenCount,
		"completion_tokens": u.CandidatesTokenCount,
		"total_tokens":      total,
	}
	if u.CachedContentTokenCount > 0 {
		usage["prompt_tokens_details"] = map[string]any{"cached_tokens": u.CachedContentTokenCount}
	}
	if u.ThoughtsTokenCount > 0 {
		usage["completion_tokens_details"] = map[string]any{"reasoning_tokens": u.ThoughtsTokenCount}
	}
	return usage
}

// geminiStreamState carries the per-stream identity and what has been emitted.
type geminiStreamState struct {
	id         string
	created    int64
	model      string
	sentRole   bool
	sentFinish bool
	blocked    bool
	nextCall   int
	usage      *geminiUsage
}

// geminiStreamToChat consumes the Gemini SSE feed and writes chat SSE frames,
// always terminating with data: [DONE]. Malformed events are skipped.
func geminiStreamToChat(src io.Reader, dst io.Writer, clientModel string) error {
	state := &geminiStreamState{
		id:      "chatcmpl-gemini-" + newReqID(),
		created: time.Now().Unix(),
		model:   clientModel,
	}
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		payload, ok := strings.CutPrefix(strings.TrimSpace(scanner.Text()), "data:")
		if !ok {
			continue
		}
		payload = strings.TrimSpace(payload)
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk geminiResponse
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if err := state.consume(dst, &chunk); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return state.finish(dst)
}

// consume turns one Gemini event into zero or more chat chunks.
func (s *geminiStreamState) consume(dst io.Writer, chunk *geminiResponse) error {
	if chunk.UsageMetadata != nil {
		s.usage = chunk.UsageMetadata
	}
	if chunk.PromptFeedback.BlockReason != "" && len(chunk.Candidates) == 0 {
		s.blocked = true
	}
	if len(chunk.Candidates) == 0 {
		return nil
	}
	candidate := chunk.Candidates[0]
	content, reasoning, calls := geminiCollectParts(candidate.Content.Parts)
	delta := map[string]any{}
	if reasoning != "" {
		delta["reasoning_content"] = reasoning
	}
	if content != "" {
		delta["content"] = content
	}
	if len(calls) > 0 {
		toolCalls := make([]map[string]any, 0, len(calls))
		for _, call := range calls {
			toolCalls = append(toolCalls, geminiToolCall(s.nextCall, call))
			s.nextCall++
		}
		delta["tool_calls"] = toolCalls
	}
	if len(delta) > 0 {
		if err := s.emit(dst, delta, nil); err != nil {
			return err
		}
	}
	if candidate.FinishReason != "" {
		return s.emitFinish(dst, geminiFinishReason(candidate.FinishReason))
	}
	return nil
}

// emit writes one chunk, stamping role:"assistant" on the very first one.
func (s *geminiStreamState) emit(dst io.Writer, delta map[string]any, finish any) error {
	if !s.sentRole {
		delta["role"] = "assistant"
		s.sentRole = true
	}
	frame := s.frame()
	frame["choices"] = []map[string]any{{
		"index":         0,
		"delta":         delta,
		"finish_reason": finish,
	}}
	return geminiWriteFrame(dst, frame)
}

func (s *geminiStreamState) emitFinish(dst io.Writer, reason string) error {
	if s.sentFinish {
		return nil
	}
	s.sentFinish = true
	return s.emit(dst, map[string]any{}, reason)
}

// finish closes the stream: a synthesized finish_reason when the upstream never
// sent one, the usage-only chunk when counters were reported, and [DONE].
func (s *geminiStreamState) finish(dst io.Writer) error {
	if !s.sentFinish {
		reason := "stop"
		if s.blocked {
			reason = "content_filter"
		}
		if err := s.emitFinish(dst, reason); err != nil {
			return err
		}
	}
	if usage := s.usage.chatUsage(); usage != nil {
		frame := s.frame()
		frame["choices"] = []map[string]any{}
		frame["usage"] = usage
		if err := geminiWriteFrame(dst, frame); err != nil {
			return err
		}
	}
	_, err := io.WriteString(dst, "data: [DONE]\n\n")
	return err
}

func (s *geminiStreamState) frame() map[string]any {
	return map[string]any{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
	}
}

func geminiWriteFrame(dst io.Writer, frame map[string]any) error {
	data, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(dst, "data: %s\n\n", data)
	return err
}
