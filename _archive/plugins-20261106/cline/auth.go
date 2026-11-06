package main

// The auth calls: WorkOS device authorization + token polling, Cline session
// register/refresh, and the paste-token probe. All of them share upstream.go's
// egress-aware client.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// deviceAuthorization is one pending WorkOS device login.
type deviceAuthorization struct {
	DeviceCode        string
	UserCode          string
	VerifyURI         string
	VerifyURIComplete string
	ExpiresIn         time.Duration
	Interval          time.Duration
}

// LoginURL is where the operator types the code.
func (a *deviceAuthorization) LoginURL() string {
	if a.VerifyURIComplete != "" {
		return a.VerifyURIComplete
	}
	return a.VerifyURI
}

// devicePollState is one poll iteration's outcome. The set mirrors the OAuth
// device-grant errors WorkOS returns; the reference switches on exactly these.
type devicePollState string

const (
	pollAuthorized devicePollState = "authorized"
	pollPending    devicePollState = "authorization_pending"
	pollSlowDown   devicePollState = "slow_down"
	pollDenied     devicePollState = "access_denied"
	pollExpired    devicePollState = "expired_token"
	// pollFailed is a fatal, non-retryable answer (unknown error, bad envelope).
	pollFailed devicePollState = "failed"
	// pollTransport is a network-level failure, which the reference retries up to
	// CLINE_POLL_MAX_FAILURES times.
	pollTransport devicePollState = "transport_error"
)

// pollDeviceTokens performs one poll iteration.
//
// Measured against the live endpoint (2026-10-22): an unapproved device answers
// HTTP 400 with {"error":"authorization_pending"}, so a non-2xx answer must be
// decoded rather than treated as a failure. slow_down answers the same way and
// requires the interval to grow.
func (c *upstreamClient) pollDeviceTokens(deviceCode string) (tokens *workOsTokens, state devicePollState, err error) {
	client, err := c.client()
	if err != nil {
		return nil, pollFailed, err
	}
	form := url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {deviceCode},
		"client_id":   {workOsClientID},
	}
	req, err := http.NewRequest(http.MethodPost, c.workOs+deviceAuthenticatePath, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, pollFailed, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return nil, pollTransport, err
	}
	defer resp.Body.Close()
	payload, err := decodeMap(resp.Body)
	if err != nil {
		return nil, pollFailed, fmt.Errorf("WorkOS token 轮询响应不可解析（HTTP %d）", resp.StatusCode)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		access := stringField(payload, "access_token")
		refresh := stringField(payload, "refresh_token")
		if access == "" || refresh == "" {
			return nil, pollFailed, fmt.Errorf("WorkOS token 响应缺少必要字段")
		}
		return &workOsTokens{AccessToken: access, RefreshToken: refresh}, pollAuthorized, nil
	}
	detail := stringField(payload, "error_description")
	switch stringField(payload, "error") {
	case "authorization_pending":
		return nil, pollPending, nil
	case "slow_down":
		return nil, pollSlowDown, nil
	case "access_denied":
		if detail == "" {
			detail = "WorkOS 授权被拒绝"
		}
		return nil, pollDenied, fmt.Errorf("%s", detail)
	case "expired_token", "invalid_grant":
		if detail == "" {
			detail = "WorkOS 设备码已过期，请重新发起登录"
		}
		return nil, pollExpired, fmt.Errorf("%s", detail)
	}
	if detail != "" {
		detail = " - " + detail
	}
	return nil, pollFailed, fmt.Errorf("WorkOS token 轮询失败（HTTP %d）%s", resp.StatusCode, detail)
}

// workOsTokens is the pair WorkOS hands back.
type workOsTokens struct {
	AccessToken  string
	RefreshToken string
}

// register exchanges WorkOS tokens for a Cline session.
func (c *upstreamClient) register(tokens workOsTokens) (*clineCredential, error) {
	client, err := c.client()
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]string{
		"accessToken":  tokens.AccessToken,
		"refreshToken": tokens.RefreshToken,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, c.api+registerPath, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	applyHeaders(req, clientHeadersOnly())
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	payload, err := decodeMap(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("token 注册失败（HTTP %d）", resp.StatusCode)
	}
	return buildCredential(payload), nil
}

// refresh renews a credential via POST /api/v1/auth/refresh.
func (c *upstreamClient) refresh(refreshToken string) (*clineCredential, error) {
	base, err := c.client()
	if err != nil {
		return nil, err
	}
	// Field names are camelCase on the wire (clineRefreshBody).
	body, err := json.Marshal(map[string]string{
		"refreshToken": refreshToken,
		"grantType":    "refresh_token",
	})
	if err != nil {
		return nil, err
	}
	client := *base
	client.Timeout = authTimeout
	req, err := http.NewRequest(http.MethodPost, c.api+refreshPath, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	applyHeaders(req, clientHeadersOnly())
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Cline 续期网络失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, errRefreshRejected
	}
	payload, err := decodeMap(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("Cline 续期响应不是 JSON（HTTP %d）", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Cline 续期失败（HTTP %d）", resp.StatusCode)
	}
	creds := buildCredential(payload)
	if creds.AccessToken == "" {
		return nil, errRefreshRejected
	}
	return creds, nil
}

var errRefreshRejected = fmt.Errorf("Cline 续期被拒绝，请重新登录")

// probeToken checks a pasted token against inference. The models catalog
// answers anonymously, so a chat call is the only endpoint that distinguishes a
// real token: 401/403 means rejected; anything else (200, 402, 429) means the
// token authenticated, even if the account is out of credit.
func (c *upstreamClient) probeToken(ctx context.Context, accessToken string) error {
	body := []byte(`{"model":"cline-free/mimo-v2.6-flash","max_tokens":1,"stream":true,` +
		`"messages":[{"role":"user","content":"ping"}]}`)
	resp, err := c.chat(ctx, accessToken, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}
