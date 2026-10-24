package gateway

import (
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
	}), "", "")
	fa := &fakeA{models: []model.ModelInfo{{ID: "gpt-4o", Name: "GPT-4o"}}}
	g := newTestGateway(t, Options{Plugins: reg, A: fa, Overrides: st.GetModelOverrides})

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
	other := modelByID(t, payload, "gpt-4o")
	if other.Name != "GPT-4o" || len(other.Overridden) != 0 {
		t.Fatalf("A model entry = %+v, want untouched", other)
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
