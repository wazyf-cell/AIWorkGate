// Package main — upstream client for Loomy (讯飞).
//
// Everything here is ported from the reference implementation
// (vendor/channel-pack/src/loomy.ts, loomy-adapter.ts, loomy-oauth.ts in
// dsh-our-free-model-2.5.3). No endpoint, header name or body field is invented:
// each carries the reference line it came from, so a reviewer can check it
// against the source.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// apiBase is LOOMY_API_BASE (loomy.ts: LOOMY_API_BASE). This is the origin
	// the chat, models and balance endpoints live under.
	apiBase = "https://loomyad.xunfei.cn/api/v1"

	// chatPath is apiBase + "/chat/completions" (loomy-adapter.ts: the send()
	// helper builds `${this.product.apiBase}/chat/completions`).
	chatPath = "/chat/completions"

	// modelsPath is GET apiBase + "/models" (the registration's
	// fetchRemoteModels: `fetch(`${LOOMY.apiBase}/models`)`).
	modelsPath = "/models"

	// requestTimeout is LOOMY_REQUEST_TIMEOUT_MS = 6e4.
	requestTimeout = 60 * time.Second

	// modelsTimeout is the 3e4 ms the reference gives the catalog call.
	modelsTimeout = 30 * time.Second
)

// upstreamClient issues Loomy's OpenAI-shaped requests.
type upstreamClient struct {
	http *http.Client
}

func newUpstreamClient() *upstreamClient {
	return &upstreamClient{http: &http.Client{
		// No total timeout: a streaming answer can legitimately run for minutes.
		// The request context and ResponseHeaderTimeout bound the dangerous parts.
		Transport: &http.Transport{
			Proxy:                 nil,
			MaxIdleConnsPerHost:   8,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: requestTimeout,
		},
	}}
}

// httpFor returns the client for one egress binding: the shared direct client
// when proxyURL is empty, otherwise a client whose transport dials through that
// proxy. Login traffic (WeChat QR page, long poll, signed exchange) uses it so a
// bound account keeps its source IP during the sign-in as well.
func (c *upstreamClient) httpFor(proxyURL string) *http.Client {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return c.http
	}
	parsed, err := url.Parse(proxyURL)
	if err != nil || parsed.Scheme == "" {
		return c.http
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyURL(parsed),
			MaxIdleConnsPerHost:   8,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: requestTimeout,
		},
	}
}

// chat POSTs an OpenAI chat body to Loomy and returns an unread response. The
// caller owns closing the body.
//
// The reference sends BOTH headers for chat (loomyChatHeaders):
//
//	Accept: text/event-stream
//	Content-Type: application/json
//	Authorization: Bearer <token>
//	token: <token>
//
// with the comment "chat 端点只认 Bearer（业务端点才认 token），这里两个都发".
// Sending only one of them has not been verified against the live service, so
// both are sent exactly as the reference does.
func (c *upstreamClient) chat(ctx context.Context, base, token string, body []byte) (*http.Response, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		base = apiBase
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+chatPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("token", token)
	return c.http.Do(req)
}

// models fetches the remote catalog.
//
// ⚠️ The reference is explicit that this endpoint takes the **token** header and
// NOT a bearer: "必须用 **token 头**（业务端点），不是 Bearer —— 带错会得到
// `100002 缺少 token`". So only the token header is set here. This is a
// deliberate asymmetry with chat(), not an oversight.
func (c *upstreamClient) models(ctx context.Context, base, token string) ([]byte, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		base = apiBase
	}
	fetchCtx, cancel := context.WithTimeout(ctx, modelsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, base+modelsPath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("token", token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("loomy: models HTTP %d: %s", resp.StatusCode, truncate(string(data), 256))
	}
	return data, nil
}

// ---------------------------------------------------------------- envelopes

// loomyOKCode is LOOMY_OK_CODE. Every business envelope carries `code`; only
// "000000" means success (parseLoomyEnvelope).
const loomyOKCode = "000000"

// loomyAuthErrorCode is LOOMY_AUTH_ERROR_CODE = "100002" ("缺少 token").
const loomyAuthErrorCode = "100002"

