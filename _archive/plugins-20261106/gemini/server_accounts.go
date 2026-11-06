package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// handleAccounts reports exactly one account while a token is stored. Quota comes
// from the live quota endpoint when it answers; otherwise it stays empty.
func (s *server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	creds, ok := s.store.get()
	accounts := []map[string]any{}
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"accounts": accounts})
		return
	}
	cred := map[string]any{
		"present":     true,
		"refreshable": creds.refreshable(),
	}
	if exp, known := creds.expiresAtMs(); known {
		cred["expires_at"] = exp / 1000
	}
	status := "ok"
	if creds.nearExpiry(time.Now()) {
		status = "expired"
	}
	acct := map[string]any{
		"id":         defaultAccountID,
		"label":      creds.label(),
		"enabled":    true,
		"status":     status,
		"credential": cred,
		"quota":      s.quotaSnapshot(r.Context(), creds),
		// Identity is always reported: even the protocol's pinned machine tag is
		// useful evidence when an account is flagged.
		"identity": creds.identityView(),
	}
	if creds.CloudaicompanionProject != "" {
		acct["project"] = creds.CloudaicompanionProject
	}
	accounts = append(accounts, acct)
	writeJSON(w, http.StatusOK, map[string]any{"accounts": accounts})
}

// handleAccountRoot serves /accounts/{id}: the host's delete and enable/disable.
// A single-account channel can be signed out (DELETE clears the credential) but
// has nothing to disable, so PATCH answers 501 instead of pretending.
func (s *server) handleAccountRoot(w http.ResponseWriter, r *http.Request, id string) {
	if id != defaultAccountID {
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

// handleAccountSub serves POST /accounts/{id}/identity/reset.
//
// mode "request" rotates the per-request identifiers (device tag + the session
// generation mixed into request.sessionId). mode "os" is refused: the five
// fingerprint headers are protocol constants and changing them would make the
// upstream reject the client, so claiming an OS switch would be a lie.
func (s *server) handleAccountSub(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/accounts/"), "/")
	parts := strings.Split(rest, "/")
	if len(parts) == 1 && parts[0] != "" {
		s.handleAccountRoot(w, r, parts[0])
		return
	}
	if len(parts) != 3 || parts[1] != "identity" || parts[2] != "reset" {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint: "+r.Method+" "+r.URL.Path)
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	if id := strings.TrimSpace(parts[0]); id != defaultAccountID {
		writeError(w, http.StatusNotFound, "account_not_found", "account "+id+" is not stored")
		return
	}
	var in struct {
		Mode string `json:"mode"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in)
	mode := strings.ToLower(strings.TrimSpace(in.Mode))
	if mode == "" {
		mode = "request"
	}
	if mode != "request" {
		writeError(w, http.StatusBadRequest, "unsupported_identity_mode",
			"mode "+mode+" is not supported: this channel's device headers are protocol constants; only the per-request ids can be rotated (mode=request)")
		return
	}
	creds, ok := s.store.get()
	if !ok {
		writeError(w, http.StatusNotFound, "account_not_found", "no credential stored")
		return
	}
	creds.ensureIdentity(time.Now())
	old := *creds.Identity
	creds.Identity = &identityState{
		DeviceID:  randomHex(8),
		SessionID: randomHex(8),
		UpdatedAt: time.Now().Unix(),
	}
	if err := s.store.set(creds); err != nil {
		writeError(w, http.StatusInternalServerError, "persist_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"mode": mode,
		"identity": map[string]any{
			"device_id":  creds.Identity.DeviceID,
			"session_id": creds.Identity.SessionID,
			"updated_at": creds.Identity.UpdatedAt,
			"source":     "derived",
		},
		"rotated_from": map[string]any{"device_id": old.DeviceID, "session_id": old.SessionID},
	})
}

func (s *server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	creds, ok := s.store.get()
	if !ok {
		writeError(w, http.StatusNotFound, "not_configured", "no credential stored")
		return
	}
	next, err := s.refreshCredentials(r.Context(), creds)
	if err != nil {
		var ce *oauthConfigError
		if errors.As(err, &ce) {
			writeError(w, http.StatusNotImplemented, "client_secret_required", ce.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"result": "fail", "message": err.Error()})
		return
	}
	exp, _ := next.expiresAtMs()
	writeJSON(w, http.StatusOK, map[string]any{"result": "ok", "expires_at": exp / 1000})
}

// refreshCredentials exchanges the refresh_token and persists the result.
//
// A rotated refresh_token replaces the stored one: Google treats a refresh as a
// replay and can revoke the whole grant if the old value is used again.
func (s *server) refreshCredentials(ctx context.Context, creds credentials) (credentials, error) {
	if !creds.refreshable() {
		return creds, fmt.Errorf("gemini: credential has no refresh_token; authorize again")
	}
	grant, err := s.client.refresh(ctx, defaultAccountID, creds.RefreshToken)
	if err != nil {
		return creds, err
	}
	next := grantToCredentials(grant, creds)
	next.ensureIdentity(time.Now())
	if err := s.store.set(next); err != nil {
		return creds, err
	}
	return next, nil
}
