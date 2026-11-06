package main

import (
	"encoding/json"
	"io"
	"net/http"
)

// handleModels prefers the live catalog and falls back to the manifest's static
// table.
//
// The reference does exactly this: fetchRemoteModels returns [] on any failure
// and the adapter then uses fallbackModels ("失败时返回空数组，由适配器回退兜底表").
func (s *server) handleModels(w http.ResponseWriter, r *http.Request) {
	creds, ok := s.store.get()
	if ok {
		body, err := s.client.models(r.Context(), creds.BaseURL, creds.AccessToken)
		if err == nil {
			if models, perr := parseModelsBody(body); perr == nil && len(models) > 0 {
				writeJSON(w, http.StatusOK, map[string]any{
					"object": "list",
					"data":   models,
					"source": "remote",
				})
				return
			}
			// A 401/100002 means the stored token is dead; surface that instead
			// of silently serving the fallback table as if it were live.
			if !okEnvelope(body) {
				s.respondFallbackModels(w, "stored token was rejected by the catalog endpoint")
				return
			}
		}
		s.respondFallbackModels(w, err.Error())
		return
	}
	s.respondFallbackModels(w, "not configured")
}

// okEnvelope reports whether a business envelope is a success.
func okEnvelope(body []byte) bool {
	ok, _, _, _ := parseEnvelope(body)
	return ok
}

// respondFallbackModels serves the manifest's static catalog, labelled so a
// caller can tell it apart from a live one.
func (s *server) respondFallbackModels(w http.ResponseWriter, reason string) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   fallbackModels(),
		"source": "fallback",
		"note":   "远端模型目录不可用，回退到清单静态表：" + reason,
	})
}

// handleChat forwards a chat request and streams the answer.
//
// Loomy is OpenAI-native, so the upstream body is the host's body with
// "stream": true forced (the reference always streams). A client that asked for
// a non-streaming answer gets the frames aggregated into one completion.
func (s *server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "cannot read request body")
		return
	}
	creds, ok := s.store.get()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "not_configured",
			"Loomy 插件尚未登录：请用 POST /admin/credentials 写入 access_token")
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

	resp, err := s.client.chat(r.Context(), creds.BaseURL, creds.AccessToken, wire)
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

// relaySSE copies the upstream SSE stream to the client frame by frame, flushing
// after each write. Loomy's frames are already chat.completion.chunk, so no
// rewriting happens: a buffer here would add visible latency to every token.
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
