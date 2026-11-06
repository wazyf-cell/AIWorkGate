package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestModelNormalizationAndCache(t *testing.T) {
	gatewayCalls, builtinCalls := 0, 0
	gateway := func(w http.ResponseWriter, r *http.Request) {
		gatewayCalls++
		if r.Header.Get("Authorization") == "" || r.Header.Get("x-security-token") == "" {
			t.Errorf("gateway request is not signed: %v", r.Header)
		}
		writeJSON(w, http.StatusOK, map[string]any{"result": map[string]any{"models": []any{
			map[string]any{"model_id": "GLM-5.2-0001", "model_name": "GLM 5.2"},
			map[string]any{"model_id": "GLM-5.2", "model_name": "GLM dupe"},
			map[string]any{"model_id": "qwen3-VL-0001", "model_name": "Qwen VL"},
			map[string]any{"model_id": "glm-5.3-flash", "model_name": "GLM Flash"},
			map[string]any{"model_id": "deepseek-v4-flash-0009", "model_name": "DS Flash"},
		}}})
	}
	builtin := func(w http.ResponseWriter, r *http.Request) {
		builtinCalls++
		if r.Header.Get("Agent-Type") != "PromptCenter" || r.Header.Get("X-Language") != "zh-cn" {
			t.Errorf("builtin headers = %v", r.Header)
		}
		writeJSON(w, http.StatusOK, map[string]any{"builtinModels": []any{
			map[string]any{"model_id": "openpangu-2.0-flash", "model_name": "OpenPangu"},
			map[string]any{"model_id": "GLM-5.2", "model_name": "GLM dupe"},
		}})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/gateway", gateway)
	mux.HandleFunc("/builtin", builtin)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ep := testEndpoints()
	ep.GatewayConfig = srv.URL + "/gateway"
	ep.BuiltinModels = srv.URL + "/builtin"
	clock := newFakeClock()
	dir := t.TempDir()
	p, _ := newTestPlugin(t, func(o *Options) {
		o.Endpoints = ep
		o.Now = clock.Now
		o.DataDir = dir
	})
	seedAccount(t, p, "alice")

	if err := p.refreshModels(context.Background()); err != nil {
		t.Fatalf("refresh models: %v", err)
	}
	models, benefit := p.modelSnapshot()
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	want := "GLM-5.2,glm-5.3-flash,deepseek-v4-flash,openpangu-2.0-flash"
	if strings.Join(ids, ",") != want {
		t.Fatalf("models = %v, want %s", ids, want)
	}
	if gatewayCalls != 1 || builtinCalls != 1 {
		t.Fatalf("catalog calls = %d/%d", gatewayCalls, builtinCalls)
	}
	ctx := map[string]int{}
	for _, m := range models {
		ctx[m.ID] = m.ContextLength
	}
	if ctx["deepseek-v4-flash"] != 1048576 || ctx["GLM-5.2"] != 202752 {
		t.Fatalf("context windows = %v", ctx)
	}
	if !benefit["glm-5.3-flash"] || benefit["GLM-5.2"] {
		t.Fatalf("benefit set = %v", benefit)
	}
	if !p.isBenefitModel("glm-5.3-flash") || p.isBenefitModel("GLM-5.2") || !p.isBenefitModel("deepseek-v4.1-flash") {
		t.Fatal("benefit classification mismatch")
	}

	cachePath := filepath.Join(dir, modelsCacheFileName)
	raw, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	if !strings.Contains(string(raw), "glm-5.3-flash") {
		t.Fatalf("cache content = %s", raw)
	}

	p2, _ := newTestPlugin(t, func(o *Options) {
		o.Endpoints = ep
		o.Now = clock.Now
		o.DataDir = dir
	})
	if p2.modelsStale(clock.Now().Unix()) {
		t.Fatal("fresh disk cache should not be stale")
	}
	if models2, _ := p2.modelSnapshot(); len(models2) != 4 {
		t.Fatalf("cached models = %+v", models2)
	}
	clock.Advance(7 * time.Hour)
	if !p2.modelsStale(clock.Now().Unix()) {
		t.Fatal("cache older than six hours must be stale")
	}
}

func TestModelsCacheFallbackWhenRefreshFails(t *testing.T) {
	ep := testEndpoints()
	dir := t.TempDir()
	cache := modelCacheFile{
		FetchedAt: 1,
		Models:    []ModelInfo{{ID: "cached-model", Name: "Cached", Tools: true}},
	}
	b, _ := json.Marshal(cache)
	if err := os.WriteFile(filepath.Join(dir, modelsCacheFileName), b, 0o600); err != nil {
		t.Fatalf("write cache: %v", err)
	}
	p, h := newTestPlugin(t, func(o *Options) {
		o.Endpoints = ep
		o.DataDir = dir
	})
	if models, _ := p.modelSnapshot(); len(models) != 1 || models[0].ID != "cached-model" {
		t.Fatalf("startup models = %+v", models)
	}
	rec := doReq(t, h, http.MethodGet, "/v1/models", reqOpts{})
	if rec.Code != http.StatusOK {
		t.Fatalf("models status = %d", rec.Code)
	}
	resp := decodeAs[modelsResp](t, rec)
	if len(resp.Data) != 1 || resp.Data[0].ID != "cached-model" {
		t.Fatalf("served models = %+v", resp.Data)
	}
	if err := p.refreshModels(context.Background()); err == nil {
		t.Fatal("refresh against unreachable endpoints should fail")
	}
	if !p.modelsStale(time.Now().Unix()) {
		t.Fatal("cache with fetched_at=1 must be stale")
	}
	if models, _ := p.modelSnapshot(); len(models) != 1 {
		t.Fatal("failed refresh must keep the cached models")
	}
}

// writeModelCache seeds models-cache.json with a snapshot fetched at fetchedAt.
func writeModelCache(t *testing.T, dir string, fetchedAt int64, ids ...string) {
	t.Helper()
	models := make([]ModelInfo, 0, len(ids))
	for _, id := range ids {
		models = append(models, ModelInfo{ID: id, Name: id, Tools: true})
	}
	b, err := json.Marshal(modelCacheFile{FetchedAt: fetchedAt, Models: models})
	if err != nil {
		t.Fatalf("marshal cache: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, modelsCacheFileName), b, 0o600); err != nil {
		t.Fatalf("write cache: %v", err)
	}
}

// The host's "立即同步" calls GET /v1/models?refresh=1: it must bypass a fresh
// cache and fetch the catalog synchronously.
func TestForcedModelRefreshBypassesFreshCache(t *testing.T) {
	for _, query := range []string{"1", "TRUE"} {
		t.Run("refresh="+query, func(t *testing.T) {
			var gatewayCalls int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/builtin" {
					writeJSON(w, http.StatusOK, map[string]any{"builtinModels": []any{}})
					return
				}
				atomic.AddInt32(&gatewayCalls, 1)
				writeJSON(w, http.StatusOK, map[string]any{"result": map[string]any{"models": []any{
					map[string]any{"model_id": "fresh-model", "model_name": "Fresh"},
				}}})
			}))
			defer srv.Close()

			ep := testEndpoints()
			ep.GatewayConfig = srv.URL + "/gateway"
			ep.BuiltinModels = srv.URL + "/builtin"
			clock := newFakeClock()
			dir := t.TempDir()
			writeModelCache(t, dir, clock.Now().Unix(), "cached-model")
			p, h := newTestPlugin(t, func(o *Options) {
				o.Endpoints = ep
				o.Now = clock.Now
				o.DataDir = dir
			})
			seedAccount(t, p, "alice")
			if p.modelsStale(clock.Now().Unix()) {
				t.Fatal("seeded cache should be fresh")
			}

			// A plain request must keep serving the fresh cache with no fetch.
			rec := doReq(t, h, http.MethodGet, "/v1/models", reqOpts{})
			if rec.Code != http.StatusOK {
				t.Fatalf("models status = %d", rec.Code)
			}
			if got := atomic.LoadInt32(&gatewayCalls); got != 0 {
				t.Fatalf("fresh-cache request fetched %d times", got)
			}
			if resp := decodeAs[modelsResp](t, rec); len(resp.Data) != 1 || resp.Data[0].ID != "cached-model" {
				t.Fatalf("fresh-cache models = %+v", resp.Data)
			}

			// The forced sync must refetch even though the cache is fresh.
			rec = doReq(t, h, http.MethodGet, "/v1/models?refresh="+query, reqOpts{})
			if rec.Code != http.StatusOK {
				t.Fatalf("forced models status = %d body %s", rec.Code, rec.Body.String())
			}
			if got := atomic.LoadInt32(&gatewayCalls); got != 1 {
				t.Fatalf("forced sync fetched %d times, want 1", got)
			}
			resp := decodeAs[modelsResp](t, rec)
			if len(resp.Data) != 1 || resp.Data[0].ID != "fresh-model" {
				t.Fatalf("forced models = %+v", resp.Data)
			}
			if p.modelsStale(clock.Now().Unix()) {
				t.Fatal("forced refresh must reset the cache timestamp")
			}
		})
	}
}

