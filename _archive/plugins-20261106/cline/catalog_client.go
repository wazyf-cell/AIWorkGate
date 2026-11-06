package main

// The catalog fetchers and the small decoding helpers the upstream calls share.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// recommendedModels fetches the free/recommended model catalog (anonymous).
func (c *upstreamClient) recommendedModels() (*recommendedModels, error) {
	resp, err := c.getJSON(c.api+recommendedModelsPath, clientHeadersOnly())
	if err != nil {
		return nil, err
	}
	return parseRecommendedModels(resp), nil
}

// modelIDs fetches the authenticated model id list.
func (c *upstreamClient) modelIDs(accessToken string) ([]string, error) {
	resp, err := c.getJSON(c.api+modelsPath, authenticateHeaders(accessToken))
	if err != nil {
		return nil, err
	}
	return parseModelIDs(resp), nil
}

func (c *upstreamClient) getJSON(target string, headers [][2]string) ([]byte, error) {
	base, err := c.client()
	if err != nil {
		return nil, err
	}
	client := *base
	client.Timeout = modelListTimeout
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	applyHeaders(req, headers)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("cline: HTTP %d: %s", resp.StatusCode, truncate(string(data), 256))
	}
	return data, nil
}

// decodeMap reads a bounded JSON object.
func decodeMap(r io.Reader) (map[string]any, error) {
	data, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if len(strings.TrimSpace(string(data))) == 0 {
		return map[string]any{}, nil
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func stringField(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// secondsOr reads a seconds field into a Duration, falling back when absent.
func secondsOr(m map[string]any, key string, fallback time.Duration) time.Duration {
	if v, ok := m[key].(float64); ok && v > 0 {
		return time.Duration(v) * time.Second
	}
	return fallback
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
