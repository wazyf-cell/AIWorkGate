package main

// The model catalog readers split out of upstream_relay.go to keep every new
// Go file under the 300-line limit from the handoff.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// modelConfigItem is one entry of the global-region model catalog.
type modelConfigItem struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContextWindow any    `json:"contextWindow"`
}

// fetchModelConfig reads the model catalog (global region only).
func (u *upstream) fetchModelConfig(a *account) []modelConfigItem {
	snap := a.snapshot()
	raw, err := u.doJSON(http.MethodGet, stateKey(snap.Region, snap.UserID),
		u.base(snap.Region)+configModelsPath,
		userapiHeaders(snap.AccessToken), nil, false)
	if err != nil {
		return nil
	}
	var data struct {
		Models []modelConfigItem `json:"models"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return nil
	}
	return data.Models
}

// fetchSandboxModels reads the sandbox's own /v1/models listing.
func (u *upstream) fetchSandboxModels(a *account) []string {
	snap := a.snapshot()
	if snap.SandboxEndpoint == "" {
		return nil
	}
	headers := map[string]string{"Accept": "application/json, text/plain, */*"}
	if snap.AccessToken != "" {
		headers["Authorization"] = bearer(snap.AccessToken)
	}
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(snap.SandboxEndpoint, "/")+"/v1/models", nil)
	if err != nil {
		return nil
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := u.do(req, stateKey(snap.Region, snap.UserID))
	if err != nil || resp.StatusCode != 200 {
		if resp != nil {
			resp.Body.Close()
		}
		return nil
	}
	defer resp.Body.Close()
	var data struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&data) != nil {
		return nil
	}
	out := make([]string, 0, len(data.Data))
	for _, m := range data.Data {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	return out
}
