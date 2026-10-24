// Package main — the "续期" button (POST /accounts/{id}/refresh).
//
// Ported from refreshRaccoonCredential (raccoon-oauth.ts):
//
//	POST <apiBase>/api/web/auth/v1/refresh {"refresh_token": "<rt>"}
//	401 or envelope code 200003 -> the login is dead, the user must log in again
//	other non-zero code         -> 续期失败 + the upstream message
//	success                     -> new access_token, refresh_token kept if absent
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// refreshPath is authApiPrefix + "/refresh".
const refreshPath = authApiPrefix + "/refresh"

// refreshError marks a refresh that ended because the login is dead.
type refreshError struct {
	message string
	dead    bool
}

func (e *refreshError) Error() string { return e.message }

func (c *upstreamClient) refresh(ctx context.Context, base string, cred credentials, proxyURL string) (credentials, error) {
	if strings.TrimSpace(cred.RefreshToken) == "" {
		return cred, &refreshError{message: "Raccoon: 没有 refresh_token，无法续期，请重新登录", dead: true}
	}
	body, err := json.Marshal(map[string]string{"refresh_token": cred.RefreshToken})
	if err != nil {
		return cred, err
	}
	reqCtx, cancel := context.WithTimeout(ctx, catalogTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, originOf(base)+refreshPath, bytes.NewReader(body))
	if err != nil {
		return cred, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	// Refresh is an upstream call like any other: it leaves through the account's
	// egress binding, so a proxied account never refreshes its token in the
	// clear and strict mode refuses a direct refresh.
	resp, err := c.clientFor(proxyURL).Do(req)
	if err != nil {
		return cred, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return cred, err
	}
	var env struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Details string `json:"details"`
		Data    struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &env)
	if resp.StatusCode == http.StatusUnauthorized || env.Code == 200003 {
		return cred, &refreshError{message: "Raccoon 登录态已过期，请重新登录", dead: true}
	}
	if env.Code != 0 {
		msg := strings.TrimSpace(env.Message)
		if msg == "" {
			msg = fmt.Sprintf("code %d", env.Code)
		}
		if d := strings.TrimSpace(env.Details); d != "" {
			msg += ": " + d
		}
		return cred, &refreshError{message: "Raccoon 续期失败：" + msg}
	}
	if strings.TrimSpace(env.Data.AccessToken) == "" {
		return cred, fmt.Errorf("raccoon: 续期响应缺少 access_token")
	}
	next := cred
	next.AccessToken = env.Data.AccessToken
	if strings.TrimSpace(env.Data.RefreshToken) != "" {
		next.RefreshToken = env.Data.RefreshToken
	}
	if exp := decodeJWTExpMs(next.AccessToken); exp != "" {
		next.ExpiresAt = exp
	}
	return next, nil
}

// handleRefresh serves POST /accounts/{id}/refresh.
func (s *server) handleRefresh(w http.ResponseWriter, r *http.Request, accountID string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	creds, ok := s.store.get()
	if !ok {
		writeError(w, http.StatusConflict, "not_configured", "尚未登录，没有可续期的凭据")
		return
	}
	// The plugin stores one account, reported as "default" by /accounts, and
	// chat/catalog resolve their egress under that same id.
	proxyURL, allow := s.proxyFor(w, "default")
	if !allow {
		return
	}
	next, err := s.client.refresh(r.Context(), creds.BaseURL, creds, proxyURL)
	if err != nil {
		var re *refreshError
		if errors.As(err, &re) {
			status := http.StatusBadGateway
			if re.dead {
				status = http.StatusUnauthorized
			}
			writeError(w, status, "refresh_failed", re.message)
			return
		}
		writeError(w, http.StatusBadGateway, "refresh_failed", err.Error())
		return
	}
	next.Label = creds.Label
	if next.Identity == nil {
		next.Identity = newIdentity()
	}
	if err := s.store.set(next); err != nil {
		writeError(w, http.StatusInternalServerError, "store_failed", "续期成功但落盘失败："+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"account_id":      accountID,
		"key_fingerprint": fingerprint(next.AccessToken),
		"expires_at":      next.ExpiresAt,
		"updated_at":      time.Now().Unix(),
	})
}
