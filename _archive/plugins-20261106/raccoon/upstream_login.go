package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------- login calls

// qrLoginPath is authApiPrefix + "/login_with_qrcode_code" (pollRaccoonQrLogin).
const qrLoginPath = authApiPrefix + "/login_with_qrcode_code"

// userInfoPath is authApiPrefix + "/user_info" (fetchRaccoonUserInfo).
const userInfoPath = authApiPrefix + "/user_info"

// qrStatus is one pollRaccoonQrLogin answer.
type qrStatus struct {
	Status       string
	AccessToken  string
	RefreshToken string
	ExpiredAt    string
}

// expiredAtTime parses the upstream's expired_at for DISPLAY/DIAGNOSIS only.
//
// DO NOT use this to decide whether a QR code has expired. The reference
// implementation (raccoon-oauth.ts pollRaccoonQrLogin) never does: it only
// carries the value through for the page to show, and the wire format has never
// been observed. This parser used to gate the poll with it, and because the
// upstream may send a *relative* value ("300" = 300s left), it was interpreted
// as an absolute instant in 1970 — so every scan looked expired on the first
// poll. Expiry is decided by the session's own ExpiresAt, and a stale code is
// answered by minting a fresh one (login.go "canceled" branch).
func (s qrStatus) expiredAtTime() (time.Time, bool) {
	raw := strings.TrimSpace(s.ExpiredAt)
	if raw == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, true
		}
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
		if n > 1e12 { // milliseconds
			return time.UnixMilli(n), true
		}
		return time.Unix(n, 0), true
	}
	return time.Time{}, false
}

// pollQRCode issues the scan-status poll. It is unauthenticated in the
// reference (postJson with no headers beyond Content-Type), which is what makes
// the QR path usable without any secret.
func (c *upstreamClient) pollQRCode(ctx context.Context, base, code string) (qrStatus, error) {
	body, err := json.Marshal(map[string]string{"qrcode_code": code})
	if err != nil {
		return qrStatus{}, err
	}
	reqCtx, cancel := context.WithTimeout(ctx, catalogTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		originOf(base)+qrLoginPath, bytes.NewReader(body))
	if err != nil {
		return qrStatus{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.clientFor("").Do(req)
	if err != nil {
		return qrStatus{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return qrStatus{}, err
	}
	var env struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Status       string `json:"status"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			ExpiredAt    string `json:"expired_at"`
		} `json:"data"`
	}
	if err := jsonUnmarshalObject(raw, &env); err != nil {
		return qrStatus{}, fmt.Errorf("raccoon: scan poll is not JSON: %w", err)
	}
	if env.Code != 0 {
		return qrStatus{}, fmt.Errorf("raccoon: scan poll code %d: %s", env.Code, truncate(env.Message, 128))
	}
	return qrStatus{
		Status:       env.Data.Status,
		AccessToken:  env.Data.AccessToken,
		RefreshToken: env.Data.RefreshToken,
		ExpiredAt:    env.Data.ExpiredAt,
	}, nil
}

// userInfo reads the account label and office identity (fetchRaccoonUserInfo).
//
// This is the one reference call that sends X-Client-Device-ID (raccoonHeaders),
// so it is also the call that makes the plugin's reported device identity a
// value the upstream actually saw. Chat and the catalog deliberately keep the
// smaller header sets the reference gives them.
func (c *upstreamClient) userInfo(ctx context.Context, base string, cred credentials) (label, officeIdentity string, err error) {
	reqCtx, cancel := context.WithTimeout(ctx, catalogTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, originOf(base)+userInfoPath, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	req.Header.Set("X-Org-Code", cred.OfficeIdentity)
	req.Header.Set("X-Raccoon-Language", "zh")
	req.Header.Set("X-Client-Platform", clientPlatform)
	req.Header.Set("X-Client-Version", clientVersion)
	if id := cred.Identity.deviceID(); id != "" {
		req.Header.Set("X-Client-Device-ID", id)
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.clientFor("").Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("raccoon: user_info HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", "", err
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Name           string `json:"name"`
			OfficeIdentity string `json:"office_identity"`
		} `json:"data"`
	}
	if err := jsonUnmarshalObject(raw, &env); err != nil {
		return "", "", err
	}
	if env.Code != 0 {
		return "", "", fmt.Errorf("raccoon: user_info code %d", env.Code)
	}
	return strings.TrimSpace(env.Data.Name), strings.TrimSpace(env.Data.OfficeIdentity), nil
}
