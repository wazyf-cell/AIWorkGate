package main

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// forcedModelRefreshTimeout bounds the synchronous refresh triggered by the
// host's "立即同步" (GET /v1/models?refresh=1). It stays well below the host's
// 60s control-call timeout so the host always receives an answer.
const forcedModelRefreshTimeout = 20 * time.Second

// modelRefreshRequested reports whether the caller asked to bypass the model
// cache (contract v0.3: refresh=1 or refresh=true).
func modelRefreshRequested(r *http.Request) bool {
	v := r.URL.Query().Get("refresh")
	return v == "1" || strings.EqualFold(v, "true")
}

func (p *Plugin) modelsGeneration() uint64 {
	p.modelsMu.RLock()
	defer p.modelsMu.RUnlock()
	return p.modelsGen
}

// refreshModels fetches and persists the remote catalog. Callers may serve the
// cached/fallback list while this runs.
func (p *Plugin) refreshModels(ctx context.Context) error {
	return p.refreshModelsSince(ctx, p.modelsGeneration())
}

// refreshModelsSince refreshes the catalog unless another caller already
// refreshed it after startGen was observed. Fetches are serialized by
// modelsFetchMu, so a burst of forced syncs performs one upstream fetch and the
// waiters reuse its result.
func (p *Plugin) refreshModelsSince(ctx context.Context, startGen uint64) error {
	p.modelsFetchMu.Lock()
	defer p.modelsFetchMu.Unlock()
	if p.modelsGeneration() != startGen {
		return nil
	}

	acct, proxyURL, err := p.pickModelAccount(ctx)
	if err != nil {
		return err
	}
	models, benefit, err := p.fetchRemoteModels(ctx, acct, proxyURL)
	if err != nil {
		return err
	}
	now := p.now().Unix()
	p.modelsMu.Lock()
	p.models = models
	if len(benefit) > 0 {
		p.benefitModels = toBenefitSet(benefit)
	}
	p.modelsFetched = now
	p.modelsGen++
	p.modelsMu.Unlock()
	p.saveModelCache(models, benefit)
	p.logLine("info", "model catalog refreshed", map[string]any{"count": len(models), "benefit": len(benefit)})
	return nil
}

// syncModels refreshes the catalog synchronously for a forced request and
// returns the freshest snapshot. A failed refresh is logged and the cached
// snapshot is served instead.
func (p *Plugin) syncModels(parent context.Context) []ModelInfo {
	startGen := p.modelsGeneration()
	ctx, cancel := context.WithTimeout(parent, forcedModelRefreshTimeout)
	defer cancel()
	if err := p.refreshModelsSince(ctx, startGen); err != nil {
		p.logLine("warn", "forced model refresh failed", map[string]any{"error": err.Error()})
	}
	models, _ := p.modelSnapshot()
	return models
}

func (p *Plugin) handleModels(w http.ResponseWriter, r *http.Request) {
	var models []ModelInfo
	if modelRefreshRequested(r) {
		// A forced sync must see the live catalog; never also spawn the
		// background refresh on this path.
		models = p.syncModels(r.Context())
	} else {
		models, _ = p.modelSnapshot()
		if p.modelsStale(p.now().Unix()) {
			gen := p.modelsGeneration()
			go func() {
				ctx, cancel := context.WithTimeout(p.rootCtx, 45*time.Second)
				defer cancel()
				if err := p.refreshModelsSince(ctx, gen); err != nil {
					p.logLine("warn", "background model refresh failed", map[string]any{"error": err.Error()})
				}
			}()
		}
	}
	data := make([]map[string]any, 0, len(models))
	created := p.startedAt.Unix()
	for _, m := range models {
		obj := map[string]any{
			"id":             m.ID,
			"object":         "model",
			"created":        created,
			"owned_by":       "codearts",
			"name":           m.Name,
			"context_length": m.ContextLength,
			"tools":          m.Tools,
			"vision":         m.Vision,
		}
		if m.Vendor != "" {
			obj["vendor"] = m.Vendor
		}
		if m.Multiplier > 0 {
			obj["multiplier"] = m.Multiplier
		}
		if len(m.Thinking) > 0 {
			obj["thinking"] = m.Thinking
		}
		if m.Free {
			obj["free"] = true
		}
		data = append(data, obj)
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}
