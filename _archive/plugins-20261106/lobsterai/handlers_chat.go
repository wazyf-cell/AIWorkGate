package main

import (
	"encoding/json"
	"io"
	"net/http"
)

// handleModels prefers the live catalog, falling back to the manifest table.
// The reference returns [] on failure so the adapter can use its fallback
// directory ("失败或未登录时返回空数组（调用方回退到产品兜底目录）").
func (s *server) handleModels(w http.ResponseWriter, r *http.Request) {
	creds, ok := s.store.get()
	if ok {
		proxy, perr := s.proxyFor("default")
		if perr != nil {
			writeEgressError(w, perr)
			return
		}
		version := s.client.clientVersion(r.Context(), proxy)
		body, err := s.client.models(r.Context(), creds.BaseURL, creds.AccessToken, version, creds, proxy)
		if err == nil {
			if models, perr := parseModelsBody(body); perr == nil && len(models) > 0 {
				writeJSON(w, http.StatusOK, map[string]any{
					"object":         "list",
					"data":           models,
					"source":         "remote",
					"client_version": version,
				})
				return
			}
			s.respondFallbackModels(w, "catalog response had no usable entries")
			return
		}
		s.respondFallbackModels(w, err.Error())
		return
	}
	s.respondFallbackModels(w, "not configured")
}

func (s *server) respondFallbackModels(w http.ResponseWriter, reason string) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   fallbackModels(),
		"source": "fallback",
		"note":   "远端模型目录不可用，回退到清单静态表：" + reason,
	})
}

// handleChat forwards a chat request and streams the answer.
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
			"LobsterAI 插件尚未登录：请用 /admin/login 取登录链接，完成后 POST /admin/exchange")
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
	proxy, perr := s.proxyFor("default")
	if perr != nil {
		writeEgressError(w, perr)
		return
	}
	version := s.client.clientVersion(r.Context(), proxy)

	resp, err := s.client.chat(r.Context(), creds.BaseURL, creds.AccessToken, version, wire, proxy)
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
