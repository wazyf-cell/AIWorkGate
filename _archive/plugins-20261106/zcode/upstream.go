// Package main — upstream client for zcode.z.ai.
//
// Endpoint and header set ported from the reference implementation
// (workbuddy2api-panel/internal/zcode/upstream). The fingerprint headers are not
// decoration: the reference documents that omitting them (or sending
// x-query-id / x-session-id) makes the upstream answer 3012 "unusual activity".
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// originBase is the ZCode origin. Kept as a package var so tests can point it
	// at a mock server.
	originBase = "https://zcode.z.ai"

	// messagesPath is the plan inference endpoint. It speaks Anthropic Messages,
	// not OpenAI chat — see protocol.go for the conversion.
	messagesPath = "/api/v1/zcode-plan/anthropic/v1/messages"

	// balancePath is the credit endpoint used to validate a desktop JWT.
	balancePath = "/api/v1/zcode-plan/billing/balance"

	// apiKeyOrigin serves the coding-plan api key. It exposes the same Anthropic
	// Messages protocol without the Aliyun captcha the zcode-plan channel asks
	// for, which is why the local-state import prefers this credential.
	apiKeyOrigin = "https://api.z.ai"

	// apiKeyMessagesPath / apiKeyModelsPath are the Anthropic-compatible routes
	// confirmed live on 2026-10-22 (see docs/evidence-zcode.md).
	apiKeyMessagesPath = "/api/anthropic/v1/messages"
	apiKeyModelsPath   = "/api/anthropic/v1/models"

	// appVersion is the client version the upstream fingerprints. The reference
	// verified 3.14.0 against the live service.
	appVersion = "3.14.0"

	// billingVersion identifies as the current desktop for billing calls.
	billingVersion = "3.14.0"

	upstreamTimeout = 5 * time.Minute
)

// upstreamClient issues Anthropic-shaped requests and returns the raw response.
type upstreamClient struct {
	http *http.Client
	// transport resolves an egress proxy URL to a cached transport. It is nil
	// for a standalone client (tests), which always goes direct.
	transport func(string) *http.Transport
}

func newUpstreamClient() *upstreamClient {
	return &upstreamClient{http: &http.Client{
		Timeout: upstreamTimeout,
		// A streaming reply must not be buffered by a proxy; the default
		// transport is fine, but a zero Timeout on the body read is handled by
		// the request context instead.
	}}
}

// messages POSTs an Anthropic body to the fixed origin and returns an unread
// response. The caller owns closing the body.
func (c *upstreamClient) messages(ctx context.Context, jwt, deviceMid string, body []byte, extra http.Header) (*http.Response, error) {
	return c.messagesTo(ctx, originBase, jwt, deviceMid, body, extra)
}

// messagesTo is messages with an explicit origin, so a test (or a mirror) can
// point the client at a mock server without touching the production constant.
func (c *upstreamClient) messagesTo(ctx context.Context, base, jwt, deviceMid string, body []byte, extra http.Header) (*http.Response, error) {
	return c.messagesVia(ctx, base, jwt, deviceMid, body, extra, "")
}

// messagesVia is messagesTo with an explicit egress proxy ("" = direct).
func (c *upstreamClient) messagesVia(ctx context.Context, base, jwt, deviceMid string, body []byte, extra http.Header, proxy string) (*http.Response, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		base = originBase
	}
	return c.post(ctx, base+messagesPath, chatHeaderSet(jwt, appVersion, deviceMid, extra), body, proxy)
}

// apiKeyMessages POSTs the same Anthropic body to api.z.ai with the
// coding-plan key. proxy is the egress forwarder URL ("" = direct).
func (c *upstreamClient) apiKeyMessages(ctx context.Context, base, key string, body []byte, extra http.Header, proxy string) (*http.Response, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		base = apiKeyOrigin
	}
	return c.post(ctx, base+apiKeyMessagesPath, apiKeyHeaderSet(key, extra), body, proxy)
}

// post issues one POST with the given header set through the transport that
// matches the proxy binding.
func (c *upstreamClient) post(ctx context.Context, target string, headers [][2]string, body []byte, proxy string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for _, kv := range headers {
		if kv[1] != "" {
			req.Header.Set(kv[0], kv[1])
		}
	}
	return c.clientFor(proxy).Do(req)
}

// clientFor returns a client bound to one egress route. The proxy state lives on
// the server; a standalone client (tests) uses the default transport.
func (c *upstreamClient) clientFor(proxy string) *http.Client {
	if strings.TrimSpace(proxy) == "" || c.transport == nil {
		return c.http
	}
	tr := c.transport(proxy)
	if tr == nil {
		return c.http
	}
	return &http.Client{Transport: tr, Timeout: upstreamTimeout}
}

// balance fetches the account's credit snapshot.
func (c *upstreamClient) balance(ctx context.Context, jwt, deviceMid string) ([]byte, error) {
	return c.balanceAt(ctx, originBase, jwt, deviceMid, "")
}

func (c *upstreamClient) balanceAt(ctx context.Context, base, jwt, deviceMid, proxy string) ([]byte, error) {
	target := strings.TrimRight(base, "/") + balancePath + "?app_version=" + billingVersion
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	for _, kv := range billingHeaderSet(jwt, deviceMid) {
		if kv[1] != "" {
			req.Header.Set(kv[0], kv[1])
		}
	}
	resp, err := c.clientFor(proxy).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("balance HTTP %d: %s", resp.StatusCode, truncate(string(data), 200))
	}
	return data, nil
}

// validateJWT asks the billing endpoint whether a desktop JWT is real. It is
// the only zcode-plan call that answers without a captcha.
func (c *upstreamClient) validateJWT(ctx context.Context, base, jwt, proxy string) error {
	data, err := c.balanceAt(ctx, base, jwt, "", proxy)
	if err != nil {
		return err
	}
	var wrap struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(data, &wrap); err != nil {
		return fmt.Errorf("balance response is not JSON")
	}
	if wrap.Code != 0 {
		return fmt.Errorf("balance code=%d", wrap.Code)
	}
	return nil
}

// validateAPIKey lists models with the coding-plan key. A 200 with a non-empty
// catalog is the acceptance signal; anything else means the key is not usable.
func (c *upstreamClient) validateAPIKey(ctx context.Context, base, key, proxy string) error {
	data, err := c.fetchModels(ctx, base, key, proxy)
	if err != nil {
		return err
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return fmt.Errorf("models response is not JSON")
	}
	if len(list.Data) == 0 {
		return fmt.Errorf("models list is empty")
	}
	return nil
}

// fetchModels GETs the live Anthropic-compatible catalog.
func (c *upstreamClient) fetchModels(ctx context.Context, base, key, proxy string) ([]byte, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		base = apiKeyOrigin
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+apiKeyModelsPath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("accept", "application/json")
	req.Header.Set("User-Agent", "ZCode/"+appVersion)
	resp, err := c.clientFor(proxy).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("models HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	return body, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
