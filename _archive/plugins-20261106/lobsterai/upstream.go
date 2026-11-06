// Package main — upstream client for LobsterAI (有道).
//
// Everything here is ported from the reference implementation
// (vendor/channel-pack/src/lobsterai.ts, lobsterai-product.ts,
// lobsterai-oauth.ts in dsh-our-free-model-2.5.3). Constants carry the reference
// symbol they came from; nothing is invented.
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
	// apiBase is LOBSTERAI_API_BASE (lobsterai-product.ts).
	apiBase = "https://lobsterai-server.youdao.com"

	// portalBase is LOBSTERAI_PORTAL_BASE: the web portal the login page lives on.
	portalBase = "https://lobsterai.youdao.com"

	// clientVersionAPI is LOBSTERAI_CLIENT_VERSION_API: the overmind endpoint that
	// reports the current desktop version. The models endpoint's query is an
	// identity payload and the reference warns a wrong version returns the wrong
	// model set, so this is resolved (with a cached fallback) rather than guessed.
	clientVersionAPI = "https://api-overmind.youdao.com/openapi/get/luna/hardware/lobsterai/prod/update"

	// fallbackClientVersion is LOBSTERAI_FALLBACK_CLIENT_VERSION.
	fallbackClientVersion = "2026.9.4"

	// clientCapabilities is LOBSTERAI_CLIENT_CAPABILITIES. The reference is
	// explicit that the models endpoint filters by this header: without it
	// `kimi-k3` is not returned (25 vs 26 models).
	clientCapabilities = "kimi-k3-agentic-v1,thinking-level-control-v1"

	// userAgent is LOBSTERAI_USER_AGENT.
	userAgent = "LobsterAI/0.1.0"

	// Paths, verbatim from lobsterai.ts.
	exchangePath = "/api/auth/exchange"
	refreshPath  = "/api/auth/refresh"
	modelsPath   = "/api/models/available"
	chatPath     = "/api/proxy/v1/chat/completions"
	callbackPath = "/auth/callback"

	// requestTimeout is LOBSTERAI_REQUEST_TIMEOUT_MS = 3e4.
	requestTimeout = 30 * time.Second

	// versionCacheTTL is LOBSTERAI_VERSION_CACHE_TTL_MS = 12h.
	versionCacheTTL = 12 * time.Hour
)

// upstreamClient issues LobsterAI's OpenAI-shaped requests.
type upstreamClient struct {
	http *http.Client
	// transport resolves an egress proxy URL to a cached transport. Nil for a
	// standalone client (tests), which always goes direct.
	transport func(string) *http.Transport

	// version caching mirrors LobsteraiClientVersionResolver: a 12h cache, a
	// remote lookup, and a fallback constant that never turns into an error.
	versionMu   sync.Mutex
	version     string
	versionAt   time.Time
	versionOnce sync.Once
}

func newUpstreamClient() *upstreamClient {
	return &upstreamClient{http: &http.Client{
		Transport: &http.Transport{
			Proxy:                 nil,
			MaxIdleConnsPerHost:   8,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: requestTimeout,
		},
	}}
}

func originOf(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return apiBase
	}
	return base
}

// clientFor returns a client bound to one egress route.
func (c *upstreamClient) clientFor(proxy string) *http.Client {
	if strings.TrimSpace(proxy) == "" || c.transport == nil {
		return c.http
	}
	tr := c.transport(proxy)
	if tr == nil {
		return c.http
	}
	// No whole-request timeout: a proxied SSE answer may legitimately outlive
	// requestTimeout. The transport still bounds the wait for response headers.
	// No whole-request timeout: a proxied SSE answer may legitimately outlive
	// requestTimeout. The transport still bounds the wait for response headers.
	return &http.Client{Transport: tr}
}

// chat POSTs an OpenAI chat body and returns an unread response.
func (c *upstreamClient) chat(ctx context.Context, base, token, version string, body []byte, proxy string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, originOf(base)+chatPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = chatHeaders(token, version)
	return c.clientFor(proxy).Do(req)
}

// modelsURL ports buildLobsteraiModelsUrl + buildLobsteraiModelsQuery: the
// keyfrom identity payload travels as a QUERY STRING, not a body.
func modelsURL(base, token, version string, cred credentials) string {
	// modelsHeaders also needs the bearer, so the credential must be present.
	q := url.Values{}
	// lobsteraiKeyfromBody: firstKeyfrom/latestKeyfrom are the stored login
	// timestamps (frozen at login), then version, then optional uuid/userId.
	if cred.FirstKeyfrom != "" {
		q.Set("firstKeyfrom", cred.FirstKeyfrom)
	}
	if cred.LatestKeyfrom != "" {
		q.Set("latestKeyfrom", cred.LatestKeyfrom)
	}
	if version != "" {
		q.Set("version", version)
	}
	if cred.UUID != "" {
		q.Set("uuid", cred.UUID)
	}
	if cred.UserID != "" {
		q.Set("userId", cred.UserID)
	}
	target := originOf(base) + modelsPath
	if encoded := q.Encode(); encoded != "" {
		target += "?" + encoded
	}
	return target
}

// models fetches the available model list. A failure returns the error so the
// caller can fall back to the static table, matching the reference's "失败时返回
// 空数组（调用方回退到产品兜底目录）".
func (c *upstreamClient) models(ctx context.Context, base, token, version string, cred credentials, proxy string) ([]byte, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, modelsURL(base, token, version, cred), nil)
	if err != nil {
		return nil, err
	}
	req.Header = modelsHeaders(token, version)
	resp, err := c.clientFor(proxy).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("lobsterai: models HTTP %d: %s", resp.StatusCode, truncate(string(data), 256))
	}
	return data, nil
}
