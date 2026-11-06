package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// clientVersion resolves the desktop version, cached for 12h.
//
// Ported from LobsteraiClientVersionResolver.resolve: cache -> remote update
// endpoint -> fallback constant. A failure is not an error; the fallback is
// returned so the models query still has a plausible identity payload.
func (c *upstreamClient) clientVersion(ctx context.Context, proxy string) string {
	c.versionMu.Lock()
	if c.version != "" && time.Since(c.versionAt) < versionCacheTTL {
		v := c.version
		c.versionMu.Unlock()
		return v
	}
	c.versionMu.Unlock()

	fetched := fallbackClientVersion
	defer func() {
		c.versionMu.Lock()
		c.version = fetched
		c.versionAt = time.Now()
		c.versionMu.Unlock()
	}()

	fetchCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, clientVersionAPI, nil)
	if err != nil {
		return fetched
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.clientFor(proxy).Do(req)
	if err != nil {
		return fetched
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fetched
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fetched
	}
	if v := parseClientVersionFromUpdate(body); v != "" {
		fetched = v
	}
	return fetched
}

// parseClientVersionFromUpdate ports the reference's parser: {data:{value:{version}}}.
func parseClientVersionFromUpdate(body []byte) string {
	var payload struct {
		Data struct {
			Value struct {
				Version string `json:"version"`
			} `json:"value"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	return parseClientVersion(payload.Data.Value.Version)
}

// parseClientVersion is parseClientVersion: a version must look like 1.2.3...
// optionally with a -suffix, otherwise it is discarded.
func parseClientVersion(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// Deliberately simple: leading digits and dots, optional -suffix.
	main, _, _ := strings.Cut(raw, "-")
	if main == "" {
		return ""
	}
	for _, r := range main {
		if (r < '0' || r > '9') && r != '.' {
			return ""
		}
	}
	if main[0] < '0' || main[0] > '9' {
		return ""
	}
	return raw
}
