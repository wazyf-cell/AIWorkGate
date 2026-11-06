package main

// The Cline gateway client: endpoint constants, the egress-aware HTTP client, the
// header sets every call carries, and the chat POST. The auth and catalog calls
// live in auth.go and catalog_client.go.

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Endpoints, all ported from def-cline.js and cline-product.ts. Each is a
// separate constant so the base and the path stay reusable, exactly as the
// reference splits apiBase from CLINE_CHAT_PATH etc.
const (
	apiBase    = "https://api.cline.bot"
	appBase    = "https://app.cline.bot"
	workOsBase = "https://api.workos.com"

	// workOsClientID is the device-authorization client id the official CLI uses.
	workOsClientID = "client_01K3A541FN8TA3EPPHTD2325AR"

	// WorkOS device flow paths (cline-product.ts:118-119).
	deviceAuthorizationPath = "/user_management/authorize/device"
	deviceAuthenticatePath  = "/user_management/authenticate"

	// Cline session paths (cline-product.ts:120-124).
	registerPath          = "/api/v1/auth/register"
	refreshPath           = "/api/v1/auth/refresh"
	chatPath              = "/api/v1/chat/completions"
	modelsPath            = "/api/v1/models"
	recommendedModelsPath = "/api/v1/ai/cline/recommended-models"

	// modelListTimeout bounds the catalog calls (CLINE_MODELS_TIMEOUT_MS).
	modelListTimeout = 20 * time.Second
	// authTimeout bounds every auth call (CLINE_HTTP_TIMEOUT_MS).
	authTimeout = 30 * time.Second
	// deviceAuthExpires is the device-code lifetime fallback.
	deviceAuthExpires = 5 * time.Minute
	// deviceAuthInterval is the polling interval fallback.
	deviceAuthInterval = 5 * time.Second
	// chatHeaderTimeout bounds the wait for response headers.
	chatHeaderTimeout = 120 * time.Second
)

// upstreamClient talks to the Cline gateway and WorkOS.
type upstreamClient struct {
	http *http.Client
	// bases are fields so tests can point them at a mock server.
	api    string
	workOs string

	// Egress state: when the host binds this account to a local forwarder, every
	// upstream call goes through it. A blocked binding fails closed instead of
	// silently falling back to a direct connection.
	mu          sync.RWMutex
	proxyURL    string
	proxyClient *http.Client
	egressErr   error
}

func newUpstreamClient() *upstreamClient {
	return &upstreamClient{
		api:    apiBase,
		workOs: workOsBase,
		http: &http.Client{
			// No total timeout: an SSE answer can run for minutes.
			Transport: &http.Transport{
				Proxy:                 nil,
				MaxIdleConnsPerHost:   8,
				IdleConnTimeout:       90 * time.Second,
				ResponseHeaderTimeout: chatHeaderTimeout,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// setEgress installs (or clears) the account's proxy route. It is called once at
// startup and again on /reload, so a binding change takes effect without a
// restart. proxyURL=="" and a nil error mean "connect directly".
func (c *upstreamClient) setEgress(proxyURL string, blocked error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.proxyURL = strings.TrimSpace(proxyURL)
	c.proxyClient = nil
	c.egressErr = blocked
	if c.proxyURL == "" {
		return
	}
	u, err := url.Parse(c.proxyURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		c.egressErr = fmt.Errorf("egress: invalid proxy url %q", c.proxyURL)
		return
	}
	base, ok := c.http.Transport.(*http.Transport)
	if !ok {
		c.egressErr = fmt.Errorf("egress: base transport is not configurable")
		return
	}
	tr := base.Clone()
	tr.Proxy = http.ProxyURL(u)
	c.proxyClient = &http.Client{
		Transport:     tr,
		CheckRedirect: c.http.CheckRedirect,
	}
}

// client returns the HTTP client to use. A blocked egress binding returns an
// error rather than an untunnelled client.
func (c *upstreamClient) client() (*http.Client, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.egressErr != nil {
		return nil, c.egressErr
	}
	if c.proxyClient != nil {
		return c.proxyClient, nil
	}
	return c.http, nil
}

// authenticateHeaders renders the auth header set for one access token.
func authenticateHeaders(accessToken string, extra ...[2]string) [][2]string {
	h := [][2]string{
		{"Authorization", "Bearer " + bearerValue(accessToken)},
		{"Accept", "application/json"},
	}
	h = append(h, clineClientHeaders...)
	return append(h, extra...)
}

// clientHeadersOnly is the set used when no credential is sent (the recommended
// models call is anonymous).
func clientHeadersOnly(extra ...[2]string) [][2]string {
	h := [][2]string{{"Accept", "application/json"}}
	h = append(h, clineClientHeaders...)
	return append(h, extra...)
}

// applyHeaders sets every pair on req, skipping empty values.
func applyHeaders(req *http.Request, pairs [][2]string) {
	for _, kv := range pairs {
		if kv[1] != "" {
			req.Header.Set(kv[0], kv[1])
		}
	}
}

// chat POSTs an OpenAI body to the gateway and returns the unread response.
func (c *upstreamClient) chat(ctx context.Context, accessToken string, body []byte) (*http.Response, error) {
	client, err := c.client()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.api+chatPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	applyHeaders(req, authenticateHeaders(accessToken, [2]string{"Content-Type", "application/json"}, [2]string{"Accept", "text/event-stream"}))
	return client.Do(req)
}

// deviceAuthorization starts the WorkOS device flow.
func (c *upstreamClient) deviceAuthorization() (*deviceAuthorization, error) {
	client, err := c.client()
	if err != nil {
		return nil, err
	}
	form := url.Values{"client_id": {workOsClientID}}
	req, err := http.NewRequest(http.MethodPost, c.workOs+deviceAuthorizationPath, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
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
		detail := stringField(payload, "error_description")
		if detail != "" {
			detail = " - " + detail
		}
		return nil, fmt.Errorf("设备码授权失败（HTTP %d）%s", resp.StatusCode, detail)
	}
	auth := &deviceAuthorization{
		DeviceCode:        stringField(payload, "device_code"),
		UserCode:          stringField(payload, "user_code"),
		VerifyURI:         stringField(payload, "verification_uri"),
		VerifyURIComplete: stringField(payload, "verification_uri_complete"),
		ExpiresIn:         secondsOr(payload, "expires_in", deviceAuthExpires),
		Interval:          secondsOr(payload, "interval", deviceAuthInterval),
	}
	if auth.DeviceCode == "" || auth.UserCode == "" || auth.VerifyURI == "" {
		return nil, fmt.Errorf("设备码授权响应缺少必要字段")
	}
	return auth, nil
}
