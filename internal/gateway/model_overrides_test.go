package gateway

import (
	"net/http"
	"reflect"
	"testing"

	"aiworkgate/internal/model"
	"aiworkgate/internal/store"
)

// newOverrideStore opens a throwaway database. The test passes its
// GetModelOverrides as Options.Overrides, exactly like the host does.
func newOverrideStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(testTempDir(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	return st
}

func modelByID(t *testing.T, payload modelsList, id string) modelEntry {
	t.Helper()
	for _, entry := range payload.Data {
		if entry.ID == id {
			return entry
		}
	}
	t.Fatalf("model %q missing from %v", id, modelIDs(payload))
	return modelEntry{}
}

// TestModelsHumanOverrideWins checks the merge point required by plan §3:
// the human override replaces only the pinned fields of the plugin report, the
// entry carries the `overridden` marker, and clearing the override falls back
// to the reported values without restarting the gateway.
func TestModelsHumanOverrideWins(t *testing.T) {
	st := newOverrideStore(t)

	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{
		ID:            "glm-5",
		Name:          "插件名",
		Vendor:        "插件厂商",
		Multiplier:    1,
		Thinking:      []string{"low"},
		ContextLength: 128000,
		Vision:        false,
		Free:          false,
		Tools:         true,
	}, model.ModelInfo{ID: "plain", Name: "普通模型"}), "", "")
	g := newTestGateway(t, Options{Plugins: reg, Overrides: st.GetModelOverrides})

	name := "人工名"
	contextLength := 200000
	vision := true
	if err := st.PutModelOverride("p1", "glm-5", store.ModelOverride{
		DisplayName:   &name,
		ContextLength: &contextLength,
		Vision:        &vision,
	}); err != nil {
		t.Fatalf("PutModelOverride: %v", err)
	}

	rec := serve(g, newGetRequest(t, "/v1/models", clientKey))
	if rec.Code != 200 {
		t.Fatalf("status = %d (body %s)", rec.Code, rec.Body.String())
	}
	payload := decodeModels(t, rec)
	entry := modelByID(t, payload, "p1/glm-5")
	if entry.Name != "人工名" || entry.ContextLength != 200000 || !entry.Vision {
		t.Fatalf("merged entry = %+v, want the human override", entry)
	}
	if !entry.Tools || entry.OwnedBy != "plugin:p1" {
		t.Fatalf("unrelated fields changed: %+v", entry)
	}
	want := []string{"display_name", "context_length", "vision"}
	if !reflect.DeepEqual(entry.Overridden, want) {
		t.Fatalf("overridden = %v, want %v", entry.Overridden, want)
	}

	// A model without an override keeps the plugin metadata and no marker.
	other := modelByID(t, payload, "p1/plain")
	if other.Name != "普通模型" || len(other.Overridden) != 0 {
		t.Fatalf("plain model entry = %+v, want untouched", other)
	}

	// Clearing the override restores the plugin report on the next list.
	if err := st.DeleteModelOverride("p1", "glm-5"); err != nil {
		t.Fatalf("DeleteModelOverride: %v", err)
	}
	rec = serve(g, newGetRequest(t, "/v1/models", clientKey))
	entry = modelByID(t, decodeModels(t, rec), "p1/glm-5")
	if entry.Name != "插件名" || entry.ContextLength != 128000 || entry.Vision {
		t.Fatalf("entry after clear = %+v, want the plugin report", entry)
	}
	if len(entry.Overridden) != 0 {
		t.Fatalf("overridden after clear = %v, want none", entry.Overridden)
	}
}

// TestModelsOverrideSurvivesPluginModelRefetch makes sure the merge reads the
// override table on every list instead of caching it: a re-sync of the plugin
// model list must not resurrect stale metadata.
func TestModelsOverrideSurvivesPluginModelRefetch(t *testing.T) {
	st := newOverrideStore(t)
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{
		ID: "glm-5", Name: "插件名", ContextLength: 128000,
	}), "", "")
	g := newTestGateway(t, Options{Plugins: reg, Overrides: st.GetModelOverrides})

	name := "人工名"
	if err := st.PutModelOverride("p1", "glm-5", store.ModelOverride{DisplayName: &name}); err != nil {
		t.Fatalf("PutModelOverride: %v", err)
	}
	for i := 0; i < 2; i++ {
		rec := serve(g, newGetRequest(t, "/v1/models", clientKey))
		entry := modelByID(t, decodeModels(t, rec), "p1/glm-5")
		if entry.Name != "人工名" {
			t.Fatalf("list %d name = %q, want 人工名", i+1, entry.Name)
		}
	}
}

// D-34: max_output_tokens publishes on /v1/models — the plugin report carries
// it, the override layer wins, and a model whose source said nothing leaves
// the field absent. Metadata only: the forwarding path never rewrites a
// request with it (2026-11-07 口径).
func TestModelsPublishesMaxOutputTokens(t *testing.T) {
	st := newOverrideStore(t)
	overrideCap := 9999
	if err := st.PutModelOverride("p1", "overridden", store.ModelOverride{
		MaxOutputTokens: &overrideCap,
	}); err != nil {
		t.Fatalf("PutModelOverride: %v", err)
	}

	reg := newFakeRegistry().add(pluginStatus("p1", true,
		model.ModelInfo{ID: "capped", Name: "Cap", MaxOutputTokens: 32768},
		model.ModelInfo{ID: "overridden", Name: "Ov", MaxOutputTokens: 128},
		model.ModelInfo{ID: "uncapped", Name: "No cap"},
	), "", "")
	g := newTestGateway(t, Options{Plugins: reg, Overrides: st.GetModelOverrides})

	rr := serve(g, newGetRequest(t, "/v1/models", clientKey))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rr.Code, rr.Body.String())
	}
	payload := decodeModels(t, rr)

	capped := modelByID(t, payload, "p1/capped")
	if capped.MaxOutputTokens == nil || *capped.MaxOutputTokens != 32768 {
		t.Fatalf("capped max_output_tokens = %v, want 32768", capped.MaxOutputTokens)
	}
	overridden := modelByID(t, payload, "p1/overridden")
	if overridden.MaxOutputTokens == nil || *overridden.MaxOutputTokens != 9999 {
		t.Fatalf("overridden max_output_tokens = %v, want the override 9999", overridden.MaxOutputTokens)
	}
	wantOverridden := []string{"max_output_tokens"}
	if !reflect.DeepEqual(overridden.Overridden, wantOverridden) {
		t.Fatalf("overridden marker = %v, want %v", overridden.Overridden, wantOverridden)
	}
	uncapped := modelByID(t, payload, "p1/uncapped")
	if uncapped.MaxOutputTokens != nil {
		t.Fatalf("uncapped max_output_tokens = %v, want absent", uncapped.MaxOutputTokens)
	}
}
