package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// handleChat shapes the body for the free-tier gate, calls the upstream, and
// either relays the SSE stream or folds it into one chat.completion.
//
// The shape rewrite is not optional: measured against the live endpoint on
// 2026-10-21, a body without `stream:true` AND the official read/shell tool
// definitions is rejected with HTTP 403 FreeTierError regardless of the key or
// UA. See protocol.go BuildPayload.
func (s *server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	// Fail closed before touching the body: under strict mode, or with a blocked
	// binding, this account has no permitted route at all.
	if err := s.egressError(); err != nil {
		writeError(w, http.StatusServiceUnavailable, "egress_missing", err.Error())
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "cannot read request body")
		return
	}
	var meta struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(body, &meta)

	creds := s.store.current()
	caps := s.caps.Get()

	// Refuse an image request the model cannot accept rather than letting the
	// upstream drop it: a user who attached a picture must learn the model did
	// not see it.
	if err := checkImageSupport(meta.Model, body, caps); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "unsupported_content", err.Error())
		return
	}

	shaped, aliases, err := BuildPayload(body, caps)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "cannot convert body: "+err.Error())
		return
	}
	post := func(payload []byte) (*http.Response, error) {
		return s.client.chat(r.Context(), creds.projectIDFor(), sessionID(),
			creds.effectiveUserAgent(), creds.effectiveKey(), payload)
	}
	resp, err := post(shaped)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "upstream request failed: "+err.Error())
		return
	}
	// Some upstream routes decode the body strictly and reject the top-level
	// "system" field the gate note lives in. Retry once with the note moved into
	// the message list; the request is otherwise unchanged.
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnprocessableEntity {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		folded, ok := FoldSystemIntoMessages(shaped)
		if !ok || !unknownSystemField(string(raw)) {
			s.rejectUpstream(w, resp.StatusCode, string(raw))
			return
		}
		resp, err = post(folded)
		if err != nil {
			writeError(w, http.StatusBadGateway, "upstream_error", "upstream request failed: "+err.Error())
			return
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		s.rejectUpstream(w, resp.StatusCode, string(raw))
		return
	}

	if meta.Stream {
		s.relayStream(w, resp.Body, aliases)
		return
	}
	completion, err := Consume(resp.Body, aliases)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "cannot read upstream stream: "+err.Error())
		return
	}
	out, err := completion.JSON(meta.Model)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "cannot encode completion: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// rejectUpstream maps an upstream error onto the plugin contract's envelope.
func (s *server) rejectUpstream(w http.ResponseWriter, upstreamStatus int, raw string) {
	status, kind, message := classifyUpstreamError(upstreamStatus, raw)
	s.log("warn", "upstream_rejected", map[string]any{
		"status": upstreamStatus, "kind": kind, "detail": truncate(message, 256),
	})
	if kind == "free_tier" || kind == "free_usage_limit" || kind == "rate_limit" {
		// A cooldown hint so the host can park the account without parsing us.
		w.Header().Set("Retry-After", "60")
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(errorBody(kind, message))
}

// relayStream forwards the upstream SSE, restoring aliased tool names as frames
// pass through. The gate forces streaming upstream, so this is the normal path
// for a caller that asked for stream:true.
func (s *server) relayStream(w http.ResponseWriter, src io.Reader, aliases map[string]string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming_unsupported", "writer cannot flush")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	reverse := map[string]string{}
	for alias, original := range aliases {
		reverse[alias] = original
	}
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if !strings.HasPrefix(line, "data:") {
			if strings.TrimSpace(line) != "" {
				fmt.Fprint(w, line+"\n")
				flusher.Flush()
			}
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			fmt.Fprint(w, "data: [DONE]\n\n")
			flusher.Flush()
			continue
		}
		if len(reverse) > 0 {
			data = restoreAliases(data, reverse)
		}
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}
	if err := scanner.Err(); err != nil {
		s.log("warn", "upstream_stream_aborted", map[string]any{"error": err.Error()})
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// restoreAliases rewrites aliased tool names back to the caller's names inside
// one SSE data frame. A frame that is not the expected shape is returned as-is.
func restoreAliases(data string, reverse map[string]string) string {
	var frame map[string]any
	if json.Unmarshal([]byte(data), &frame) != nil {
		return data
	}
	changed := false
	choices, _ := frame["choices"].([]any)
	for _, c := range choices {
		choice, _ := c.(map[string]any)
		if choice == nil {
			continue
		}
		for _, key := range []string{"delta", "message"} {
			holder, _ := choice[key].(map[string]any)
			if holder == nil {
				continue
			}
			calls, _ := holder["tool_calls"].([]any)
			for _, call := range calls {
				entry, _ := call.(map[string]any)
				fn, _ := entry["function"].(map[string]any)
				name, _ := fn["name"].(string)
				if alias, ok := reverse[name]; ok {
					fn["name"] = alias
					changed = true
				}
			}
		}
	}
	if !changed {
		return data
	}
	out, err := json.Marshal(frame)
	if err != nil {
		return data
	}
	return string(out)
}
