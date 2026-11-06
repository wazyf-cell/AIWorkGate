package webui

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// promptAnalyzeMaxBytes matches the gateway's inbound body allowance instead of
// the admin API's 1 MiB: the point of the tool is pasting a real request, and
// real agent requests carry tool schemas and history past 1 MiB.
const promptAnalyzeMaxBytes = 8 << 20

// handlePromptAnalyze estimates the six-section token split (D-55) of a pasted
// request body. The body is sent as `{"model": "...", "body": {...}}` so the
// model can be chosen independently of the request's own "model" field.
func (s *server) handlePromptAnalyze(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) {
		return
	}
	if s.opts.PromptAnalyze == nil {
		s.unavailable(w)
		return
	}
	var request struct {
		Model string          `json:"model"`
		Body  json.RawMessage `json:"body"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, promptAnalyzeMaxBytes)
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		var tooLarge *http.MaxBytesError
		switch {
		case errors.As(err, &tooLarge):
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large (limit 8 MiB)", "payload_too_large")
		case errors.Is(err, io.EOF):
			writeError(w, http.StatusBadRequest, "request body is required", "bad_request")
		default:
			writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error(), "bad_request")
		}
		return
	}
	if body := strings.TrimSpace(string(request.Body)); body == "" || body == "null" {
		writeError(w, http.StatusBadRequest, "body is required", "bad_request")
		return
	}
	payload, err := s.opts.PromptAnalyze(strings.TrimSpace(request.Model), request.Body)
	if err != nil {
		writeSchedulerError(w, "analyze prompt", err)
		return
	}
	if payload == nil {
		payload = map[string]any{}
	}
	writeJSON(w, http.StatusOK, payload)
}
