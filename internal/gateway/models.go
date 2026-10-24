package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"aiworkgate/internal/model"
	"aiworkgate/internal/scheduler"
	"aiworkgate/internal/store"
)

// modelEntry is one item of GET /v1/models. It carries the plain OpenAI fields
// plus the optional manifest metadata.
type modelEntry struct {
	ID            string `json:"id"`
	Object        string `json:"object"`
	Created       int64  `json:"created"`
	OwnedBy       string `json:"owned_by,omitempty"`
	Name          string `json:"name,omitempty"`
	ContextLength int    `json:"context_length,omitempty"`
	Tools         bool   `json:"tools,omitempty"`
	Vision        bool   `json:"vision,omitempty"`
	// Catalog metadata mirrored from the plugin/A report (contract v0.3). The
	// multiplier is a pointer so "not published" (absent) stays distinguishable
	// from a real 0.0 rate, which Free already covers.
	Vendor     string   `json:"vendor,omitempty"`
	Multiplier *float64 `json:"multiplier,omitempty"`
	Thinking   []string `json:"thinking,omitempty"`
	Free       bool     `json:"free,omitempty"`
	// Overridden lists the fields taken from the human override layer, so a
	// client can tell a hand-edited value from the plugin's own report.
	Overridden []string `json:"overridden,omitempty"`
}

// CanonicalModelID is the comparison key for model ids: surrounding whitespace
// is dropped and the rest is lower-cased. A's unified catalog folds
// case/whitespace variants of one id into a single entry
// (unified_catalog.rs:37,459,490, MIT, 朱天伟); the published /v1/models id
// keeps the spelling of the first source that reported it.
//
// It is exported so the client-key scope admission in keys_scope.go compares
// the same way instead of requiring an exact string match.
func CanonicalModelID(id string) string {
	return strings.ToLower(strings.TrimSpace(id))
}

// catalogMultiplier exposes a published rate; 0 means "not published".
func catalogMultiplier(v float64) *float64 {
	if v <= 0 {
		return nil
	}
	return &v
}

type modelsList struct {
	Object string       `json:"object"`
	Data   []modelEntry `json:"data"`
}

