package main

// The account surface: one Cline credential, projected as account "default".
// There is no account pool and no enabled flag in the store, so the handlers only
// report what exists.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// handleAccounts reports one account while a login is stored.
func (s *server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	creds, ok := s.store.get()
	accounts := []any{}
	if ok {
		now := time.Now().UnixMilli()
		status := "ok"
		if creds.expired(now) {
			status = "expired"
		}
		account := map[string]any{
			"id":      "default",
			"label":   creds.label(),
			"enabled": true,
			"status":  status,
			"credential": map[string]any{
				"present":     true,
				"refreshable": creds.refreshable(),
			},
			// Quota is unknown, not zero. Claiming a number would be fabricated:
			// Cline reports credit only inside a 402, not as a readable balance.
			"quota": map[string]any{},
		}
		if creds.ExpireTime > 0 {
			account["credential"].(map[string]any)["expires_at"] = creds.ExpireTime / 1000
		}
		accounts = append(accounts, account)
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": accounts})
}

// handleAccountSub serves PATCH /accounts/{id}, POST /accounts/{id}/refresh and
// POST /accounts/{id}/identity/reset.
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
		// One sealed credential, no account-level enabled flag: acknowledging an
		// {"enabled":false} patch would claim a persistence that does not exist.
		// Signing out is DELETE.
		writeError(w, http.StatusNotImplemented, "not_supported",
			"此渠道只有一个账号，且没有可持久化的 enabled 状态；要停止使用请 DELETE /accounts/default")
	case action == "" && r.Method == http.MethodDelete:
		if err := s.store.clear(); err != nil {
			writeError(w, http.StatusInternalServerError, "delete_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": accountID})
	case action == "refresh" && r.Method == http.MethodPost:
		s.handleRefresh(w, r)
	case action == "identity/reset" && r.Method == http.MethodPost:
		// This channel's identity is the WorkOS token itself; there is no device
		// fingerprint to rotate. Say so instead of inventing a no-op success.
		writeError(w, http.StatusNotImplemented, "not_implemented",
			"Cline 的设备身份由 WorkOS token 承担，没有可单独重置的指纹")
	case action == "checkin" && r.Method == http.MethodPost:
		writeError(w, http.StatusNotImplemented, "not_implemented",
			"Cline 没有签到机制（免费额度按天按模型由服务端结算，不由插件触发）")
	default:
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint: "+r.Method+" "+r.URL.Path)
	}
}

// handleAdminCredentials lets the operator hand tokens to the plugin directly.
func (s *server) handleAdminCredentials(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST or PUT")
		return
	}
	var in struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		Nickname     string `json:"nickname"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	cred := &clineCredential{
		AccessToken:  in.AccessToken,
		RefreshToken: in.RefreshToken,
		Nickname:     in.Nickname,
	}
	if err := s.store.set(cred); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	stored, _ := s.store.get()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"configured":      true,
		"key_fingerprint": fingerprint(stored.AccessToken),
		"refreshable":     stored.refreshable(),
	})
}
