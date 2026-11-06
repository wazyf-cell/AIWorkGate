// Package main — what happens when the phone answers: persist, expire, submit.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

func decodeJWTExpMs(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var payload struct {
		Exp float64 `json:"exp"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil || payload.Exp <= 0 {
		return ""
	}
	return strconv.FormatInt(int64(payload.Exp*1000), 10)
}

func (s *server) complete(w http.ResponseWriter, id string, status qrStatus) {
	prev, _ := s.store.get()
	creds := credentials{
		AccessToken:  status.AccessToken,
		RefreshToken: status.RefreshToken,
		ExpiresAt:    decodeJWTExpMs(status.AccessToken),
		BaseURL:      prev.BaseURL,
		Identity:     newIdentity(),
	}
	// user_info is the reference's own account-label call; it is also the only
	// place the reference sends X-Client-Device-ID, so the device identity this
	// plugin reports is one the upstream really sees.
	if label, org, err := s.client.userInfo(context.Background(), prev.BaseURL, creds); err == nil {
		creds.Label = label
		if org != "" {
			creds.OfficeIdentity = org
		}
	}
	if err := s.store.set(creds); err != nil {
		writeError(w, http.StatusInternalServerError, "store_failed",
			"登录已成功但凭据落盘失败："+err.Error())
		return
	}
	s.logins.update(id, func(x *loginSession) {
		x.Status = "success"
		x.AccountID = "default"
	})
	writeJSON(w, http.StatusOK, map[string]any{"status": "success", "account_id": "default"})
}

// fail marks the session failed and answers 410 so the UI shows a readable
// reason instead of spinning forever.
func (s *server) fail(w http.ResponseWriter, id, message string) {
	s.logins.update(id, func(x *loginSession) {
		x.Status = "failed"
		x.Err = message
	})
	writeJSON(w, http.StatusGone, loginErrorBody("qr_expired", message))
	s.logins.remove(id)
}

func loginErrorBody(kind, message string) map[string]any {
	return map[string]any{
		"error":  map[string]any{"message": message, "type": kind, "code": kind},
		"status": "failed",
	}
}

// handleLoginSubmit answers the form endpoint the UI shows for oauth sessions.
// The reference exposes no code for the user to paste back: the phone confirms
// the scan and the poll picks the token up, so there is nothing to submit.
func (s *server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/accounts/login/"), "/submit")
	if sess, ok := s.logins.get(id); ok && sess.Status == "success" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "success", "account_id": sess.AccountID})
		return
	}
	writeError(w, http.StatusBadRequest, "no_submit_needed",
		"二维码登录不需要提交任何内容：请用手机 App 扫码并在手机上确认，本插件会自动完成。"+
			"若二维码已过期，请点「重新开始」。")
}
