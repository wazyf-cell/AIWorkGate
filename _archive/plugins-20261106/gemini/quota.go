package main

// Quota parsing for /accounts: the retrieveUserQuotaSummary response shape.
// Kept apart from the HTTP surface in server.go.

import (
	"context"
	"encoding/json"
	"time"
)

// quotaSnapshot reads retrieveUserQuotaSummary, caching per token for 60 s exactly
// as fetchGeminiCreditBalance does. The five-hour and weekly buckets are averaged
// into one remaining percentage, matching toGeminiCreditBalance.
func (s *server) quotaSnapshot(ctx context.Context, creds credentials) map[string]any {
	key := creds.AccessToken
	s.quotaMu.Lock()
	if cached, ok := s.quotaCache[key]; ok && time.Since(cached.at) < quotaCacheTTL {
		s.quotaMu.Unlock()
		return cached.result
	}
	s.quotaMu.Unlock()

	project := projectOr(creds.CloudaicompanionProject)
	endpoint := firstNonEmpty(creds.EndpointBase, endpointSandbox)
	raw, err := s.client.quota(ctx, creds.AccessToken, defaultAccountID, endpoint, project)
	if err != nil {
		return map[string]any{}
	}
	result, ok := parseQuotaSummary(raw)
	if !ok {
		return map[string]any{}
	}
	s.quotaMu.Lock()
	s.quotaCache[key] = quotaEntry{at: time.Now(), result: result}
	s.quotaMu.Unlock()
	return result
}

// parseQuotaSummary walks the groups[].buckets[] shape the reference reads. The
// two bucket ids are gemini-5h and gemini-weekly.
func parseQuotaSummary(raw []byte) (map[string]any, bool) {
	var payload struct {
		Groups []struct {
			Buckets []struct {
				BucketID          string  `json:"bucketId"`
				RemainingFraction float64 `json:"remainingFraction"`
				ResetTime         string  `json:"resetTime"`
			} `json:"buckets"`
		} `json:"groups"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return nil, false
	}
	var fractions []float64
	var resetAt int64
	for _, g := range payload.Groups {
		for _, b := range g.Buckets {
			if b.BucketID != "gemini-5h" && b.BucketID != "gemini-weekly" {
				continue
			}
			fractions = append(fractions, b.RemainingFraction)
			if t, err := time.Parse(time.RFC3339, b.ResetTime); err == nil {
				resetAt = t.Unix()
			}
		}
	}
	if len(fractions) == 0 {
		return nil, false
	}
	var sum float64
	for _, f := range fractions {
		sum += f
	}
	remaining := sum / float64(len(fractions)) * 100
	out := map[string]any{"remaining": remaining, "total": 100, "plan": "Gemini"}
	if resetAt > 0 {
		out["reset_at"] = resetAt
	}
	return out, true
}