// envelope mirrors parseLoomyEnvelope: {code, desc|message, data}.
func parseEnvelope(payload []byte) (ok bool, code, message string, data []byte) {
	var env struct {
		Code string          `json:"code"`
		Desc string          `json:"desc"`
		Msg  string          `json:"message"`
		Data json.RawMessage `json:"data"`
	}
	if err := jsonUnmarshalObject(payload, &env); err != nil {
		return false, "", "响应不是 JSON 对象", nil
	}
	message = env.Desc
	if strings.TrimSpace(message) == "" {
		message = env.Msg
	}
	if env.Code != loomyOKCode {
		if strings.TrimSpace(message) == "" {
			message = fmt.Sprintf("业务错误 %s", env.Code)
		}
		return false, env.Code, message, nil
	}
	return true, env.Code, message, env.Data
}

// loomyModel is one entry of the remote catalog, reduced to the fields the host
// understands. Ported from parseLoomyRemoteModels + fallbackModels.
type loomyModel struct {
	ID            string   `json:"id"`
	Name          string   `json:"name,omitempty"`
	ContextLength int      `json:"context_length,omitempty"`
	Tools         bool     `json:"tools,omitempty"`
	Vision        bool     `json:"vision,omitempty"`
	Thinking      []string `json:"thinking,omitempty"`
}

// parseModelsBody converts Loomy's catalog into the host's model list.
//
// The reference accepts either a bare array or {data: [...]}
// (parseLoomyRemoteModels). Only `type == "chat"` entries are chat models, and
// the capabilities block carries the vision flag
// (capabilities.input_modalities contains "image").
func parseModelsBody(body []byte) ([]loomyModel, error) {
	raw := bytes.TrimSpace(body)
	if len(raw) == 0 {
		return nil, nil
	}
	// The catalog may be wrapped in the business envelope or be a bare payload.
	var list []loomyCatalogEntry
	if err := jsonUnmarshalArray(raw, &list); err != nil {
		var env struct {
			Data []loomyCatalogEntry `json:"data"`
		}
		if err := jsonUnmarshalObject(raw, &env); err != nil {
			var wrapped struct {
				Code string `json:"code"`
				Data struct {
					Data []loomyCatalogEntry `json:"data"`
				} `json:"data"`
			}
			if err := jsonUnmarshalObject(raw, &wrapped); err != nil {
				return nil, fmt.Errorf("loomy: catalog is not a model list: %w", err)
			}
			list = wrapped.Data.Data
		} else {
			list = env.Data
		}
	}
	out := make([]loomyModel, 0, len(list))
	for _, e := range list {
		if strings.TrimSpace(e.ID) == "" || e.Type != "chat" {
			continue
		}
		// The published model id must not contain "/" (the host drops those).
		if strings.Contains(e.ID, "/") {
			continue
		}
		name := e.Name
		if strings.TrimSpace(name) == "" {
			name = e.ID
		}
		out = append(out, loomyModel{
			ID:            e.ID,
			Name:          loomyDisplayName(name),
			ContextLength: e.ContextLength,
			Tools:         true,
			Vision:        hasInputModality(e, "image"),
			Thinking:      e.ReasoningEfforts,
		})
	}
	return out, nil
}

// loomyCatalogEntry is one raw catalog row.
type loomyCatalogEntry struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Type             string   `json:"type"`
	ContextLength    int      `json:"context_length"`
	ReasoningEfforts []string `json:"reasoning_efforts"`
	Capabilities     struct {
		Reasoning       bool     `json:"reasoning"`
		InputModalities []string `json:"input_modalities"`
	} `json:"capabilities"`
}

func hasInputModality(e loomyCatalogEntry, want string) bool {
	for _, m := range e.Capabilities.InputModalities {
		if strings.EqualFold(strings.TrimSpace(m), want) {
			return true
		}
	}
	return false
}

// loomyDisplayName keeps the "· xRATE" suffix the reference preserves
// (loomyDisplayName/splitLoomyRate). The rate is display-only.
func loomyDisplayName(raw string) string { return strings.TrimSpace(raw) }

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
