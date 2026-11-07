package gateway

// D-30① 暴露上限 24、D-30② group_only 第三态（/v1/models 过滤）单测。

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"aiworkgate/internal/model"
	"aiworkgate/internal/scheduler"
)

// TestModelsCap24: a plugin reporting 30 models publishes the sorted first 24;
// the remainder stays schedulable (the cap only shortens the list).
func TestModelsCap24(t *testing.T) {
	mods := make([]model.ModelInfo, 0, 30)
	for i := 0; i < 30; i++ {
		mods = append(mods, model.ModelInfo{ID: fmt.Sprintf("m%02d", i)})
	}
	reg := newFakeRegistry().add(pluginStatus("p", true, mods...), "", "")
	g := newTestGateway(t, Options{Plugins: reg})
	rr := serve(g, newGetRequest(t, "/v1/models", clientKey))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	payload := decodeModels(t, rr)
	if len(payload.Data) != manyModels {
		t.Fatalf("published %d models, want %d", len(payload.Data), manyModels)
	}
	// sorted by model id, first 24
	for i, entry := range payload.Data {
		want := fmt.Sprintf("p/m%02d", i)
		if entry.ID != want {
			t.Fatalf("entry %d = %q, want %q", i, entry.ID, want)
		}
	}
}

// TestModelsCap24StillSchedulable: a model beyond the cap routes normally.
func TestModelsCap24StillSchedulable(t *testing.T) {
	mods := make([]model.ModelInfo, 0, 30)
	for i := 0; i < 30; i++ {
		mods = append(mods, model.ModelInfo{ID: fmt.Sprintf("m%02d", i)})
	}
	srv, _ := newUpstreamServer(t, jsonResponder(http.StatusOK,
		`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	reg := newFakeRegistry().add(pluginStatus("p", true, mods...), srv.URL, "tok")
	g := newTestGateway(t, Options{Plugins: reg})
	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p/m29","messages":[{"role":"user","content":"hi"}]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("scheduling p/m29 = %d (%s), want 200 (cap is list-only)", rr.Code, rr.Body.String())
	}
}

// TestGroupOnlyPoolNotPublished: a group-only pool's models stay out of the
// catalog even when the pool is otherwise enabled.
func TestGroupOnlyPoolNotPublished(t *testing.T) {
	reg := newFakeRegistry().
		add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), "", "").
		add(pluginStatus("p2", true, model.ModelInfo{ID: "gonly"}), "", "")
	g := newTestGateway(t, Options{
		Plugins: reg,
		Snapshot: func(context.Context) (*scheduler.Snapshot, error) { return nil, nil },
	})
	_ = g
	snap := &scheduler.Snapshot{
		Pools: map[string]*scheduler.Pool{},
		Bindings: map[string]scheduler.Binding{},
	}
	snap.Pools["p1"] = &scheduler.Pool{PluginID: "p1", UpstreamEnabled: true, Models: map[string]model.ModelInfo{"m1": {ID: "m1"}}}
	snap.Pools["p2"] = &scheduler.Pool{PluginID: "p2", UpstreamEnabled: true, GroupOnly: true, Models: map[string]model.ModelInfo{"gonly": {ID: "gonly"}}}
	g2 := newTestGateway(t, Options{
		Plugins:  reg,
		Snapshot: func(context.Context) (*scheduler.Snapshot, error) { return snap, nil },
	})
	rr := serve(g2, newGetRequest(t, "/v1/models", clientKey))
	payload := decodeModels(t, rr)
	for _, entry := range payload.Data {
		if entry.ID == "p2/gonly" {
			t.Fatalf("group-only pool model published: %+v", modelIDs(payload))
		}
	}
	if ids := modelIDs(payload); len(ids) == 0 || ids[0] != "p1/m1" {
		t.Fatalf("normal pool missing from catalog: %v", ids)
	}
}