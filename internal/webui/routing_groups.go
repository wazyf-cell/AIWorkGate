package webui

import (
	"encoding/json"
	"net/http"
)

// handleRoutingGroups reads and replaces the config-declared P11 pattern
// groups. The host validates the list (the config normalizer would silently
// drop bad entries) and returns the resolved view on both verbs.
func (s *server) handleRoutingGroups(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet, http.MethodPut) {
		return
	}
	if r.Method == http.MethodGet {
		if s.opts.RoutingGroups == nil {
			s.unavailable(w)
			return
		}
		payload, err := s.opts.RoutingGroups()
		if err != nil {
			writeSchedulerError(w, "read routing groups", err)
			return
		}
		writeJSON(w, http.StatusOK, payload)
		return
	}
	if s.opts.SaveRoutingGroups == nil {
		s.unavailable(w)
		return
	}
	var body struct {
		Groups json.RawMessage `json:"groups"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if groups := string(body.Groups); groups == "" || groups == "null" {
		writeError(w, http.StatusBadRequest, "groups is required", "bad_request")
		return
	}
	payload, err := s.opts.SaveRoutingGroups(body.Groups)
	if err != nil {
		writeSchedulerError(w, "save routing groups", err)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}
