// Package main — upstream client for Raccoon (商汤).
//
// Everything here is ported from the reference implementation
// (vendor/channel-pack/src/raccoon.ts and raccoon-adapter.ts in
// dsh-our-free-model-2.5.3). Constants carry the reference symbol they came
// from; nothing is invented.
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
	// apiBase is RACCOON_API_BASE (raccoon.ts).
	apiBase = "https://xiaohuanxiong.com"

	// authApiPrefix / llmApiPrefix / pointsApiPrefix / desktopApiPrefix are the
	// four families the reference declares on the product (raccoon.ts).
	authApiPrefix    = "/api/web/auth/v1"
	llmApiPrefix     = "/api/web/llm/v2"
	pointsApiPrefix  = "/api/web/points/v1"
	desktopApiPrefix = "/api/web/desktop/v1"

	// llmChatPath is apiBase + llmApiPrefix + "/chat/completions"
	// (raccoon-adapter.ts: `${this.product.apiBase}${this.product.llmApiPrefix}/chat/completions`).
	llmChatPath = llmApiPrefix + "/chat/completions"

	// llmCatalogPath is apiBase + llmApiPrefix + "/model_catalog" (fetchModels).
	llmCatalogPath = llmApiPrefix + "/model_catalog"

	// userAgent / clientPlatform / clientVersion are the client fingerprint the
	// reference declares (raccoon.ts). The adapter sends the platform on chat.
	userAgent      = "Raccoon Work/1.0.35 (Windows)"
	clientPlatform = "desktop-windows"
	clientVersion  = "v1.0.35"

	// requestTimeout is RACCOON_REQUEST_TIMEOUT_MS = 6e4.
	requestTimeout = 60 * time.Second

	// catalogTimeout is the 2e4 ms the reference gives fetchModels.
	catalogTimeout = 20 * time.Second

	// requestBodyLimitBytes is the upstream's own cap: the reference documents
	// that this gateway meters by request-body BYTES and answers
	// `HTTP_413: request body exceeds 10MB`. Enforcing it locally turns a
	// wasted round trip into an immediate, actionable error.
	requestBodyLimitBytes = 10 << 20
)

// upstreamClient issues Raccoon's OpenAI-shaped requests.
//
// One *http.Client is cached per egress proxy URL, so an account bound to a
// local forwarder (egress.json) really leaves through it and a direct account
// never falls back to the proxy or vice versa.
type upstreamClient struct {
	mu      sync.Mutex
	clients map[string]*http.Client
}

func newUpstreamClient() *upstreamClient {
	return &upstreamClient{clients: map[string]*http.Client{}}
}

// clientFor returns the caching client for proxyURL ("" means direct).
func (c *upstreamClient) clientFor(proxyURL string) *http.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cl, ok := c.clients[proxyURL]; ok {
		return cl
	}
	tr := &http.Transport{
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: requestTimeout,
	}
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	cl := &http.Client{Transport: tr}
	c.clients[proxyURL] = cl
	return cl
}

// resetClients drops every cached transport (POST /reload after egress.json
// changed, so a re-bound account does not keep the old socket).
func (c *upstreamClient) resetClients() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, cl := range c.clients {
		if tr, ok := cl.Transport.(*http.Transport); ok {
			tr.CloseIdleConnections()
		}
		delete(c.clients, k)
	}
}

// chat POSTs an OpenAI chat body and returns an unread response.
//
// The header set is the reference's inline `headers()` closure in the adapter
// (raccoon-adapter.ts), which is slightly smaller than raccoonHeaders():
// it adds X-Client-Platform and omits the device id.
func (c *upstreamClient) chat(ctx context.Context, base string, cred credentials, body []byte, proxyURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		originOf(base)+llmChatPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	// Verbatim from the reference's headers() closure.
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	req.Header.Set("X-Org-Code", cred.OfficeIdentity) // empty for personal accounts
	req.Header.Set("X-Raccoon-Language", "zh")
	req.Header.Set("X-Client-Platform", clientPlatform)
	req.Header.Set("User-Agent", userAgent)
	return c.clientFor(proxyURL).Do(req)
}

// catalog fetches the model catalog.
//
// The reference's fetchModels sends a smaller set than chat does: no
// X-Client-Platform, and it is documented to return [] on any failure so the
// adapter can fall back to its static table.
func (c *upstreamClient) catalog(ctx context.Context, base string, cred credentials, proxyURL string) ([]byte, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, catalogTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet,
		originOf(base)+llmCatalogPath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	req.Header.Set("X-Org-Code", cred.OfficeIdentity)
	req.Header.Set("X-Raccoon-Language", "zh")
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.clientFor(proxyURL).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("raccoon: catalog HTTP %d: %s", resp.StatusCode, truncate(string(data), 256))
	}
	return data, nil
}

func originOf(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return apiBase
	}
	return base
}
