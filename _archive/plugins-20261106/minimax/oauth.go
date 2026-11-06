package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ------------------------------------------------------------------ OAuth

// deviceAuthorization is the parsed /oauth2/device/code response.
type deviceAuthorization struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
	// CodeVerifier is generated locally for PKCE and never comes from upstream.
	CodeVerifier string `json:"-"`
}

// startDeviceAuthorization performs the RFC 8628 device-code request with PKCE.
//
// One deliberate deviation from the reference: it sets code_challenge to a SHA-256
// of a random verifier and then sends the verifier on the poll, which is exactly
// what the reference does too (createMinimaxPkce + buildMinimaxPollBody). The
// verification URL is opened by a human, so the challenge is never verified by a
// browser exchange here.
func (c *upstreamClient) startDeviceAuthorization(ctx context.Context, accountBase string) (deviceAuthorization, error) {
	verifier, challenge, err := newPKCE()
	if err != nil {
		return deviceAuthorization{}, err
	}
	form := url.Values{
		"client_id":             {productClientID},
		"scope":                 {productScope},
		"audience":              {productAudience},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	var parsed struct {
		DeviceCode              string      `json:"device_code"`
		UserCode                string      `json:"user_code"`
		VerificationURI         string      `json:"verification_uri"`
		VerificationURL         string      `json:"verification_url"`
		VerificationURIComplete string      `json:"verification_uri_complete"`
		ExpiresIn               json.Number `json:"expires_in"`
		Interval                json.Number `json:"interval"`
	}
	if err := c.postForm(ctx, c.oauthBase(accountBase)+deviceCodePath, form, oauthTimeout, &parsed); err != nil {
		return deviceAuthorization{}, err
	}
	auth := deviceAuthorization{
		DeviceCode:              parsed.DeviceCode,
		UserCode:                parsed.UserCode,
		VerificationURI:         firstNonEmpty(parsed.VerificationURI, parsed.VerificationURL),
		VerificationURIComplete: firstNonEmpty(parsed.VerificationURIComplete, parsed.VerificationURI, parsed.VerificationURL),
		ExpiresIn:               parsePositiveInt(parsed.ExpiresIn),
		Interval:                parsePositiveInt(parsed.Interval),
		CodeVerifier:            verifier,
	}
	if auth.DeviceCode == "" || auth.UserCode == "" || auth.VerificationURI == "" {
		return deviceAuthorization{}, fmt.Errorf("minimax: device authorization response is missing device_code/user_code/verification_uri")
	}
	if auth.ExpiresIn == 0 {
		return deviceAuthorization{}, fmt.Errorf("minimax: device authorization response is missing expires_in")
	}
	if auth.Interval == 0 {
		auth.Interval = 5
	}
	return auth, nil
}

// tokenGrant is the parsed /oauth2/token response.
type tokenGrant struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
}