// handleModels merges plugin models (prefixed) with A's bare models. A failure
// is not fatal: the last cache is served with X-AIWorkGate-A-Stale: 1.
func (g *Gateway) handleModels(w http.ResponseWriter, r *http.Request, rec *reqRecord) {
	// The list is merged by the gateway (plugins + bindings + A + custom), so
	// the request log must not attribute it to A alone.
	rec.source = "gateway"
	entries := make([]modelEntry, 0, 32)
	// Keyed by CanonicalModelID, so "Kimi-K2" reported twice (different case or
	// padding) stays one entry. The first spelling is the one published.
	seen := make(map[string]bool, 32)

	// One snapshot per request: the binding list and the per-pool "接入"
	// filter both come from it.
	var snap *scheduler.Snapshot
	if g.snapshot != nil {
		snap, _ = g.snapshot(r.Context())
	}
	exposed := make(map[string]map[string]bool, 8)
	if snap != nil {
		for id, pool := range snap.Pools {
			if pool == nil {
				continue
			}
			// 接入 off means the pool accepts no upstream traffic, so none of
			// its models belong in the catalog. An empty set filters them all.
			if !pool.UpstreamEnabled {
				exposed[id] = map[string]bool{}
				continue
			}
			if len(pool.Models) == 0 {
				continue
			}
			set := make(map[string]bool, len(pool.Models))
			for modelID := range pool.Models {
				set[CanonicalModelID(modelID)] = true
			}
			exposed[id] = set
		}
	}

	if g.plugins != nil {
		for _, st := range g.plugins.List() {
			if !st.Enabled {
				continue
			}
			id := st.Manifest.ID
			if id == "" {
				continue
			}
			mods, _ := g.pluginModelList(r.Context(), st)
			overrides := g.modelOverridesFor(id)
			for _, m := range mods {
				if set, ok := exposed[id]; ok && !set[CanonicalModelID(m.ID)] {
					continue
				}
				full := id + "/" + m.ID
				key := CanonicalModelID(full)
				if seen[key] {
					continue
				}
				seen[key] = true
				// Ported from A's L1 meta layer (unified_catalog.rs:165-210,
				// 332-381, MIT, 朱天伟): the human override wins over the
				// plugin report, and only the fields the user actually set are
				// replaced. A catalog sync never touches the override table,
				// so this merge is the single place where the two layers meet.
				var overridden []string
				if o, ok := overrideForModel(overrides, m.ID); ok {
					m = o.ApplyInfo(m)
					overridden = o.Fields()
				}
				name := m.Name
				entries = append(entries, modelEntry{
					ID:            full,
					Object:        "model",
					OwnedBy:       "plugin:" + id,
					Name:          name,
					ContextLength: m.ContextLength,
					Tools:         m.Tools,
					Vision:        m.Vision,
					Vendor:        m.Vendor,
					Multiplier:    catalogMultiplier(m.Multiplier),
					Thinking:      m.Thinking,
					Free:          m.Free,
					Overridden:    overridden,
				})
			}
		}
	}

	// Logical model bindings are first-class model ids (design §5/§6.1).
	if snap != nil {
		names := make([]string, 0, len(snap.Bindings))
		for name := range snap.Bindings {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			key := CanonicalModelID(name)
			if name == "" || seen[key] {
				continue
			}
			seen[key] = true
			entries = append(entries, modelEntry{
				ID:      name,
				Object:  "model",
				OwnedBy: "scheduler",
				Name:    snap.Bindings[name].Display,
			})
		}
	}

	aModels, stale := g.aModelList(r.Context())
	for _, m := range aModels {
		key := CanonicalModelID(m.ID)
		if m.ID == "" || seen[key] {
			continue
		}
		seen[key] = true
		entries = append(entries, modelEntry{
			ID:            m.ID,
			Object:        "model",
			OwnedBy:       "a",
			Name:          m.Name,
			ContextLength: m.ContextLength,
			Tools:         m.Tools,
			Vision:        m.Vision,
			Vendor:        m.Vendor,
			Multiplier:    catalogMultiplier(m.Multiplier),
			Thinking:      m.Thinking,
			Free:          m.Free,
		})
	}

	payload, err := json.Marshal(modelsList{Object: "list", Data: entries})
	if err != nil {
		g.fail(w, rec, http.StatusInternalServerError, "internal_error", "internal_error",
			"failed to encode model list")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set(sourceHeader, "a")
	if stale {
		h.Set(staleHeader, "1")
	}
	rec.status = http.StatusOK
	if g.debugBody.Load() {
		rec.bodyResp = string(payload)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
}

// ModelOverride aliases the store row so Options stays free of a concrete
// store dependency.
type ModelOverride = store.ModelOverride

// modelOverridesFor returns the human override layer of one plugin keyed by
// model id. It is read on every list so an edit saved in the resource page is
// visible immediately. Errors degrade to "no overrides": a missing database
// must never break the model list.
func (g *Gateway) modelOverridesFor(pluginID string) map[string]ModelOverride {
	if g.overrides == nil || pluginID == "" {
		return nil
	}
	overrides, err := g.overrides(pluginID)
	if err != nil {
		return nil
	}
	return overrides
}

// overrideForModel returns the human override of one model. Overrides are
// keyed by the id the catalog stored, which is the plugin's own spelling; a
// case/whitespace variant of the same id still matches, mirroring the canonical
// merge used for the published list.
func overrideForModel(overrides map[string]ModelOverride, modelID string) (ModelOverride, bool) {
	if len(overrides) == 0 {
		return ModelOverride{}, false
	}
	if o, ok := overrides[modelID]; ok {
		return o, true
	}
	key := CanonicalModelID(modelID)
	for id, o := range overrides {
		if CanonicalModelID(id) == key {
			return o, true
		}
	}
	return ModelOverride{}, false
}

// aModelList returns A's models, fetching at most once per TTL. The second
// result reports whether the list is stale (A unreachable, cache reused).
func (g *Gateway) aModelList(ctx context.Context) ([]model.ModelInfo, bool) {
	if g.a == nil {
		return g.cachedAModels(), true
	}

	g.aMu.Lock()
	fresh := g.aHave && g.now().Sub(g.aFetched) < g.cacheTTL
	cached := append([]model.ModelInfo(nil), g.aModels...)
	g.aMu.Unlock()
	if fresh {
		return cached, false
	}

	fetchCtx, cancel := context.WithTimeout(ctx, modelsFetchTimeout)
	defer cancel()
	models, err := g.a.Models(fetchCtx)
	if err != nil {
		// Re-read the cache: a concurrent request may have refreshed it.
		return g.cachedAModels(), true
	}
	clean := make([]model.ModelInfo, 0, len(models))
	for _, m := range models {
		if strings.TrimSpace(m.ID) == "" {
			continue
		}
		clean = append(clean, m)
	}

	g.aMu.Lock()
	g.aModels = clean
	g.aFetched = g.now()
	g.aHave = true
	g.aMu.Unlock()
	return clean, false
}

func (g *Gateway) cachedAModels() []model.ModelInfo {
	g.aMu.Lock()
	defer g.aMu.Unlock()
	return append([]model.ModelInfo(nil), g.aModels...)
}

// InvalidateAModelCache drops the cached A model list so the next /v1/models
// refetches. The app calls it when A transitions from down to reachable; the
// previous list stays available for the stale-response path.
func (g *Gateway) InvalidateAModelCache() {
	if g == nil {
		return
	}
	g.aMu.Lock()
	g.aHave = false
	g.aMu.Unlock()
}

// pluginModelList returns a plugin's local models. Manifest models win; when
// the manifest lists none, the plugin's /v1/models is queried (cached for the
// model cache TTL). known=false means the registry could not be determined.
func (g *Gateway) pluginModelList(ctx context.Context, st model.PluginStatus) ([]model.ModelInfo, bool) {
	id := st.Manifest.ID
	if local := validLocalModels(st.Manifest.Models); len(local) > 0 {
		return local, true
	}
	if id == "" {
		return nil, false
	}

	g.pMu.Lock()
	if c, ok := g.pCache[id]; ok && g.now().Before(c.until) {
		g.pMu.Unlock()
		return append([]model.ModelInfo(nil), c.models...), c.known
	}
	g.pMu.Unlock()

	models, known := g.fetchPluginModels(ctx, id)

	ttl := g.cacheTTL
	if !known {
		ttl = 30 * time.Second
	}
	g.pMu.Lock()
	g.pCache[id] = pluginModels{
		models: append([]model.ModelInfo(nil), models...),
		known:  known,
		until:  g.now().Add(ttl),
	}
	g.pMu.Unlock()
	return models, known
}

func (g *Gateway) fetchPluginModels(ctx context.Context, id string) ([]model.ModelInfo, bool) {
	if g.plugins == nil {
		return nil, false
	}
	baseURL, token, ok := g.plugins.Endpoint(id)
	if !ok || strings.TrimSpace(baseURL) == "" {
		return nil, false
	}

	fetchCtx, cancel := context.WithTimeout(ctx, modelsFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet,
		strings.TrimRight(baseURL, "/")+"/v1/models", nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, false
	}
	return parseModelsBody(body), true
}

// parseModelsBody accepts the OpenAI shape {"data":[...]}, the host shape
// {"models":[...]}, or a bare array. It is used for a plugin's own /v1/models,
// where ids may legitimately contain "/" (cline reports google/gemma-4-31b-it).
// Those are not dropped: the public id becomes <plugin>/<physical>, and the
// plugin prefix is split at the first slash, so a namespaced physical id stays
// routable. Dropping them silently reduced a 488-model catalog to zero.
func parseModelsBody(body []byte) []model.ModelInfo {
	// One parser for every caller. This function used to be a hand-rolled copy
	// that understood display/rate/supports_image but missed supported_efforts,
	// while the plugin client understood none of the aliases — the same upstream
	// response therefore lost different fields depending on which path decoded
	// it. model.ParseModelList accepts every spelling we have seen in practice.
	// A body that fails to decode yields no usable catalog; the caller keeps the
	// stale cache and reports staleness, which is its existing behaviour.
	out, err := model.ParseModelList(body)
	if err != nil {
		return nil
	}
	kept := out[:0]
	for _, m := range out {
		if m.ID == "" {
			continue
		}
		kept = append(kept, m)
	}
	return kept
}

// validLocalModels filters manifest models to routable local ids.
func validLocalModels(in []model.ModelInfo) []model.ModelInfo {
	out := make([]model.ModelInfo, 0, len(in))
	for _, m := range in {
		id := strings.TrimSpace(m.ID)
		if id == "" || strings.Contains(id, "/") {
			continue
		}
		m.ID = id
		out = append(out, m)
	}
	return out
}

// availableModels returns up to 20 model names for the unknown-model 404
// message: enabled plugin models plus the cached A list (no network call).
// exposed, when non-nil, maps a plugin id to the models the resource page still
// exposes; plugins absent from the map have no catalog filter (same rule as
// GET /v1/models).
func (g *Gateway) availableModels(exposed map[string]map[string]bool) []string {
	out := make([]string, 0, 20)
	if g.plugins != nil {
		for _, st := range g.plugins.List() {
			if !st.Enabled {
				continue
			}
			id := st.Manifest.ID
			if id == "" {
				continue
			}
			set, filtered := exposed[id]
			mods := validLocalModels(st.Manifest.Models)
			if len(mods) == 0 {
				g.pMu.Lock()
				c, ok := g.pCache[id]
				g.pMu.Unlock()
				if ok {
					mods = validLocalModels(c.models)
				}
			}
			for _, m := range mods {
				if filtered && !set[CanonicalModelID(m.ID)] {
					continue
				}
				out = append(out, id+"/"+m.ID)
				if len(out) >= 20 {
					return out
				}
			}
		}
	}
	for _, m := range g.cachedAModels() {
		if m.ID == "" {
			continue
		}
		out = append(out, m.ID)
		if len(out) >= 20 {
			break
		}
	}
	return out
}

// writeUnknownModel sends the 404 required for unknown plugin models, listing
// what is available. The optional exposed map is supplied when the caller
// already knows the pool's catalog filter (the switch rejected the model).
func (g *Gateway) writeUnknownModel(w http.ResponseWriter, rec *reqRecord, name, pluginID string, exposed ...map[string]map[string]bool) {
	var filter map[string]map[string]bool
	if len(exposed) > 0 {
		filter = exposed[0]
	}
	msg := fmt.Sprintf("unknown model %q", name)
	if pluginID != "" {
		msg += " for plugin " + pluginID
	}
	avail := g.availableModels(filter)
	if len(avail) > 0 {
		msg += "; available models: " + strings.Join(avail, ", ")
	}
	g.fail(w, rec, http.StatusNotFound, "invalid_request_error", "model_not_found", msg)
}
