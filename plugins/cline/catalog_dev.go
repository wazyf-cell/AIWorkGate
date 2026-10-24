package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// modelsDevURL is the shared capability catalog. Cline's own endpoints never
// report a context window, so this is the only place those numbers come from.
const modelsDevURL = "https://models.dev/api.json"

// modelsDevTimeout bounds the whole download (CLINE_MODELS_DEV_TIMEOUT_MS).
const modelsDevTimeout = 20 * time.Second

// modelsDevClient is a separate client so a hung models.dev call cannot occupy the
// gateway's connection pool.
var modelsDevClient = &http.Client{
	Timeout: modelsDevTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// fetchModelsDev downloads the raw catalog. base carries the account's egress
// route, so a proxied account does not leak a direct models.dev connection.
func fetchModelsDev(base *http.Client) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, modelsDevURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	client := modelsDevClient
	if base != nil {
		clone := *base
		clone.Timeout = modelsDevTimeout
		client = &clone
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("models.dev HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
}

// parseClineModelsDev projects the `cline-pass` block into ModelInfo rows.
//
// The block may sit at the root or under "providers"; the reference accepts both
// (parseClineModelsDev).
func parseClineModelsDev(body []byte) map[string]ModelInfo {
	out := map[string]ModelInfo{}
	var root map[string]any
	if json.Unmarshal(body, &root) != nil {
		return out
	}
	block := root
	if providers, ok := root["providers"].(map[string]any); ok {
		block = providers
	}
	entry, ok := block["cline-pass"].(map[string]any)
	if !ok {
		return out
	}
	models, ok := entry["models"].(map[string]any)
	if !ok {
		return out
	}
	for rawID, rawEntry := range models {
		if strings.TrimSpace(rawID) == "" {
			continue
		}
		record, ok := rawEntry.(map[string]any)
		if !ok {
			continue
		}
		id := rawID
		if !strings.HasPrefix(id, "cline-pass/") {
			id = "cline-pass/" + id
		}
		info := ModelInfo{ID: id, Name: stringField(record, "name")}
		if limit, ok := record["limit"].(map[string]any); ok {
			if context, ok := limit["context"].(float64); ok && context > 0 {
				info.ContextLength = int(context)
			}
		}
		if modalities, ok := record["modalities"].(map[string]any); ok {
			if inputs, ok := modalities["input"].([]any); ok {
				for _, item := range inputs {
					if s, ok := item.(string); ok && strings.EqualFold(s, "image") {
						info.SupportsImage = true
					}
				}
			}
		}
		out[id] = info
	}
	return out
}
