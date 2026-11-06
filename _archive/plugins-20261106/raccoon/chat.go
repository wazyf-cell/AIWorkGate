// Package main — the chat forwarder: one request in, an SSE relay or an
// aggregated chat.completion out.
package main

import (
	"encoding/json"
	"io"
	"net/http"
)

// handleChat forwards a chat request and streams the answer.
func (s *server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	// Enforce the upstream's own byte cap before spending a round trip on a 413.
	body, err := io.ReadAll(io.LimitReader(r.Body, requestBodyLimitBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "cannot read request body")
		return
	}
	if len(body) > requestBodyLimitBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "request_too_large",
			"请求体超过 10MB：Raccoon 网关按请求体字节卡上限（上游会回 HTTP_413）")
		return
	}
	creds, ok := s.store.get()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "not_configured",
			"Raccoon 插件尚未登录：请用 POST /admin/credentials 写入 access_token")
		return
	}

	var meta struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &meta)

	wire, err := forceStream(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "cannot read body: "+err.Error())
		return
	}
	wantStream := streamRequested(body)
	if !wantStream {
		// The client wants one chat.completion, so the plugin aggregates the SSE
		// stream. Ask upstream for the usage chunk it is going to aggregate; a
		// streaming client keeps its own stream_options untouched.
		if wire, err = forceUsage(wire); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "cannot read body: "+err.Error())
			return
		}
	}
	proxyURL, allow := s.proxyFor(w, "default")
	if !allow {
		return
	}

	resp, err := s.client.chat(r.Context(), creds.BaseURL, creds, wire, proxyURL)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "upstream request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(openAIErrorBody(resp.StatusCode, truncate(string(raw), 512)))
		return
	}
	if wantStream {
		relaySSE(w, resp.Body)
		return
	}
	out, err := aggregateSSE(resp.Body, meta.Model)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "cannot aggregate upstream stream: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// relaySSE copies the upstream SSE stream frame by frame, flushing each write.
func relaySSE(w http.ResponseWriter, up io.Reader) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming_unsupported", "writer cannot flush")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	buf := make([]byte, 4<<10)
	for {
		n, err := up.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			flusher.Flush()
		}
		if err != nil {
			return
		}
	}
}