// A failed forced refresh must not fail the request: the cached catalog is
// served with 200.
func TestForcedModelRefreshFallsBackToCacheOnFailure(t *testing.T) {
	var upstreamCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamCalls, 1)
		http.Error(w, "catalog unavailable", http.StatusInternalServerError)
	}))
	defer srv.Close()

	ep := testEndpoints()
	ep.GatewayConfig = srv.URL + "/gateway"
	ep.BuiltinModels = srv.URL + "/builtin"
	clock := newFakeClock()
	dir := t.TempDir()
	writeModelCache(t, dir, clock.Now().Unix(), "cached-model")
	p, h := newTestPlugin(t, func(o *Options) {
		o.Endpoints = ep
		o.Now = clock.Now
		o.DataDir = dir
	})
	seedAccount(t, p, "alice")

	rec := doReq(t, h, http.MethodGet, "/v1/models?refresh=1", reqOpts{})
	if rec.Code != http.StatusOK {
		t.Fatalf("forced models status = %d body %s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&upstreamCalls); got == 0 {
		t.Fatal("forced refresh did not attempt a live fetch")
	}
	resp := decodeAs[modelsResp](t, rec)
	if len(resp.Data) != 1 || resp.Data[0].ID != "cached-model" {
		t.Fatalf("fallback models = %+v", resp.Data)
	}
	if models, _ := p.modelSnapshot(); len(models) != 1 || models[0].ID != "cached-model" {
		t.Fatalf("failed forced refresh must keep the cached models: %+v", models)
	}
}
