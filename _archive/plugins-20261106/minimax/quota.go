package main

// Credit and quota parsing, kept apart from the HTTP surface in server.go.

import (
	"context"
	"encoding/json"
)

// quotaSnapshot reads the credit balance. Any failure yields an empty object: a
// wrong number is worse than no number, and the reference's own parser treats a
// non-envelope answer as "unknown".
func (s *server) quotaSnapshot(ctx context.Context, creds credentials) map[string]any {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	raw, err := s.client.credits(ctx, creds.AccessToken, creds.BaseURL)
	if err != nil {
		return map[string]any{}
	}
	remaining, ok := parseCreditTotal(raw)
	if !ok {
		return map[string]any{}
	}
	// No total: /credit/details reports remaining_amount only. Inventing
	// total = remaining would read as "100% left" forever, so the host must fall
	// back to its absolute threshold (internal/schedule/quota.go quotaLow).
	return map[string]any{"remaining": remaining, "plan": "credits"}
}

// parseCreditTotal walks the /credit/details envelope the reference reads:
// base_resp.status_code must be 0, the payload may be wrapped in "data", and each
// details[] entry carries remaining_amount.
func parseCreditTotal(raw []byte) (float64, bool) {
	var env struct {
		BaseResp struct {
			StatusCode *float64 `json:"status_code"`
		} `json:"base_resp"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return 0, false
	}
	if env.BaseResp.StatusCode != nil && *env.BaseResp.StatusCode != 0 {
		return 0, false
	}
	body := raw
	if len(env.Data) > 0 && string(env.Data) != "null" {
		body = env.Data
	}
	var data struct {
		Details []struct {
			RemainingAmount json.Number `json:"remaining_amount"`
		} `json:"details"`
	}
	if json.Unmarshal(body, &data) != nil {
		return 0, false
	}
	if data.Details == nil {
		return 0, false
	}
	var total float64
	for _, d := range data.Details {
		if v, err := d.RemainingAmount.Float64(); err == nil && v > 0 {
			total += v
		}
	}
	return total, true
}
