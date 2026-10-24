package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

func (p *Plugin) signedGetText(ctx context.Context, rawURL string, acct *Account, proxyURL string, extraHeaders map[string]string) (string, bool) {
	resp, err := p.signedRequest(ctx, http.MethodGet, rawURL, nil, acct, proxyURL, acct.ID, nil, extraHeaders)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", false
	}
	return string(b), true
}

func extractArray(text string, path ...string) ([]any, bool) {
	var root any
	if err := json.Unmarshal([]byte(text), &root); err != nil {
		return nil, false
	}
	cur := root
	for _, key := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur = obj[key]
	}
	arr, ok := cur.([]any)
	return arr, ok
}

// fetchRemoteModels pulls the two model catalogs with SDK-HMAC signatures.
// Benefit models are those gateway entries whose model_id carries no build
// suffix (matching isCodeArtsBenefitModel in the reference implementation).
func (p *Plugin) fetchRemoteModels(ctx context.Context, acct *Account, proxyURL string) ([]ModelInfo, []string, error) {
	models := make([]ModelInfo, 0, 32)
	seen := map[string]bool{}
	benefit := make([]string, 0, 8)
	fetchedAny := false

	// Match the reference implementation's 10s per-catalog timeout; the
	// overall refresh context can be longer (background work).
	gatewayCtx, cancelGateway := context.WithTimeout(ctx, p.opts.ModelFetchWait)
	gatewayText, gatewayOK := p.signedGetText(gatewayCtx, p.endpoints.GatewayConfig, acct, proxyURL, nil)
	cancelGateway()
	if gatewayOK {
		text := gatewayText
		if arr, ok := extractArray(text, "result", "models"); ok {
			fetchedAny = true
			for _, item := range arr {
				obj, ok := item.(map[string]any)
				if !ok {
					continue
				}
				rawID, _ := obj["model_id"].(string)
				mi := parseRemoteModel(obj, seen)
				if mi == nil {
					continue
				}
				if rawID != "" && normalizeModelID(rawID) == rawID {
					benefit = append(benefit, mi.ID)
				}
				models = append(models, *mi)
			}
		}
	}
	extra := map[string]string{
		"Content-Type": "application/json",
		"Agent-Type":   "PromptCenter",
		"X-Language":   "zh-cn",
	}
	builtinCtx, cancelBuiltin := context.WithTimeout(ctx, p.opts.ModelFetchWait)
	builtinText, builtinOK := p.signedGetText(builtinCtx, p.endpoints.BuiltinModels, acct, proxyURL, extra)
	cancelBuiltin()
	if builtinOK {
		text := builtinText
		if arr, ok := extractArray(text, "builtinModels"); ok {
			fetchedAny = true
			for _, item := range arr {
				obj, ok := item.(map[string]any)
				if !ok {
					continue
				}
				if mi := parseRemoteModel(obj, seen); mi != nil {
					models = append(models, *mi)
				}
			}
		}
	}
	if !fetchedAny || len(models) == 0 {
		return nil, nil, errors.New("model catalog unavailable")
	}
	return models, benefit, nil
}

// pickModelAccount returns the first enabled account with refreshable or fresh
// credentials; it refreshes that account if needed.
func (p *Plugin) pickModelAccount(ctx context.Context) (*Account, string, error) {
	for _, snap := range p.accountSnapshots() {
		if !snap.Enabled {
			continue
		}
		proxyURL, err := p.egressProxyFor(snap.ID)
		if err != nil {
			continue
		}
		acct, err := p.ensureCredential(ctx, snap.ID, false)
		if err != nil || acct == nil {
			continue
		}
		return acct, proxyURL, nil
	}
	return nil, "", errors.New("no usable CodeArts account")
}
