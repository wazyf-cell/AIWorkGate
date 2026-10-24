package gateway

import (
	"net/http"
	"testing"

	"aiworkgate/internal/model"
)

// TestModelsListCarriesCatalogMetadata pins the contract v0.3 fields on the
// public model list: clients pick a model by vendor/price/thinking tiers, so
// dropping them here makes the catalog invisible to agents.
func TestModelsListCarriesCatalogMetadata(t *testing.T) {
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{
		ID:            "glm-5",
		Name:          "GLM-5",
		Vendor:        "Zhipu",
		Multiplier:    0.7,
		Thinking:      []string{"off", "high"},
		ContextLength: 200000,
		Vision:        true,
	}), "", "")
	g := newTestGateway(t, Options{Plugins: reg})

	rr := serve(g, newGetRequest(t, "/v1/models", clientKey))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	payload := decodeModels(t, rr)
	entry := modelByID(t, payload, "p1/glm-5")
	if entry.Vendor != "Zhipu" {
		t.Errorf("vendor = %q", entry.Vendor)
	}
	if entry.Multiplier == nil || *entry.Multiplier != 0.7 {
		t.Errorf("multiplier = %v, want 0.7", entry.Multiplier)
	}
	if len(entry.Thinking) != 2 || entry.Thinking[1] != "high" {
		t.Errorf("thinking = %v", entry.Thinking)
	}
	if !entry.Vision {
		t.Error("vision = false")
	}
}

// TestParseModelsBodyKeepsCatalogMetadata pins the plugin boundary: the host
// used to decode plugin /v1/models into a struct without these fields, so every
// reported vendor/rate/thinking tier silently disappeared.
func TestParseModelsBodyKeepsCatalogMetadata(t *testing.T) {
	body := []byte(`{"data":[{"id":"glm-5","vendor":"Zhipu","multiplier":0.7,
		"thinking":["off","high"],"free":true,"context_length":200000}]}`)
	got := parseModelsBody(body)
	if len(got) != 1 {
		t.Fatalf("models = %d", len(got))
	}
	m := got[0]
	if m.Vendor != "Zhipu" || m.Multiplier != 0.7 || !m.Free || len(m.Thinking) != 2 || m.ContextLength != 200000 {
		t.Fatalf("parsed model = %+v", m)
	}
}

// TestParseModelsBodyKeepsNamespacedIDs pins the catalog of plugins whose
// upstream ids are owner/model (cline: google/gemma-4-31b-it). Dropping them
// left the plugin with an empty model list, so every explicit <plugin>/<model>
// request 404'd even though the account was valid.
func TestParseModelsBodyKeepsNamespacedIDs(t *testing.T) {
	body := []byte(`{"data":[{"id":"google/gemma-4-31b-it:free"},{"id":"glm-5"}]}`)
	got := parseModelsBody(body)
	if len(got) != 2 {
		t.Fatalf("models = %d, want 2 (namespaced id must survive)", len(got))
	}
	if got[0].ID != "google/gemma-4-31b-it:free" {
		t.Fatalf("first id = %q", got[0].ID)
	}
}
