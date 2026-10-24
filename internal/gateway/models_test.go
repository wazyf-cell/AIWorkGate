package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"aiworkgate/internal/model"
	"aiworkgate/internal/scheduler"
)

func decodeModels(t *testing.T, rec *httptest.ResponseRecorder) modelsList {
	t.Helper()
	var payload modelsList
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("models body is not JSON: %v (body %q)", err, rec.Body.String())
	}
	return payload
}

func modelIDs(payload modelsList) []string {
	out := make([]string, 0, len(payload.Data))
	for _, m := range payload.Data {
		out = append(out, m.ID)
	}
	return out
}

func TestModelsMergedPrefixedAndDeduped(t *testing.T) {
	p2Srv, p2Calls := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("plugin path = %q, want /v1/models", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[
			{"id":"live","display":"Live","context_length":64000,"tools":true},
			{"id":"chat"},
			{"id":"google/gemma-4-31b-it:free"},
			{"id":""}
		]}`)
	})

	reg := newFakeRegistry()
	reg.add(pluginStatus("p1", true, model.ModelInfo{
		ID: "chat", Name: "Chat One", ContextLength: 128000, Tools: true,
	}), "", "")
	reg.add(pluginStatus("p2", true), p2Srv.URL, "p2-token")
	reg.add(pluginStatus("off", false, model.ModelInfo{ID: "hidden"}), "", "")

	fa := &fakeA{models: []model.ModelInfo{
		{ID: "gpt-4o", Name: "GPT-4o", ContextLength: 200000},
		{ID: "p1/chat", Name: "duplicate full id"},
		{ID: "gpt-4o", Name: "duplicate bare id"},
		{ID: "   "},
	}}
	g := newTestGateway(t, Options{Plugins: reg, A: fa})

	rr := serve(g, newGetRequest(t, "/v1/models", clientKey))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := rr.Header().Get(sourceHeader); got != "a" {
		t.Fatalf("source header = %q, want a", got)
	}
	if got := rr.Header().Get(staleHeader); got != "" {
		t.Fatalf("stale header = %q, want empty while A is healthy", got)
	}

	payload := decodeModels(t, rr)
	if payload.Object != "list" {
		t.Fatalf("object = %q, want list", payload.Object)
	}
	wantIDs := []string{"p1/chat", "p2/live", "p2/chat", "p2/google/gemma-4-31b-it:free", "gpt-4o"}
	gotIDs := modelIDs(payload)
	if len(gotIDs) != len(wantIDs) {
		t.Fatalf("model ids = %v, want %v", gotIDs, wantIDs)
	}
	for i := range wantIDs {
		if gotIDs[i] != wantIDs[i] {
			t.Fatalf("model ids = %v, want %v", gotIDs, wantIDs)
		}
	}

	byID := make(map[string]modelEntry, len(payload.Data))
	for _, m := range payload.Data {
		byID[m.ID] = m
	}
	p1 := byID["p1/chat"]
	if p1.OwnedBy != "plugin:p1" || p1.Name != "Chat One" || p1.ContextLength != 128000 || !p1.Tools {
		t.Fatalf("p1 model metadata = %+v", p1)
	}
	live := byID["p2/live"]
	if live.OwnedBy != "plugin:p2" || live.Name != "Live" || live.ContextLength != 64000 || !live.Tools {
		t.Fatalf("p2 model metadata = %+v", live)
	}
	if byID["p2/chat"].OwnedBy != "plugin:p2" {
		t.Fatalf("p2/chat metadata = %+v", byID["p2/chat"])
	}
	gpt := byID["gpt-4o"]
	if gpt.OwnedBy != "a" || gpt.Name != "GPT-4o" || gpt.ContextLength != 200000 {
		t.Fatalf("A model metadata = %+v", gpt)
	}

	if p2Calls.count() != 1 {
		t.Fatalf("plugin /v1/models calls = %d, want 1", p2Calls.count())
	}
	got := p2Calls.last(t)
	if got.Authorization != "Bearer p2-token" {
		t.Fatalf("plugin models Authorization = %q, want Bearer p2-token", got.Authorization)
	}
	if fa.calls() != 1 {
		t.Fatalf("A Models calls = %d, want 1", fa.calls())
	}
}

func TestModelsAStaleServesLastCache(t *testing.T) {
	reg := newFakeRegistry().add(
		pluginStatus("p", true, model.ModelInfo{ID: "chat"}), "", "")
	fa := &fakeA{models: []model.ModelInfo{{ID: "gpt-4o", Name: "GPT-4o"}}}
	g := newTestGateway(t, Options{
		Plugins:        reg,
		A:              fa,
		AModelCacheTTL: 15 * time.Millisecond,
	})

	first := serve(g, newGetRequest(t, "/v1/models", clientKey))
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d", first.Code)
	}
	if got := first.Header().Get(staleHeader); got != "" {
		t.Fatalf("first stale header = %q, want empty", got)
	}
	if got := modelIDs(decodeModels(t, first)); len(got) != 2 || got[0] != "p/chat" || got[1] != "gpt-4o" {
		t.Fatalf("first models = %v", got)
	}

	fa.setModelsErr(errors.New("A is down"))
	time.Sleep(40 * time.Millisecond)

	second := serve(g, newGetRequest(t, "/v1/models", clientKey))
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, want 200 with stale cache", second.Code)
	}
	if got := second.Header().Get(staleHeader); got != "1" {
		t.Fatalf("second stale header = %q, want 1", got)
	}
	if got := modelIDs(decodeModels(t, second)); len(got) != 2 || got[1] != "gpt-4o" {
		t.Fatalf("stale models = %v, want the cached A model", got)
	}
	if fa.calls() != 2 {
		t.Fatalf("A Models calls = %d, want 2 (cache expired)", fa.calls())
	}
}

func TestModelsAUnavailableWithoutCache(t *testing.T) {
	reg := newFakeRegistry().add(
		pluginStatus("p", true, model.ModelInfo{ID: "chat"}), "", "")
	fa := &fakeA{modelsErr: errors.New("A is down")}
	g := newTestGateway(t, Options{Plugins: reg, A: fa})

	rr := serve(g, newGetRequest(t, "/v1/models", clientKey))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get(staleHeader); got != "1" {
		t.Fatalf("stale header = %q, want 1", got)
	}
	if got := modelIDs(decodeModels(t, rr)); len(got) != 1 || got[0] != "p/chat" {
		t.Fatalf("models = %v, want only the plugin model", got)
	}
}

func TestModelsSurvivesPluginModelsFailure(t *testing.T) {
	dead := deadServerURL(t)
	reg := newFakeRegistry().add(pluginStatus("p", true), dead, "tok")
	fa := &fakeA{models: []model.ModelInfo{{ID: "gpt-4o"}}}
	g := newTestGateway(t, Options{Plugins: reg, A: fa})

	rr := serve(g, newGetRequest(t, "/v1/models", clientKey))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get(staleHeader); got != "" {
		t.Fatalf("stale header = %q; only A failures mark the list stale", got)
	}
	if got := modelIDs(decodeModels(t, rr)); len(got) != 1 || got[0] != "gpt-4o" {
		t.Fatalf("models = %v, want A's model", got)
	}
}

func TestCanonicalModelIDTrimsAndLowercases(t *testing.T) {
	cases := map[string]string{
		"  Kimi-K2 ":  "kimi-k2",
		"kimi-k2":     "kimi-k2",
		"KIMI-K2":     "kimi-k2",
		"\tkimi-k2\n": "kimi-k2",
		"":            "",
		"   ":         "",
	}
	for in, want := range cases {
		if got := CanonicalModelID(in); got != want {
			t.Fatalf("CanonicalModelID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestModelsCanonicalMergeDedupesCaseVariants(t *testing.T) {
	// A (unified_catalog.rs:37,459,490): ids that differ only by case or
	// surrounding whitespace are one model; the first spelling is published.
	reg := newFakeRegistry().add(pluginStatus("p", true,
		model.ModelInfo{ID: "Kimi-K2", Name: "Kimi K2", ContextLength: 128000},
		model.ModelInfo{ID: "kimi-k2", Name: "duplicate spelling"},
		model.ModelInfo{ID: "  KIMI-K2  ", Name: "spaced duplicate"},
	), "", "")
	fa := &fakeA{models: []model.ModelInfo{
		{ID: "gpt-4o", Name: "GPT-4o"},
		{ID: " GPT-4O ", Name: "duplicate spelling"},
	}}
	g := newTestGateway(t, Options{Plugins: reg, A: fa})

	rr := serve(g, newGetRequest(t, "/v1/models", clientKey))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	got := modelIDs(decodeModels(t, rr))
	want := []string{"p/Kimi-K2", "gpt-4o"}
	if len(got) != len(want) {
		t.Fatalf("model ids = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("model ids = %v, want %v", got, want)
		}
	}
	// The surviving entry keeps the metadata of the first spelling.
	byID := make(map[string]modelEntry, len(got))
	for _, m := range decodeModels(t, rr).Data {
		byID[m.ID] = m
	}
	if entry := byID["p/Kimi-K2"]; entry.Name != "Kimi K2" || entry.ContextLength != 128000 {
		t.Fatalf("first spelling metadata = %+v", entry)
	}
}

func TestInvalidateAModelCacheForcesRefetch(t *testing.T) {
	reg := newFakeRegistry().add(pluginStatus("p", true, model.ModelInfo{ID: "chat"}), "", "")
	fa := &fakeA{models: []model.ModelInfo{{ID: "gpt-4o"}}}
	g := newTestGateway(t, Options{Plugins: reg, A: fa})

	if rr := serve(g, newGetRequest(t, "/v1/models", clientKey)); rr.Code != http.StatusOK {
		t.Fatalf("first status = %d", rr.Code)
	}
	if rr := serve(g, newGetRequest(t, "/v1/models", clientKey)); rr.Code != http.StatusOK {
		t.Fatalf("second status = %d", rr.Code)
	}
	if fa.calls() != 1 {
		t.Fatalf("A Models calls = %d, want 1 (cached)", fa.calls())
	}

	g.InvalidateAModelCache()
	if rr := serve(g, newGetRequest(t, "/v1/models", clientKey)); rr.Code != http.StatusOK {
		t.Fatalf("third status = %d", rr.Code)
	}
	if fa.calls() != 2 {
		t.Fatalf("A Models calls = %d, want 2 after invalidation", fa.calls())
	}
}

func TestInvalidateAModelCacheKeepsStaleFallback(t *testing.T) {
	reg := newFakeRegistry().add(pluginStatus("p", true, model.ModelInfo{ID: "chat"}), "", "")
	fa := &fakeA{models: []model.ModelInfo{{ID: "gpt-4o"}}}
	g := newTestGateway(t, Options{Plugins: reg, A: fa})

	if rr := serve(g, newGetRequest(t, "/v1/models", clientKey)); rr.Code != http.StatusOK {
		t.Fatalf("first status = %d", rr.Code)
	}
	fa.setModelsErr(errors.New("A is reconnecting"))
	g.InvalidateAModelCache()

	rr := serve(g, newGetRequest(t, "/v1/models", clientKey))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 served from the previous list", rr.Code)
	}
	if got := rr.Header().Get(staleHeader); got != "1" {
		t.Fatalf("stale header = %q, want 1", got)
	}
	if got := modelIDs(decodeModels(t, rr)); len(got) != 2 {
		t.Fatalf("models = %v, want plugin + cached A model", got)
	}
}

func TestModelsHidePoolsWithUpstreamDisabled(t *testing.T) {
	reg := newFakeRegistry()
	reg.add(pluginStatus("p1", true, model.ModelInfo{ID: "alpha"}), "", "")
	reg.add(pluginStatus("p2", true, model.ModelInfo{ID: "gamma"}), "", "")
	snap := &scheduler.Snapshot{Pools: map[string]*scheduler.Pool{
		"p1": {PluginID: "p1", UpstreamEnabled: true, Models: map[string]model.ModelInfo{"alpha": {ID: "alpha"}}},
		"p2": {PluginID: "p2", UpstreamEnabled: false, Models: map[string]model.ModelInfo{"gamma": {ID: "gamma"}}},
	}}
	g := newTestGateway(t, Options{
		Plugins:  reg,
		Snapshot: func(context.Context) (*scheduler.Snapshot, error) { return snap, nil },
	})
	rr := serve(g, newGetRequest(t, "/v1/models", clientKey))
	ids := modelIDs(decodeModels(t, rr))
	for _, id := range ids {
		if id == "p2/gamma" {
			t.Fatalf("a pool with 接入 off is still advertised: %v", ids)
		}
	}
	found := false
	for _, id := range ids {
		if id == "p1/alpha" {
			found = true
		}
	}
	if !found {
		t.Fatalf("enabled pool's model missing: %v", ids)
	}
}
