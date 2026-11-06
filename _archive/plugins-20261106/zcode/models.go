package main

import (
	"encoding/json"
	"errors"
	"net/http"
)

// handleModels prefers the live catalog for the api-key route and falls back to
// the static table when the credential is a desktop JWT (the zcode-plan channel
// lists models only through the captcha-gated console) or when the call fails.
// The fallback is labelled as such; it is never presented as a live answer.
func (s *server) handleModels(w http.ResponseWriter, r *http.Request) {
	creds, ok := s.store.get()
	if ok && creds.kind() == credentialKindAPIKey {
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
		if body, ferr := s.client.fetchModels(r.Context(), creds.origin(), creds.secret(), proxy); ferr == nil {
			if models, perr := parseAnthropicModels(body); perr == nil && len(models) > 0 {
				writeJSON(w, http.StatusOK, map[string]any{
					"object": "list", "data": models, "source": "remote",
				})
				return
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list", "data": manifestModels(), "source": "fallback",
	})
}

// parseAnthropicModels reads /api/anthropic/v1/models: {data:[{id,display_name}]}.
func parseAnthropicModels(body []byte) ([]map[string]any, error) {
	var list struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(list.Data))
	for _, m := range list.Data {
		if m.ID == "" {
			continue
		}
		name := m.DisplayName
		if name == "" {
			name = m.ID
		}
		out = append(out, map[string]any{"id": m.ID, "object": "model", "owned_by": "zcode", "name": name})
	}
	return out, nil
}
