package main

// Aggregation helpers: turn OpenAI SSE chunks back into one completion, or wrap
// a JSON completion as a single chunk. Ported from chat.go
// (aggregateSSE / singleChunkSSE / pumpSSE / payloadMsg / extractUsage).
//
// The non-streaming relay path needs aggregateSSE: the relay only ever emits
// chunks, so a non-stream request is served by consuming its own stream.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

var finishRe = regexp.MustCompile(`"finish_reason"\s*:\s*"(?:stop|length|tool_calls)"`)

// aggregateSSE folds a chunk sequence into one chat.completion body.
func aggregateSSE(lines []string) map[string]any {
	var content, reasoning []string
	role := "assistant"
	finish := "stop"
	var usage map[string]any
	ident, model := "", ""
	var created any
	toolCalls := map[int]map[string]any{}
	toolOrder := []int{}
	for _, line := range lines {
		line = strings.TrimRight(line, "\r\n")
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := line[6:]
		if payload == "[DONE]" {
			break
		}
		var chunk struct {
			ID      string         `json:"id"`
			Model   string         `json:"model"`
			Created any            `json:"created"`
			Usage   map[string]any `json:"usage"`
			Choices []struct {
				FinishReason any            `json:"finish_reason"`
				Delta        map[string]any `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if ident == "" && chunk.ID != "" {
			ident = chunk.ID
		}
		if model == "" && chunk.Model != "" {
			model = chunk.Model
		}
		if created == nil && chunk.Created != nil {
			created = chunk.Created
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		for _, ch := range chunk.Choices {
			if s := toString(ch.FinishReason); s != "" {
				finish = s
			}
			delta := ch.Delta
			if delta == nil {
				continue
			}
			if r, ok := delta["role"].(string); ok && r != "" {
				role = r
			}
			if c, ok := delta["content"].(string); ok {
				content = append(content, c)
			}
			if rc, ok := delta["reasoning_content"].(string); ok {
				reasoning = append(reasoning, rc)
			}
			if tcs, ok := delta["tool_calls"].([]any); ok {
				for _, tc := range tcs {
					tcm, ok := tc.(map[string]any)
					if !ok {
						continue
					}
					idx := int(toInt64(tcm["index"]))
					merged, ok := toolCalls[idx]
					if !ok {
						merged = map[string]any{"index": idx}
						toolCalls[idx] = merged
						toolOrder = append(toolOrder, idx)
					}
					for _, k := range []string{"id", "type"} {
						if v := toString(tcm[k]); v != "" {
							merged[k] = v
						}
					}
					fn, _ := merged["function"].(map[string]any)
					if fn == nil {
						fn = map[string]any{}
						merged["function"] = fn
					}
					if df, _ := tcm["function"].(map[string]any); df != nil {
						if n := toString(df["name"]); n != "" {
							fn["name"] = n
						}
						if a := toString(df["arguments"]); a != "" {
							fn["arguments"] = toString(fn["arguments"]) + a
						}
					}
				}
			}
		}
	}
	message := map[string]any{"role": role, "content": strings.Join(content, "")}
	if len(reasoning) > 0 {
		message["reasoning_content"] = strings.Join(reasoning, "")
	}
	if len(toolOrder) > 0 {
		sort.Ints(toolOrder)
		tcs := make([]any, 0, len(toolOrder))
		for _, i := range toolOrder {
			tcs = append(tcs, toolCalls[i])
		}
		message["tool_calls"] = tcs
	}
	var createdVal any = time.Now().Unix()
	if created != nil {
		createdVal = created
	}
	if ident == "" {
		ident = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	resp := map[string]any{
		"id":      ident,
		"object":  "chat.completion",
		"created": createdVal,
		"model":   model,
		"choices": []map[string]any{{"index": 0, "message": message, "finish_reason": finish}},
	}
	if usage != nil {
		resp["usage"] = usage
	}
	return resp
}

// singleChunkSSE re-expresses a JSON completion as one SSE chunk + [DONE].
func singleChunkSSE(full map[string]any, out chunkOut) {
	chunk := map[string]any{}
	for _, k := range []string{"id", "created", "model"} {
		if v, ok := full[k]; ok {
			chunk[k] = v
		}
	}
	chunk["object"] = "chat.completion.chunk"
	if choices, _ := full["choices"].([]any); len(choices) > 0 {
		if cm, ok := choices[0].(map[string]any); ok {
			if msg, ok := cm["message"].(map[string]any); ok {
				delta := map[string]any{}
				if v, ok := msg["role"]; ok {
					delta["role"] = v
				}
				if v, ok := msg["content"]; ok {
					delta["content"] = v
				}
				if tcs, ok := msg["tool_calls"]; ok {
					delta["tool_calls"] = tcs
				}
				chunk["choices"] = []map[string]any{{"index": 0, "delta": delta, "finish_reason": "stop"}}
			}
		}
	}
	if _, ok := chunk["choices"]; !ok {
		chunk["choices"] = []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}
	}
	if u, ok := full["usage"]; ok {
		chunk["usage"] = u
	}
	if raw, err := json.Marshal(chunk); err == nil {
		out.writeData(string(raw))
	}
	out.writeDone()
}

// pumpSSE forwards an upstream OpenAI SSE stream as-is, guaranteeing [DONE].
func pumpSSE(resp *http.Response, out *sseWriter) {
	sawDone := false
	finished := false
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 256*1024), 8<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: [DONE]") {
			sawDone = true
			out.writeDone()
			break
		}
		if !finished && strings.Contains(line, `"finish_reason"`) && finishRe.MatchString(line) {
			finished = true
		}
		out.mu.Lock()
		fmt.Fprintf(out.w, "%s\n", line)
		if out.fl != nil {
			out.fl.Flush()
		}
		out.mu.Unlock()
		if finished {
			out.writeDone()
			sawDone = true
			break
		}
	}
	if !sawDone {
		out.writeDone()
	}
}

// convertRelayEvents consumes the relay stream into out and always closes it.
func convertRelayEvents(events *http.Response, runID, session string, body []byte, out chunkOut) {
	defer events.Body.Close()
	cv := newRelayConverter(runID, session, body, out)
	scanner := bufio.NewScanner(events.Body)
	scanner.Buffer(make([]byte, 0, 256*1024), 8<<20)
	var buf []string
	flush := func() {
		if len(buf) > 0 {
			cv.handleFrame(strings.Join(buf, "\n"))
			buf = buf[:0]
		}
	}
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r\n")
		if line == "" {
			flush()
			if cv.done {
				return
			}
			continue
		}
		buf = append(buf, line)
	}
	flush()
	if !cv.done {
		cv.finish()
	}
}
