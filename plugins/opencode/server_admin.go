package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// handleReload lives in egress.go: the host calls it after rewriting
// runtime\opencode\egress.json, and it must re-read that file rather than just
// drop a cache. Dropping the capability table stays here so the next model
// listing refetches it, which is what the previous handler did.

// handleAdminCredentials lets the operator hand a key to the plugin directly.
func (s *server) handleAdminCredentials(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST or PUT")
		return
	}
	var in struct {
		APIKey    string `json:"api_key"`
		Label     string `json:"label"`
		UserAgent string `json:"user_agent"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	key := strings.TrimSpace(in.APIKey)
	if key == "" {
		key = anonymousKey
	}
	if err := s.store.set(credentials{
		APIKey: key, Label: in.Label, UserAgent: in.UserAgent,
	}); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	creds := s.store.current()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"configured":      s.store.configured(),
		"anonymous":       creds.anonymous(),
		"key_fingerprint": fingerprint(creds.APIKey),
		"project":         creds.projectIDFor(),
	})
}
