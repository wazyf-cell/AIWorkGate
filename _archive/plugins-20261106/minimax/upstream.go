// Package main — upstream client for the MiniMax Code agent backend.
//
// Every endpoint and header below is lifted from the reference implementation's
// vendor/channel-pack/src/minimax*.ts and its pack.js bundle. Nothing here is
// guessed:
//
//	accountHost  https://account.minimax.cn     (OAuth, device code + refresh)
//	apiHost      https://agent.minimax.cn       (models, inference, credits)
//
//	/oauth2/device/code                          POST form  (device authorization)
//	/oauth2/token                                POST form  (poll + refresh)
//	/mavis/api/v1/models?region=cn&buildEnv=prod GET
//	/mavis/api/v1/llm/v1/messages                POST (Anthropic Messages, SSE)
//	/minimax-cloud/api/v1/signin/status          GET  ?timezone_id=
//	/minimax-cloud/api/v1/signin/claim           POST {} ?timezone_id=
//	/minimax-cloud/api/v1/credit/details         GET
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	accountHost = "https://account.minimax.cn"
	apiHost     = "https://agent.minimax.cn"

	// Product constants, verbatim from the MINIMAX product object.
	productRegion   = "cn"
	productBuildEnv = "prod"
	productClientID = "mcode-public"
	productAudience = "agent-backend"
	productScope    = "agent.default"

	deviceCodePath  = "/oauth2/device/code"
	tokenPath       = "/oauth2/token"
	modelsPath      = "/mavis/api/v1/models"
	inferPath       = "/mavis/api/v1/llm/v1/messages"
	signinStatusPth = "/minimax-cloud/api/v1/signin/status"
	signinClaimPth  = "/minimax-cloud/api/v1/signin/claim"
	creditPath      = "/minimax-cloud/api/v1/credit/details"

	// The reference's timeouts: 30 s for ordinary calls, 20 s for OAuth.
	requestTimeout = 30 * time.Second
	oauthTimeout   = 20 * time.Second

	// Inline limits declared by the definition.
	maxImageBytesInline = 10 * 1024 * 1024
	maxRequestBodyBytes = 64 * 1024 * 1024
)

// apiBase is the inference origin. A package var so tests can point it at a
// mock server without touching the production constant.
var apiBase = apiHost

// upstreamClient issues MiniMax-shaped requests and returns raw responses.
type upstreamClient struct {
	http *http.Client
	// accountBase overrides the OAuth origin (tests set it to a mock server).
	accountBase string

	// Egress: the host binds the account to a local forwarder. A blocked binding
	// fails closed; it never falls back to a direct connection.
	mu          sync.RWMutex
	proxyClient *http.Client
	egressErr   error
}

func newUpstreamClient() *upstreamClient {
	return &upstreamClient{
		http:        &http.Client{Timeout: 5 * time.Minute},
		accountBase: accountHost,
	}
}

// setEgress installs (or clears) the account's proxy route. proxyURL=="" with a
// nil error means direct.
func (c *upstreamClient) setEgress(proxyURL string, blocked error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.proxyClient = nil
	c.egressErr = blocked
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return
	}
	u, err := url.Parse(proxyURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		c.egressErr = fmt.Errorf("egress: invalid proxy url %q", proxyURL)
		return
	}
	c.proxyClient = &http.Client{
		Timeout: c.http.Timeout,
		Transport: &http.Transport{
			Proxy:                 http.ProxyURL(u),
			MaxIdleConnsPerHost:   8,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: 2 * time.Minute,
		},
	}
}

// client returns the HTTP client to use, or the blocked-binding error.
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

// oauthBase resolves the OAuth origin: a stored override, then the client's
// configured base, then the production host.
func (c *upstreamClient) oauthBase(override string) string {
	if base := strings.TrimSpace(override); base != "" {
		return originOr(base, accountHost)
	}
	if base := strings.TrimSpace(c.accountBase); base != "" {
		return originOr(base, accountHost)
	}
	return accountHost
}

// inferHeaders is minimaxInferHeaders: bearer + JSON + SSE accept. There is no
// product/version header family on this channel — the reference sends only these
// three.
func inferHeaders(accessToken string) [][2]string {
	return [][2]string{
		{"Authorization", "Bearer " + accessToken},
		{"Content-Type", "application/json"},
		{"Accept", "text/event-stream"},
	}
}

// jsonHeaders is minimaxHeaders: bearer + JSON accept, used by models, credits
// and sign-in.
func jsonHeaders(accessToken string) [][2]string {
	return [][2]string{
		{"Authorization", "Bearer " + accessToken},
		{"Accept", "application/json"},
	}
}

// messages POSTs an Anthropic-shaped body to the inference endpoint and returns
// an unread response. The caller owns closing the body.
func (c *upstreamClient) messages(ctx context.Context, accessToken, base string, body []byte) (*http.Response, error) {
	if len(body) > maxRequestBodyBytes {
		return nil, fmt.Errorf("minimax: request body %d bytes exceeds the %d byte limit", len(body), maxRequestBodyBytes)
	}
	client, err := c.client()
	if err != nil {
		return nil, err
	}
	target := originOr(base, apiHost) + inferPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for _, kv := range inferHeaders(accessToken) {
		req.Header.Set(kv[0], kv[1])
	}
	return client.Do(req)
}

// models lists the remote catalog. region and buildEnv are query parameters the
// reference always sets.
func (c *upstreamClient) models(ctx context.Context, accessToken, base string) ([]byte, error) {
	u, err := url.Parse(originOr(base, apiBase) + modelsPath)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("region", productRegion)
	q.Set("buildEnv", productBuildEnv)
	u.RawQuery = q.Encode()
	return c.getJSON(ctx, accessToken, u.String())
}

// credits reads the credit details envelope.
func (c *upstreamClient) credits(ctx context.Context, accessToken, base string) ([]byte, error) {
	return c.getJSON(ctx, accessToken, originOr(base, apiHost)+creditPath)
}

// getJSON performs a bearer GET with a bounded read.
func (c *upstreamClient) getJSON(ctx context.Context, accessToken, target string) ([]byte, error) {
	client, err := c.client()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	for _, kv := range jsonHeaders(accessToken) {
		req.Header.Set(kv[0], kv[1])
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("minimax: HTTP %d: %s", resp.StatusCode, truncate(string(data), 256))
	}
	return data, nil
}

// originOr lets a stored base_url (a mirror or a test server) override the
// production origin without editing the constant.
func originOr(base, fallback string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return fallback
	}
	return base
}
