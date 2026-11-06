package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// handleChat converts the OpenAI body, calls the MiniMax upstream, and writes
// either a translated SSE stream or a folded completion.
func (s *server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "cannot read request body")
		return
	}
	creds, ok := s.store.get()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "not_configured",
			"MiniMax 插件尚未登录：请先在设置页完成登录，或调用 /admin/credentials 写入 access_token")
		return
	}
	if err := s.egressError(); err != nil {
		writeError(w, http.StatusForbidden, "egress_missing", err.Error())
		return
	}
	if s.store.expired() && creds.refreshable() {
		if refreshed, err := s.refreshCredentials(r.Context(), creds); err == nil {
			creds = refreshed
		}
	}

	var meta struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(body, &meta)

	anth, err := openAIToAnthropicWith(body, reasoningParams{Effort: reasoningEffortOf(body)})
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "cannot convert body: "+err.Error())
		return
	}
	resp, err := s.client.messages(r.Context(), creds.AccessToken, creds.BaseURL, anth)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "upstream request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		status := resp.StatusCode
		if status == http.StatusPaymentRequired {
			writeError(w, http.StatusTooManyRequests, "quota_exceeded", truncate(string(raw), 512))
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write(openAIErrorBody(status, truncate(string(raw), 512)))
		return
	}

	if meta.Stream {
		s.streamOpenAI(w, resp.Body, meta.Model)
		return
	}
	t := newTranslator(func(map[string]any, any) {})
	if _, err := pump(resp.Body, t); err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "upstream stream error: "+err.Error())
		return
	}
	if t.textBuf.Len() == 0 && len(t.toolCall) == 0 {
		// A 200 with no frames is not an empty answer: it is a broken upstream.
		writeError(w, http.StatusBadGateway, "upstream_error", "upstream returned no content")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(completionFromState(t, meta.Model))
}

// reasoningEffortOf reads the OpenAI reasoning_effort value, which MiniMax maps
// onto output_config.effort.
func reasoningEffortOf(body []byte) string {
	var probe struct {
		ReasoningEffort string `json:"reasoning_effort"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return ""
	}
	return strings.TrimSpace(probe.ReasoningEffort)
}

// streamOpenAI translates the Anthropic SSE stream into OpenAI chat.completion
// chunks.
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

	sentRole := false
	emit := func(delta map[string]any, finish any) {
		if !sentRole {
			sentRole = true
			if delta == nil {
				delta = map[string]any{}
			}
			delta["role"] = "assistant"
		}
		chunk := map[string]any{
			"id":     "chatcmpl-minimax",
			"object": "chat.completion.chunk",
			"model":  model,
			"choices": []any{map[string]any{
				"index":         0,
				"delta":         delta,
				"finish_reason": finish,
			}},
		}
		b, err := json.Marshal(chunk)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}

	t := newTranslator(emit)
	emit(map[string]any{"role": "assistant", "content": ""}, nil)
	stopReason, err := pump(up, t)
	if err != nil {
		// Headers are already sent, so report the failure as an SSE error frame
		// instead of a fake finish + [DONE].
		writeStreamError(w, flusher, "upstream stream error: "+err.Error())
		return
	}
	if t.textBuf.Len() == 0 && len(t.toolCall) == 0 && stopReason == "" {
		writeStreamError(w, flusher, "upstream returned an empty stream")
		return
	}
	emit(map[string]any{}, mapStopReason(firstNonEmpty(t.state.stop, stopReason)))
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func writeStreamError(w io.Writer, flusher http.Flusher, message string) {
	payload, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": message, "type": "upstream_error"},
	})
	fmt.Fprintf(w, "data: %s\n\n", payload)
	flusher.Flush()
}
