package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// handleAdminCredentials lets the operator hand a token to the plugin directly.
func (s *server) handleAdminCredentials(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST or PUT")
		return
	}
	var in struct {
		AccessToken   string `json:"access_token"`
		RefreshToken  string `json:"refresh_token"`
		FirstKeyfrom  string `json:"first_keyfrom"`
		LatestKeyfrom string `json:"latest_keyfrom"`
		UUID          string `json:"uuid"`
		UserID        string `json:"user_id"`
		YID           string `json:"yid"`
		AccountUserID string `json:"account_user_id"`
		Nickname      string `json:"nickname"`
		Label         string `json:"label"`
		BaseURL       string `json:"base_url"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if err := s.store.set(credentials{
		AccessToken: in.AccessToken, RefreshToken: in.RefreshToken,
		FirstKeyfrom: in.FirstKeyfrom, LatestKeyfrom: in.LatestKeyfrom,
		UUID: in.UUID, UserID: in.UserID, YID: in.YID,
		AccountUserID: in.AccountUserID, Nickname: in.Nickname,
		Label: in.Label, BaseURL: in.BaseURL, UpdatedAt: timeNowUnix(),
	}); err != nil {
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

// handleAdminExchange completes the login from a portal auth code.
//
// This is the one channel of the three whose login needs no secret: the flow is
// "open the portal login page in a browser -> it redirects to
// http://127.0.0.1:<port>/auth/callback?code=... -> POST /api/auth/exchange".
// The plugin cannot host that browser, so the operator completes it and posts the
// code here; the exchange itself is the reference's exact body
// (authCode + firstKeyfrom + latestKeyfrom + uuid + version).
func (s *server) handleAdminExchange(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	var in struct {
		Code string `json:"code"`
		// Optional: reuse a previously issued login URL's session.
		UUID         string `json:"uuid"`
		FirstKeyfrom string `json:"first_keyfrom"`
		BaseURL      string `json:"base_url"`
		Label        string `json:"label"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	code := strings.TrimSpace(in.Code)
	if code == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "code is required")
		return
	}
	session := loginSession{
		UUID:          firstNonEmpty(in.UUID, newUUIDHex()),
		FirstKeyfrom:  firstNonEmpty(in.FirstKeyfrom, strconvNow()),
		LatestKeyfrom: strconvNow(),
	}
	creds, err := s.doExchange(r.Context(), in.BaseURL, code, session)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "exchange failed: "+err.Error())
		return
	}
	creds.BaseURL = in.BaseURL
	creds.Label = in.Label
	if err := s.store.set(creds); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"configured":      true,
		"user_id":         creds.UserID,
		"key_fingerprint": fingerprint(creds.AccessToken),
	})
}
