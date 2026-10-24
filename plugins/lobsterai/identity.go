package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// identityOf is the identity block of GET /accounts.
//
// LobsterAI sends an identity payload on every models call: uuid (created at
// login) plus first/latestKeyfrom. uuid is the device-ish identifier the server
// can correlate, so it is the one exposed here. The keyfrom timestamps are
// frozen at login on purpose (the reference never updates latestKeyfrom on
// refresh), so they are not rotated by an identity reset.
func (s *server) identityOf(c credentials) map[string]any {
	if strings.TrimSpace(c.UUID) == "" {
		return nil
	}
	return map[string]any{
		"device_id":  c.UUID,
		"user_agent": userAgent,
		"source":     "derived",
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
		s.handleRefresh(w, r, accountID)
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

func (s *server) handleIdentityDetails(w http.ResponseWriter, r *http.Request, accountID string) {
	creds, ok := s.store.get()
	if !ok || accountID != "default" {
		writeError(w, http.StatusNotFound, "account_not_found", "account "+accountID+" not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "identity": s.identityOf(creds)})
}

// handleIdentityReset rotates the stored uuid. mode "os" is refused: this plugin
// has no OS-level identifier to switch.
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
			"mode "+mode+" is not supported by the LobsterAI plugin")
		return
	}
	creds, ok := s.store.get()
	if !ok || accountID != "default" {
		writeError(w, http.StatusNotFound, "account_not_found", "account "+accountID+" not found")
		return
	}
	old := creds.UUID
	creds.UUID = newUUIDHex()
	creds.UpdatedAt = timeNowUnix()
	if err := s.store.set(creds); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"mode":     mode,
		"rotated":  old != creds.UUID,
		"identity": s.identityOf(creds),
	})
}

// handleRefresh renews the access token through /api/auth/refresh.
//
// Ported from refreshCredential: POST the keyfrom body plus refreshToken to the
// api origin with the anonymous headers. The endpoint and body shape are the
// reference's; it could not be exercised here because no LobsterAI account was
// available (see docs/evidence-lobsterai.md).
func (s *server) handleRefresh(w http.ResponseWriter, r *http.Request, accountID string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	creds, ok := s.store.get()
	if !ok || accountID != "default" {
		writeError(w, http.StatusNotFound, "account_not_found", "account "+accountID+" not found")
		return
	}
	if strings.TrimSpace(creds.RefreshToken) == "" {
		writeError(w, http.StatusBadRequest, "not_refreshable", "没有 refresh_token：请重新登录")
		return
	}
	proxy, err := s.proxyFor("default")
	if err != nil {
		writeEgressError(w, err)
		return
	}
	version := s.client.clientVersion(r.Context(), proxy)
	next, err := s.client.refreshAuth(r.Context(), creds.BaseURL, creds, version, proxy)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "refresh failed: "+err.Error())
		return
	}
	// Merge onto the stored keyfrom tuple; those timestamps stay frozen.
	creds.AccessToken = next.AccessToken
	if next.RefreshToken != "" {
		creds.RefreshToken = next.RefreshToken
	}
	creds.UpdatedAt = timeNowUnix()
	if err := s.store.set(creds); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"result": "ok", "key_fingerprint": fingerprint(creds.AccessToken),
	})
}

func writeEgressError(w http.ResponseWriter, err error) {
	if em, ok := err.(*egressMissingError); ok {
		writeError(w, http.StatusForbidden, "egress_missing", em.Error())
		return
	}
	writeError(w, http.StatusInternalServerError, "egress_error", err.Error())
}
