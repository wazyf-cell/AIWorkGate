package main

import (
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// handleAdminCredentials lets the operator hand a credential to the plugin
// directly. Accepts the reference's credential JSON verbatim.
func (s *server) handleAdminCredentials(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST or PUT")
		return
	}
	var in credentials
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	in.ensureIdentity(time.Now())
	if err := s.store.set(in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	creds, _ := s.store.get()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"configured":      true,
		"key_fingerprint": fingerprint(creds.AccessToken),
	})
}

func (s *server) handleReload(w http.ResponseWriter, r *http.Request) {
	egress := 0
	if ef, err := loadEgressFile(s.cfg.EgressFile); err == nil {
		if s.client.egress != nil {
			s.client.egress.set(ef)
		}
		egress = len(ef.Accounts)
	} else {
		writeError(w, http.StatusInternalServerError, "egress_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "rules": 0, "egress": egress})
}
