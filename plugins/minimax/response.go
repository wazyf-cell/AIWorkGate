// Package main — the response half of the MiniMax conversion.
//
// MiniMax has no non-streaming mode in the reference implementation: the adapter
// always sends stream:true and consumes an Anthropic-shaped SSE stream
// (consumeMinimaxSse). This plugin therefore always asks upstream for a stream and
// reshapes it for the host:
//
//   - when the host asked for stream, each frame is translated into an OpenAI
//     chat.completion.chunk and forwarded as it arrives;
//   - when the host did not, the same translation runs and the deltas are folded
//     into one chat.completion body.
//
// The upstream event vocabulary is Anthropic's (message_start / content_block_* /
// message_delta), so nothing here invents a MiniMax-only shape.
package main

import (
	"encoding/json"
	"strings"
)

// sseFrame is one decoded upstream event. Deliberately loose: the reference
// tolerates unknown frames by ignoring them, and so does this.
type sseFrame struct {
	Type string `json:"type"`
	// Index is the content block index, meaningful on content_block_* events.
	Index        int             `json:"index"`
	ContentBlock json.RawMessage `json:"content_block"`
	Delta        json.RawMessage `json:"delta"`
	Message      json.RawMessage `json:"message"`
	Usage        json.RawMessage `json:"usage"`
	Error        json.RawMessage `json:"error"`
}

// contentBlock is the subset of content_block this plugin reads.
type contentBlock struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

// blockDelta is the subset of delta this plugin reads.
type blockDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Thinking    string `json:"thinking"`
	PartialJSON string `json:"partial_json"`
	StopReason  string `json:"stop_reason"`
}

// usageCounts is the token accounting MiniMax reports.
type usageCounts struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// minimaxUsage reads an Anthropic usage object, including the cache and thinking
// detail fields the reference maps.
func minimaxUsage(raw json.RawMessage) usageCounts {
	var u struct {
		InputTokens         int `json:"input_tokens"`
		OutputTokens        int `json:"output_tokens"`
		CacheReadInputToken int `json:"cache_read_input_tokens"`
		OutputTokensDetails struct {
			ThinkingTokens int `json:"thinking_tokens"`
		} `json:"output_tokens_details"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &u) != nil {
		return usageCounts{}
	}
	return usageCounts{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens}
}

// mapStopReason translates an Anthropic stop_reason into OpenAI's finish_reason.
// The reference's mapMinimaxStopReason collapses refusal to a plain stop.
func mapStopReason(r string) string {
	switch r {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}

// streamState accumulates the per-index block state a stream needs. MiniMax
// sends tool_use arguments as input_json_delta fragments, so a tool block is only
// complete once its content_block_stop arrives.
type streamState struct {
	blocks map[int]*blockState
	usage  usageCounts
	stop   string
}

type blockState struct {
	kind     string // text | reasoning | tool_use
	id       string
	name     string
	text     string
	toolArgs string
}

// toolCall is one assembled tool call for the non-streaming fold.
type toolCall struct {
	ID   string
	Name string
	Args string
}

// translator turns upstream events into OpenAI deltas.
//
// textBuf exists for the non-streaming caller: every emitted text delta is also
// accumulated here, so folding the stream into one completion does not have to
// re-read per-block state that has already been discarded.
type translator struct {
	emit     func(delta map[string]any, finish any)
	state    streamState
	textBuf  strings.Builder
	toolCall []toolCall
}

// newTranslator builds a translator that reports deltas through emit. finish is
// sent as the chunk's finish_reason and may be nil on intermediate deltas.
func newTranslator(emit func(map[string]any, any)) *translator {
	return &translator{emit: emit, state: streamState{blocks: map[int]*blockState{}}}
}

// completionFromState folds what the translator observed into one chat.completion
// body, for a caller that did not ask for a stream.
func completionFromState(t *translator, model string) []byte {
	msg := map[string]any{"role": "assistant", "content": t.textBuf.String()}
	if len(t.toolCall) > 0 {
		calls := make([]any, 0, len(t.toolCall))
		for _, c := range t.toolCall {
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
	finish := mapStopReason(t.state.stop)
	if len(t.toolCall) > 0 {
		finish = "tool_calls"
	}
	out := map[string]any{
		"id":     "chatcmpl-minimax",
		"object": "chat.completion",
		"model":  model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": finish,
		}},
		"usage": map[string]any{
			"prompt_tokens":     t.state.usage.InputTokens,
			"completion_tokens": t.state.usage.OutputTokens,
			"total_tokens":      t.state.usage.InputTokens + t.state.usage.OutputTokens,
		},
	}
	b, _ := json.Marshal(out)
	return b
}
