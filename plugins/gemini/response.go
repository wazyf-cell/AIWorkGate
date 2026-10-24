// Package main — the response half of the Gemini conversion.
//
// The upstream answers /v1internal:streamGenerateContent?alt=sse with SSE frames
// whose JSON may be wrapped in {"response": {...}}; the reference unwraps either
// shape (consumeGeminiSse). Parts carry text, thought (reasoning) or functionCall,
// and usageMetadata carries the token counts.
//
// This port converts those frames into OpenAI chat.completion.chunk deltas for a
// streaming caller, and folds them into one chat.completion body otherwise.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// geminiFrame is the loose shape of one SSE data payload.
type geminiFrame struct {
	Response *geminiInner `json:"response"`
	// The unwrapped form carries the same fields inline.
	geminiInner
}

// geminiInner is the generateContent response body.
type geminiInner struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text             string `json:"text"`
				Thought          bool   `json:"thought"`
				ThoughtSignature string `json:"thoughtSignature"`
				FunctionCall     *struct {
					Name string          `json:"name"`
					Args json.RawMessage `json:"args"`
				} `json:"functionCall"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata *geminiUsage `json:"usageMetadata"`
}

// geminiUsage is usageMetadata.
type geminiUsage struct {
	PromptTokenCount        int `json:"promptTokenCount"`
	CachedContentTokenCount int `json:"cachedContentTokenCount"`
	CandidatesTokenCount    int `json:"candidatesTokenCount"`
	TotalTokenCount         int `json:"totalTokenCount"`
	ThoughtsTokenCount      int `json:"thoughtsTokenCount"`
}

// usageCounts is the OpenAI-side accounting.
type usageCounts struct {
	InputTokens  int
	OutputTokens int
}

// mapFinish is mapGeminiFinish: a turn that produced tool calls finishes as
// tool_calls regardless of what the upstream said.
func mapFinish(reason string, toolCalls int) string {
	if toolCalls > 0 {
		return "tool_calls"
	}
	switch strings.ToUpper(reason) {
	case "MAX_TOKENS":
		return "length"
	default:
		return "stop"
	}
}

// geminiTranslator accumulates the stream state needed to emit OpenAI deltas.
type geminiTranslator struct {
	emit      func(delta map[string]any, finish any)
	textBuf   strings.Builder
	toolCalls []toolCall
	usage     usageCounts
	finish    string
	// toolSeq numbers emitted tool calls so their delta index is stable.
	toolSeq int
	// onSignature captures each functionCall's thoughtSignature so the next
	// turn can carry it back. Nil means "no cache attached".
	onSignature func(name, argsJSON, sig string)
}

type toolCall struct {
	ID   string
	Name string
	Args string
}

func newGeminiTranslator(emit func(map[string]any, any)) *geminiTranslator {
	return &geminiTranslator{emit: emit}
}

// HandleFrame consumes one "data:" payload and reports whether an explicit
// terminal state was reached. Gemini signals completion with finishReason, not a
// sentinel, so this returns false in the normal case.
func (t *geminiTranslator) HandleFrame(payload string) error {
	payload = strings.TrimSpace(payload)
	if payload == "" || payload == "[DONE]" {
		return nil
	}
	var f geminiFrame
	if json.Unmarshal([]byte(payload), &f) != nil {
		return nil
	}
	inner := f.geminiInner
	if f.Response != nil {
		inner = *f.Response
	}
	if inner.UsageMetadata != nil {
		t.noteUsage(inner.UsageMetadata)
	}
	if len(inner.Candidates) == 0 {
		return nil
	}
	cand := inner.Candidates[0]
	if cand.FinishReason != "" {
		t.finish = cand.FinishReason
	}
	for _, part := range cand.Content.Parts {
		if part.FunctionCall != nil {
			args := "{}"
			if len(part.FunctionCall.Args) > 0 && string(part.FunctionCall.Args) != "null" {
				args = canonicalArgs(part.FunctionCall.Args)
			}
			if part.ThoughtSignature != "" && t.onSignature != nil {
				t.onSignature(part.FunctionCall.Name, args, part.ThoughtSignature)
			}
			id := newToolCallID(t.toolSeq)
			t.toolSeq++
			t.toolCalls = append(t.toolCalls, toolCall{ID: id, Name: part.FunctionCall.Name, Args: args})
			index := len(t.toolCalls) - 1
			t.emit(map[string]any{"tool_calls": []any{map[string]any{
				"index": index,
				"id":    id,
				"type":  "function",
				"function": map[string]any{
					"name":      part.FunctionCall.Name,
					"arguments": args,
				},
			}}}, nil)
			continue
		}
		if part.Text == "" {
			continue
		}
		if part.Thought {
			// Reasoning deltas are not part of the host's render path, so the
			// text is withheld (same choice as the zcode port).
			continue
		}
		t.textBuf.WriteString(part.Text)
		t.emit(map[string]any{"content": part.Text}, nil)
	}
	return nil
}

// noteUsage accepts a usage frame only when its total is not smaller than the last
// one seen, matching noteUsage in the reference (a later frame can restate the
// same counts).
func (t *geminiTranslator) noteUsage(u *geminiUsage) {
	t.usage.InputTokens = maxInt(0, u.PromptTokenCount-u.CachedContentTokenCount)
	t.usage.OutputTokens = u.CandidatesTokenCount
}

// canonicalArgs re-marshals a JSON object with sorted keys, which is what the
// reference's canonicalArgs does so a tool call's arguments compare stably.
func canonicalArgs(raw json.RawMessage) string {
	var parsed map[string]any
	if json.Unmarshal(raw, &parsed) != nil || parsed == nil {
		return "{}"
	}
	b, err := json.Marshal(sortValue(parsed))
	if err != nil {
		return "{}"
	}
	return string(b)
}

// sortValue recursively sorts object keys, mirroring the reference's sortValue.
func sortValue(v any) any {
	switch typed := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for k, child := range typed {
			out[k] = sortValue(child)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, child := range typed {
			out[i] = sortValue(child)
		}
		return out
	default:
		return v
	}
}

// newToolCallID is newToolCallId: "gemini_tool_<base36 ms>_<index>".
func newToolCallID(index int) string {
	return fmt.Sprintf("gemini_tool_%d_%d", nowMillis(), index)
}

// pumpGemini reads an SSE body and feeds the translator.
func pumpGemini(r io.Reader, t *geminiTranslator) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		// The reference ignores comments, named events and anything that is not
		// a data line.
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		if err := t.HandleFrame(strings.TrimSpace(strings.TrimPrefix(line, "data:"))); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// completionFromTranslator folds the observed stream into one chat.completion.
func completionFromTranslator(t *geminiTranslator, model string) []byte {
	msg := map[string]any{"role": "assistant", "content": t.textBuf.String()}
	if len(t.toolCalls) > 0 {
		calls := make([]any, 0, len(t.toolCalls))
		for _, c := range t.toolCalls {
			calls = append(calls, map[string]any{
				"id":   c.ID,
				"type": "function",
				"function": map[string]any{
					"name":      c.Name,
					"arguments": c.Args,
				},
			})
		}
		msg["tool_calls"] = calls
	}
	out := map[string]any{
		"id":     "chatcmpl-gemini",
		"object": "chat.completion",
		"model":  model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": mapFinish(t.finish, len(t.toolCalls)),
		}},
		"usage": map[string]any{
			"prompt_tokens":     t.usage.InputTokens,
			"completion_tokens": t.usage.OutputTokens,
			"total_tokens":      t.usage.InputTokens + t.usage.OutputTokens,
		},
	}
	b, _ := json.Marshal(out)
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
