package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// handleChat converts the OpenAI body, calls the route the stored credential
// selects, and writes either a converted completion or a translated SSE stream.
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
			"ZCode 插件尚未登录：请用 local_state 读取本机客户端登录态，或调用 /admin/credentials 写入凭据")
		return
	}
	proxy, err := s.proxyFor("default")
	if err != nil {
		var em *egressMissingError
		if errors.As(err, &em) {
			writeError(w, http.StatusForbidden, "egress_missing", em.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "egress_error", err.Error())
		return
	}

	var meta struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(body, &meta)

	anth, err := openAIToAnthropic(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "cannot convert body: "+err.Error())
		return
	}
	var resp *http.Response
	if creds.kind() == credentialKindAPIKey {
		resp, err = s.client.apiKeyMessages(r.Context(), creds.origin(), creds.secret(), anth, r.Header, proxy)
	} else {
		resp, err = s.client.messagesVia(r.Context(), creds.origin(), creds.JWT, creds.DeviceMid, anth, r.Header, proxy)
	}
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

	if meta.Stream {
		s.streamOpenAI(w, resp.Body, meta.Model)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "cannot read upstream body")
		return
	}
	out, err := anthropicToOpenAI(raw, meta.Model)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "cannot convert response: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}
