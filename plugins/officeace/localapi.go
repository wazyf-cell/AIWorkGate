package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const defaultLocalAPIURL = "https://127.0.0.1:3004"

// localClient talks to OfficeAce's loopback API. It always bypasses proxies and
// only disables TLS verification for loopback addresses (self-signed cert).
type localClient struct {
	baseURL string
	http    *http.Client
}

func newLocalClient(baseURL string) *localClient {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = defaultLocalAPIURL
	}
	u, err := url.Parse(baseURL)
	insecure := false
	if err == nil && strings.EqualFold(u.Scheme, "https") {
		host := u.Hostname()
		ip := net.ParseIP(host)
		insecure = host == "localhost" || (ip != nil && ip.IsLoopback())
	}
	tr := &http.Transport{
		Proxy:               nil, // loopback must never traverse a proxy
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        4,
		IdleConnTimeout:     60 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: insecure}, //nolint:gosec // loopback-only
	}
	return &localClient{
		baseURL: baseURL,
		http:    &http.Client{Transport: tr, Timeout: 6 * time.Second},
	}
}

type localHTTPError struct {
	Status  int
	Message string
	// RetryAfter carries the Retry-After delay in seconds when the local
	// service sent one (0 = not sent). It lets the check-in path report an
	// exact reset_at instead of guessing a cooldown.
	RetryAfter int64
}

func (e *localHTTPError) Error() string {
	if e.Status == 0 {
		return "OfficeAce 本机服务不可用：" + e.Message
	}
	return fmt.Sprintf("OfficeAce 本机 API 返回 %d：%s", e.Status, e.Message)
}

func (e *localHTTPError) isDown() bool { return e.Status == 0 }

func (l *localClient) do(ctx context.Context, method, path, sessionID string, body []byte) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, l.baseURL+path, reader)
	if err != nil {
		return nil, 0, &localHTTPError{Status: 0, Message: err.Error()}
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if sessionID != "" {
		req.Header.Set("Authorization", "Bearer "+sessionID)
	}
	resp, err := l.http.Do(req)
	if err != nil {
		return nil, 0, &localHTTPError{Status: 0, Message: err.Error()}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, resp.StatusCode, &localHTTPError{Status: resp.StatusCode, Message: "读取响应失败：" + err.Error()}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(data))
		if len(msg) > 500 {
			msg = msg[:500] + "…"
		}
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return data, resp.StatusCode, &localHTTPError{
			Status:     resp.StatusCode,
			Message:    msg,
			RetryAfter: retryAfterSeconds(resp.Header.Get("Retry-After")),
		}
	}
	return data, resp.StatusCode, nil
}

// retryAfterSeconds parses the integer-seconds form of Retry-After. An HTTP
// date or garbage yields 0, which the caller treats as "not provided".
func retryAfterSeconds(value string) int64 {
	seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || seconds <= 0 {
		return 0
	}
	return seconds
}

// unwrapObject tolerates both direct payloads and {data:{...}} envelopes.
func unwrapObject(raw []byte) (map[string]json.RawMessage, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	if data, ok := root["data"]; ok {
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(data, &nested); err == nil && nested != nil {
			return nested, nil
		}
	}
	return root, nil
}
