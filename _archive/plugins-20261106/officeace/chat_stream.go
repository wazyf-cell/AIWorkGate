package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// relaySSE forwards upstream SSE bytes verbatim and flushes immediately. An
// upstream that answers 2xx but sends nothing is reported as an error instead
// of a 200 with an empty body.
func (p *Plugin) relaySSE(w http.ResponseWriter, r *http.Request, resp *http.Response) {
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(strings.ToLower(ct), "text/event-stream") {
		// Upstream ignored stream:true and returned JSON; wrap it as one SSE event.
		data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if err != nil {
			writeError(w, http.StatusBadGateway, "upstream_error", "reading upstream body: "+err.Error())
			return
		}
		if len(bytes.TrimSpace(data)) == 0 {
			writeError(w, http.StatusBadGateway, "upstream_error", "upstream returned an empty body")
			return
		}
		setSSEHeaders(w)
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", strings.TrimSpace(string(data)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flushWriter(w)
		return
	}
	// Peek before committing headers: once 200 is written the host books the
	// attempt as successful, so an empty stream must fail first.
	reader := bufio.NewReaderSize(resp.Body, 32*1024)
	if _, err := reader.Peek(1); err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "upstream returned an empty SSE stream")
		return
	}
	setSSEHeaders(w)
	w.WriteHeader(http.StatusOK)
	buf := make([]byte, 32*1024)
	for {
		if r.Context().Err() != nil {
			return
		}
		n, err := reader.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			flushWriter(w)
		}
		if err != nil {
			return
		}
	}
}

type aggregatedToolCall struct {
	ID        string
	Type      string
	Name      string
	Arguments strings.Builder
}

// aggregatedChoice accumulates one upstream choices[].index so n > 1 keeps
// every candidate separate instead of merging texts into one answer.
type aggregatedChoice struct {
	content   strings.Builder
	reasoning strings.Builder
	toolCalls map[int]*aggregatedToolCall
	finish    string
}

func newAggregatedChoice() *aggregatedChoice {
	return &aggregatedChoice{toolCalls: map[int]*aggregatedToolCall{}}
}

// hasContent reports whether upstream actually produced an answer for this
// choice (a finish_reason alone counts: an empty-but-finished reply is real).
func (c *aggregatedChoice) hasContent() bool {
	return c.content.Len() > 0 || c.reasoning.Len() > 0 || len(c.toolCalls) > 0 || c.finish != ""
}

func (c *aggregatedChoice) message() (map[string]any, string) {
	msg := map[string]any{"role": "assistant", "content": nil}
	if c.content.Len() > 0 {
		msg["content"] = c.content.String()
	}
	if c.reasoning.Len() > 0 {
		msg["reasoning_content"] = c.reasoning.String()
	}
	finish := c.finish
	if len(c.toolCalls) > 0 {
		indexes := make([]int, 0, len(c.toolCalls))
		for idx := range c.toolCalls {
			indexes = append(indexes, idx)
		}
		sort.Ints(indexes)
		calls := make([]map[string]any, 0, len(indexes))
		for _, idx := range indexes {
			tc := c.toolCalls[idx]
			typ := tc.Type
			if typ == "" {
				typ = "function"
			}
			calls = append(calls, map[string]any{
				"id":   tc.ID,
				"type": typ,
				"function": map[string]any{
					"name":      tc.Name,
					"arguments": tc.Arguments.String(),
				},
			})
		}
		msg["tool_calls"] = calls
		if finish == "" {
			finish = "tool_calls"
		}
	}
	if finish == "" {
		finish = "stop"
	}
	return msg, finish
}

type rawToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func (c *aggregatedChoice) mergeToolCalls(calls []rawToolCall) {
	for _, tc := range calls {
		agg := c.toolCalls[tc.Index]
		if agg == nil {
			agg = &aggregatedToolCall{}
			c.toolCalls[tc.Index] = agg
		}
		if tc.ID != "" {
			agg.ID = tc.ID
		}
		if tc.Type != "" {
			agg.Type = tc.Type
		}
		if tc.Function.Name != "" {
			agg.Name += tc.Function.Name
		}
		agg.Arguments.WriteString(tc.Function.Arguments)
	}
}

type upstreamChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string        `json:"role"`
			Content          string        `json:"content"`
			ReasoningContent string        `json:"reasoning_content"`
			ToolCalls        []rawToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage map[string]any `json:"usage"`
}

// aggregateChat turns the upstream SSE stream into one chat.completion object.
// An upstream stream with no parsed chunk (or no completion content) is an
// error: the host must not see a fabricated 200 completion.
func (p *Plugin) aggregateChat(w http.ResponseWriter, model string, resp *http.Response) {
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(ct, "application/json") {
		data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		if err != nil {
			writeError(w, http.StatusBadGateway, "upstream_error", "reading upstream body: "+err.Error())
			return
		}
		var probe map[string]any
		if json.Unmarshal(data, &probe) == nil {
			if choices, ok := probe["choices"].([]any); ok && len(choices) > 0 {
				writeJSON(w, http.StatusOK, probe)
				return
			}
			writeError(w, http.StatusBadGateway, "upstream_error", "upstream returned a JSON body without choices")
			return
		}
		writeError(w, http.StatusBadGateway, "upstream_error", "invalid upstream JSON")
		return
	}

	id := ""
	choices := map[int]*aggregatedChoice{}
	usage := map[string]any(nil)
	sawChunk := false
	reader := bufio.NewReaderSize(resp.Body, 64*1024)
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "data:") {
				payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
				if payload == "[DONE]" {
					break
				}
				var chunk upstreamChunk
				if json.Unmarshal([]byte(payload), &chunk) == nil {
					sawChunk = true
					if chunk.ID != "" {
						id = chunk.ID
					}
					if chunk.Usage != nil {
						usage = chunk.Usage
					}
					for _, ch := range chunk.Choices {
						agg := choices[ch.Index]
						if agg == nil {
							agg = newAggregatedChoice()
							choices[ch.Index] = agg
						}
						agg.content.WriteString(ch.Delta.Content)
						agg.reasoning.WriteString(ch.Delta.ReasoningContent)
						agg.mergeToolCalls(ch.Delta.ToolCalls)
						if ch.FinishReason != nil && *ch.FinishReason != "" {
							agg.finish = *ch.FinishReason
						}
					}
				}
			}
		}
		if err != nil {
			break
		}
	}

	if !sawChunk {
		writeError(w, http.StatusBadGateway, "upstream_error", "upstream returned an empty SSE stream")
		return
	}
	if len(choices) == 0 {
		writeError(w, http.StatusBadGateway, "upstream_error", "upstream returned no completion choices")
		return
	}
	indexes := make([]int, 0, len(choices))
	for idx, agg := range choices {
		if agg.hasContent() {
			indexes = append(indexes, idx)
		}
	}
	if len(indexes) == 0 {
		writeError(w, http.StatusBadGateway, "upstream_error", "upstream returned no completion content")
		return
	}
	sort.Ints(indexes)
	wireChoices := make([]map[string]any, 0, len(indexes))
	for _, idx := range indexes {
		msg, finish := choices[idx].message()
		wireChoices = append(wireChoices, map[string]any{
			"index":         idx,
			"message":       msg,
			"finish_reason": finish,
		})
	}
	if id == "" {
		id = "chatcmpl-officeace-" + randomHex(6)
	}
	out := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": p.now().Unix(),
		"model":   model,
		"choices": wireChoices,
	}
	if usage != nil {
		out["usage"] = usage
	}
	writeJSON(w, http.StatusOK, out)
}
