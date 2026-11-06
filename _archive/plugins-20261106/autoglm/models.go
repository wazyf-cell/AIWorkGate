package main

// Model catalog. The reference serves a static list and enriches it from the
// upstream when an account is healthy (engine.go Models / staticModels).
//
// The static lists are copied verbatim from engine.go so the host shows the same
// model ids the reference advertises even before any account is configured.

import (
	"sync"
	"time"
)

var cnStaticModels = []string{
	"openclaw", "openclaw/default", "glm-5.3-flash", "glm-5.2",
	"zai_auto", "zai_auto-fast", "zai_glm-5.3-flash", "zaicoding_glm-5.3",
	"tdpsk_deepseek-v4-flash-202605", "tdpsk_deepseek-v4-pro-202606",
}

var globalStaticModels = []string{
	"zai_auto", "zai_auto-fast", "zaicoding_glm-5.3",
	"tdpsk_deepseek-v4-flash-202605", "tdpsk_deepseek-v4-pro-202606", "zai_glm-5.3-flash",
}

// createdStamp is the reference's fixed created timestamp for catalog rows.
const createdStamp = 1787000000

// staticModels is the offline catalog.
func staticModels() []map[string]any {
	out := make([]map[string]any, 0, len(cnStaticModels)+len(globalStaticModels))
	for _, m := range cnStaticModels {
		out = append(out, map[string]any{
			"id": m, "object": "model", "created": createdStamp,
			"owned_by": "autoglm", "context_length": 1000000, "description": "cn",
			"name": m + " (cn)", "tools": true,
		})
	}
	for _, m := range globalStaticModels {
		out = append(out, map[string]any{
			"id": "global/" + m, "object": "model", "created": createdStamp,
			"owned_by": "autoglm", "context_length": 1000000, "description": "global",
			"name": m + " (global)", "tools": true,
		})
	}
	return out
}

// modelCache caches the live catalog for an hour, and remembers a failure for
// five minutes so a dead upstream is not retried on every page load.
type modelCache struct {
	mu     sync.Mutex
	list   []map[string]any
	at     time.Time
	failAt time.Time
}

func (c *modelCache) current(e *engine) []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if len(c.list) > 0 && now.Sub(c.at) < time.Hour {
		return c.list
	}
	if !c.failAt.IsZero() && now.Sub(c.failAt) < 5*time.Minute {
		if len(c.list) > 0 {
			return c.list
		}
		return staticModels()
	}
	out := e.liveModels()
	if len(out) > 0 {
		c.list, c.at, c.failAt = out, now, time.Time{}
		return out
	}
	c.failAt = now
	if len(c.list) > 0 {
		return c.list
	}
	return staticModels()
}

// list is the handler-facing accessor.
func (c *modelCache) modelsFor(e *engine) []map[string]any { return c.current(e) }

// liveModels asks the upstream for its catalog, per region.
func (e *engine) liveModels() []map[string]any {
	var out []map[string]any
	for _, region := range []string{regionCN, regionGlobal} {
		a := e.pickFor(region, nil)
		if a == nil {
			continue
		}
		if e.prepare(a) != nil {
			continue
		}
		if region == regionGlobal {
			for _, it := range e.up.fetchModelConfig(a) {
				if it.ID == "" {
					continue
				}
				cw := int64(1000000)
				if v := toInt64(it.ContextWindow); v > 0 {
					cw = v
				}
				out = append(out, map[string]any{
					"id": "global/" + it.ID, "object": "model", "created": createdStamp,
					"owned_by": "autoglm", "context_length": cw,
					"description": it.Name + " (global)", "name": it.Name,
				})
			}
		}
		for _, mid := range e.up.fetchSandboxModels(a) {
			id := mid
			if region == regionGlobal {
				id = "global/" + mid
			}
			out = append(out, map[string]any{
				"id": id, "object": "model", "created": createdStamp,
				"owned_by": "autoglm", "context_length": 1000000, "name": mid,
			})
		}
	}
	return out
}
