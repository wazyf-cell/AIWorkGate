package main

import (
	"net/http"
	"strings"
	"time"
)

// handleAccounts reports exactly one account. The free tier is anonymous by
// default, so the account exists before any credential is stored — an empty list
// would make every opencode/<model> call fail with 503 before this plugin runs.
func (s *server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	creds := s.store.current()
	account := map[string]any{
		"id":      "default",
		"label":   firstNonEmpty(creds.Label, "OpenCode 匿名通道"),
		"enabled": true,
		"status":  "ok",
		"credential": map[string]any{
			"present": true,
			// The anonymous literal cannot be renewed; claiming it could would
			// put a false "auto renew" promise in the UI.
			"refreshable": !creds.anonymous(),
		},
		// Quota is unknown, not zero: the host only parks an account when
		// remaining <= 0 while total or expires_at is set. A number here would
		// be fabricated.
		"quota": map[string]any{},
		// The identity block gives the UI something real to reset: device_id is
		// the x-opencode-project fingerprint every chat sends.
		"identity": identityView(creds),
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": []any{account}})
}

// handleAccountSub serves PATCH /accounts/{id}, POST /accounts/{id}/refresh,
// GET /accounts/{id}/identity and POST /accounts/{id}/identity/reset.
func (s *server) handleAccountSub(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/accounts/"), "/")
	if rest == "" {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
		return
	}
	parts := strings.Split(rest, "/")
	accountID := strings.TrimSpace(parts[0])
	action := ""
	if len(parts) > 1 {
		action = strings.Join(parts[1:], "/")
	}
	if accountID != "default" {
		writeError(w, http.StatusConflict, "account_unavailable", "unknown account "+accountID)
		return
	}
	switch {
	case action == "" && r.Method == http.MethodPatch:
		// Enabling/disabling is a host-side concern and there is one account that
		// cannot be dropped, so acknowledge instead of silently ignoring.
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case action == "refresh" && r.Method == http.MethodPost:
		s.handleRefresh(w)
	case action == "identity" && r.Method == http.MethodGet:
		s.handleIdentityDetails(w)
	case action == "identity/reset" && r.Method == http.MethodPost:
		s.handleIdentityReset(w, r)
	case action == "checkin" && r.Method == http.MethodPost:
		// There is no check-in on this channel; saying so beats pretending.
		writeError(w, http.StatusNotImplemented, "not_implemented",
			"OpenCode 没有签到机制（免费额度不按天或按签到发放）")
	default:
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint: "+r.Method+" "+r.URL.Path)
	}
}

// handleRefresh reports credential state. This channel has no refresh_token: an
// sk- key does not expire and the anonymous literal cannot be rotated, so the
// answer is honest rather than a fake renewal.
func (s *server) handleRefresh(w http.ResponseWriter) {
	message := "该渠道没有 refresh_token 概念（Zen key 不过期）；无需续期"
	if s.store.current().anonymous() {
		message = "匿名通道没有可续期的凭据；如需重置设备指纹请调 /accounts/default/identity/reset"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"result": "ok", "message": message, "at": time.Now().Unix(),
	})
}
