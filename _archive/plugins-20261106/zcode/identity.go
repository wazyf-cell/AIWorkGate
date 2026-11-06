package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// accountViewIdentity is the identity block of GET /accounts.
//
// ZCode has exactly one real device identifier: X-Device-Mid, a per-account
// uuid4 the upstream fingerprints ("a fresh install on this machine" per
// account). The trace trio is regenerated per request, and x-query-id /
// x-session-id are deliberately NOT sent, so there is no session id to report;
// leaving those fields empty is the honest answer.
func (s *server) identityOf(c credentials) map[string]any {
	if strings.TrimSpace(c.DeviceMid) == "" {
		return nil
	}
	return map[string]any{
		"device_id":  c.DeviceMid,
		"user_agent": "ZCode/" + appVersion,
		"source":     "random",
		"updated_at": c.UpdatedAt,
	}
}

// handleAccountSub routes /accounts/{id}/... endpoints.
func (s *server) handleAccountSub(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/accounts/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 1 && parts[0] != "" {
		s.handleAccountRoot(w, r, parts[0])
		return
	}
	if len(parts) < 2 {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint: "+r.Method+" "+r.URL.Path)
		return
	}
	accountID := parts[0]
	switch parts[1] {
	case "identity":
		if len(parts) == 3 && parts[2] == "reset" {
			s.handleIdentityReset(w, r, accountID)
			return
		}
		s.handleIdentityDetails(w, r, accountID)
	case "refresh":
		writeError(w, http.StatusNotImplemented, "not_implemented",
			"ZCode 凭据不能在不重新登录的情况下续期")
	default:
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint: "+r.Method+" "+r.URL.Path)
	}
}

// handleAccountRoot serves /accounts/{id}: the host's delete and enable/disable.
// A single-account channel can be signed out (DELETE clears the credential) but
// has nothing to disable, so PATCH answers 501 instead of pretending.
func (s *server) handleAccountRoot(w http.ResponseWriter, r *http.Request, id string) {
	if id != "default" {
		writeError(w, http.StatusNotFound, "account_not_found", "account "+id+" is not stored")
		return
	}
	switch r.Method {
	case http.MethodDelete:
		if !s.store.configured() {
			writeError(w, http.StatusNotFound, "account_not_found", "no credential stored")
			return
		}
		if err := s.store.clear(); err != nil {
			writeError(w, http.StatusInternalServerError, "persist_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case http.MethodPatch:
		writeError(w, http.StatusNotImplemented, "not_supported",
			"this channel has a single account and cannot be disabled; delete it to sign out")
	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use DELETE or PATCH")
	}
}

// handleIdentityDetails reports the device identity without rotating it.
func (s *server) handleIdentityDetails(w http.ResponseWriter, r *http.Request, accountID string) {
	creds, ok := s.store.get()
	if !ok || accountID != "default" {
		writeError(w, http.StatusNotFound, "account_not_found", "account "+accountID+" not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "identity": s.identityOf(creds)})
}

// handleIdentityReset really rotates the stored X-Device-Mid. mode "os" is
// refused because this plugin never touches OS-level identifiers; declaring it
// would be a promise with nothing behind it.
func (s *server) handleIdentityReset(w http.ResponseWriter, r *http.Request, accountID string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	var in struct {
		Mode string `json:"mode"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &in); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
			return
		}
	}
	mode := strings.ToLower(strings.TrimSpace(in.Mode))
	if mode == "" {
		mode = "request"
	}
	if mode != "request" {
		writeError(w, http.StatusBadRequest, "unsupported_identity_mode",
			"mode "+mode+" is not supported by the ZCode plugin")
		return
	}
	creds, ok := s.store.get()
	if !ok || accountID != "default" {
		writeError(w, http.StatusNotFound, "account_not_found", "account "+accountID+" not found")
		return
	}
	old := creds.DeviceMid
	creds.DeviceMid = newUUID()
	creds.UpdatedAt = s.clock().Unix()
	if err := s.store.set(creds); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"mode":     mode,
		"rotated":  old != creds.DeviceMid,
		"identity": s.identityOf(creds),
	})
}

// now is injectable so tests do not depend on the wall clock.
func (s *server) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}