// pollDeviceToken asks for the token once. It returns pending/slowDown so the
// caller can keep polling, matching the reference's state machine.
//
// Measured (2026-10-22): an unapproved device answers HTTP 400 with
// {"error":"authorization_pending",...}. A non-2xx body must therefore be
// decoded, not treated as a failure.
func (c *upstreamClient) pollDeviceToken(ctx context.Context, accountBase string, auth deviceAuthorization) (grant tokenGrant, status string, err error) {
	form := url.Values{
		"grant_type":    {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code":   {auth.DeviceCode},
		"client_id":     {productClientID},
		"code_verifier": {auth.CodeVerifier},
	}
	var raw struct {
		AccessToken  string      `json:"access_token"`
		RefreshToken string      `json:"refresh_token"`
		TokenType    string      `json:"token_type"`
		ExpiresIn    json.Number `json:"expires_in"`
		Scope        string      `json:"scope"`
		Status       string      `json:"status"`
		Error        string      `json:"error"`
		Description  string      `json:"error_description"`
	}
	code, data, err := c.postFormStatus(ctx, c.oauthBase(accountBase)+tokenPath, form, oauthTimeout)
	if err != nil {
		return tokenGrant{}, "", err
	}
	if len(data) > 0 {
		if json.Unmarshal(data, &raw) != nil {
			return tokenGrant{}, "", fmt.Errorf("minimax: token response is not JSON (HTTP %d)", code)
		}
	}
	switch raw.Status {
	case "pending", "slow_down", "denied", "access_denied", "expired", "expired_token":
		return tokenGrant{}, raw.Status, nil
	}
	switch raw.Error {
	case "authorization_pending":
		return tokenGrant{}, "pending", nil
	case "slow_down":
		return tokenGrant{}, "slow_down", nil
	case "access_denied":
		return tokenGrant{}, "denied", nil
	case "expired_token", "invalid_grant":
		return tokenGrant{}, "expired", nil
	}
	if code < 200 || code >= 300 {
		detail := strings.TrimSpace(raw.Error)
		if detail == "" {
			detail = truncate(string(data), 200)
		}
		return tokenGrant{}, "", fmt.Errorf("minimax: token poll failed (HTTP %d): %s", code, detail)
	}
	if raw.Error != "" {
		return tokenGrant{}, "", fmt.Errorf("minimax: authorization failed: %s", raw.Error)
	}
	g := tokenGrant{
		AccessToken:  raw.AccessToken,
		RefreshToken: raw.RefreshToken,
		TokenType:    raw.TokenType,
		ExpiresIn:    parsePositiveInt(raw.ExpiresIn),
		Scope:        raw.Scope,
	}
	if g.AccessToken == "" {
		return tokenGrant{}, "", fmt.Errorf("minimax: token response is missing access_token")
	}
	if g.RefreshToken == "" {
		return tokenGrant{}, "", fmt.Errorf("minimax: token response is missing refresh_token")
	}
	if !strings.EqualFold(g.TokenType, "bearer") {
		return tokenGrant{}, "", fmt.Errorf("minimax: token response token_type is not Bearer")
	}
	if g.ExpiresIn == 0 {
		return tokenGrant{}, "", fmt.Errorf("minimax: token response is missing expires_in")
	}
	if !strings.Contains(g.Scope, productScope) {
		return tokenGrant{}, "", fmt.Errorf("minimax: token response scope does not contain %s", productScope)
	}
	return g, "", nil
}

// refresh exchanges a refresh_token. The form is buildMinimaxRefreshBody exactly:
// grant_type, refresh_token, client_id, scope, audience.
func (c *upstreamClient) refresh(ctx context.Context, accountBase, refreshToken string) (tokenGrant, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {productClientID},
		"scope":         {productScope},
		"audience":      {productAudience},
	}
	var raw struct {
		AccessToken  string      `json:"access_token"`
		RefreshToken string      `json:"refresh_token"`
		TokenType    string      `json:"token_type"`
		ExpiresIn    json.Number `json:"expires_in"`
		Scope        string      `json:"scope"`
		Error        string      `json:"error"`
	}
	if err := c.postForm(ctx, c.oauthBase(accountBase)+tokenPath, form, oauthTimeout, &raw); err != nil {
		return tokenGrant{}, err
	}
	if raw.AccessToken == "" {
		return tokenGrant{}, fmt.Errorf("minimax: refresh response is missing access_token")
	}
	g := tokenGrant{
		AccessToken:  raw.AccessToken,
		RefreshToken: firstNonEmpty(raw.RefreshToken, refreshToken),
		TokenType:    raw.TokenType,
		ExpiresIn:    parsePositiveInt(raw.ExpiresIn),
		Scope:        raw.Scope,
	}
	// The reference runs the refresh through the same parseMinimaxTokenGrant as
	// the device grant, so the same three fields are mandatory.
	if !strings.EqualFold(g.TokenType, "bearer") {
		return tokenGrant{}, fmt.Errorf("minimax: refresh response token_type is not Bearer")
	}
	if g.ExpiresIn == 0 {
		return tokenGrant{}, fmt.Errorf("minimax: refresh response is missing expires_in")
	}
	if !strings.Contains(g.Scope, productScope) {
		return tokenGrant{}, fmt.Errorf("minimax: refresh response scope does not contain %s", productScope)
	}
	return g, nil
}

// postForm POSTs an x-www-form-urlencoded body and decodes the JSON answer.
func (c *upstreamClient) postForm(ctx context.Context, target string, form url.Values, timeout time.Duration, out any) error {
	code, data, err := c.postFormStatus(ctx, target, form, timeout)
	if err != nil {
		return err
	}
	if code < 200 || code >= 300 {
		return fmt.Errorf("minimax: HTTP %d: %s", code, truncate(string(data), 256))
	}
	return json.Unmarshal(data, out)
}

// postFormStatus posts a form and returns the raw status and body. It exists for
// the OAuth device grant, whose pending/slow_down answers are HTTP 400 with a
// JSON error body that must be interpreted rather than treated as a failure.
func (c *upstreamClient) postFormStatus(ctx context.Context, target string, form url.Values, timeout time.Duration) (int, []byte, error) {
	client, err := c.client()
	if err != nil {
		return 0, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, data, nil
}

// newPKCE returns a verifier and its S256 challenge, both base64url without
// padding (createMinimaxPkce does the same with 32 random bytes).
func newPKCE() (verifier, challenge string, err error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(b[:])
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// parsePositiveInt reads a JSON number that upstream may send either as a number
// or as a numeric string.
func parsePositiveInt(n json.Number) int {
	if n == "" {
		return 0
	}
	v, err := n.Int64()
	if err != nil || v <= 0 {
		return 0
	}
	return int(v)
}
