package main

// GET /v1/models: the catalog endpoint, kept apart from the catalog model
// (models.go) and its remote fetchers (catalog_dev.go).

import "net/http"

// handleModels lists the models this account can serve, merging both remote lists
// with the models.dev metadata.
func (s *server) handleModels(w http.ResponseWriter, r *http.Request) {
	if err := s.egressError(); err != nil {
		// A blocked account has no route to the catalog endpoints either; serving
		// the measured fallback table keeps the picker populated without leaking a
		// direct connection.
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": fallbackModels})
		return
	}
	remote, err := s.client.recommendedModels()
	if err != nil {
		// Not fatal: an authenticated /v1/models may still answer.
		s.log("warn", "recommended_models_failed", map[string]any{"error": err.Error()})
		remote = &recommendedModels{}
	}
	var ids []string
	if creds, ok := s.store.get(); ok {
		if list, err := s.client.modelIDs(creds.AccessToken); err == nil {
			ids = list
		} else {
			s.log("warn", "models_list_failed", map[string]any{"error": err.Error()})
		}
	}
	dev := s.modelsDev()
	models := mergeModels(remote, ids, dev)
	if len(models) == 0 {
		// Both remotes unreachable: serve the measured fallback table rather than
		// an empty list, which would hide every model from the UI.
		models = append([]ModelInfo(nil), fallbackModels...)
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": models})
}

// modelsDev fetches the `cline-pass` block of models.dev for context windows and
// image support that neither Cline endpoint reports. A failure yields nil: the
// catalog works without it, just with fewer metadata fields.
func (s *server) modelsDev() map[string]ModelInfo {
	base, err := s.client.client()
	if err != nil {
		return nil
	}
	body, err := fetchModelsDev(base)
	if err != nil {
		s.log("warn", "models_dev_failed", map[string]any{"error": err.Error()})
		return nil
	}
	return parseClineModelsDev(body)
}
