package gateway

import (
	"net/http/httptest"
	"strings"
	"testing"

	"aiworkgate/internal/model"
)

func TestUnknownModelHintHonoursCatalogFilter(t *testing.T) {
	reg := newFakeRegistry().
		add(pluginStatus("officeace", true,
			model.ModelInfo{ID: "glm-5.3"}, model.ModelInfo{ID: "deepseek-v4-pro"}), "", "").
		add(pluginStatus("codearts", true, model.ModelInfo{ID: "GLM-5.2"}), "", "")
	g := newTestGateway(t, Options{Plugins: reg, Log: &fakeLogSink{}})

	rec := &reqRecord{reqID: "hint-1", model: "officeace/glm-5.3"}
	w := httptest.NewRecorder()
	g.writeUnknownModel(w, rec, rec.model, "officeace", map[string]map[string]bool{
		"officeace": {CanonicalModelID("deepseek-v4-pro"): true},
	})

	body := w.Body.String()
	idx := strings.Index(body, "available models:")
	if idx < 0 {
		t.Fatalf("hint has no model list: %s", body)
	}
	list := body[idx:]
	if strings.Contains(list, "glm-5.3") {
		t.Fatalf("hint advertises a model the catalog switched off: %s", list)
	}
	if !strings.Contains(list, "officeace/deepseek-v4-pro") {
		t.Fatalf("hint lost the exposed model: %s", list)
	}
	if !strings.Contains(list, "codearts/GLM-5.2") {
		t.Fatalf("a pool without a catalog filter must stay listed: %s", list)
	}
}

func TestUnknownModelHintUnfilteredWithoutCatalogContext(t *testing.T) {
	reg := newFakeRegistry().add(pluginStatus("officeace", true,
		model.ModelInfo{ID: "glm-5.3"}), "", "")
	g := newTestGateway(t, Options{Plugins: reg, Log: &fakeLogSink{}})

	rec := &reqRecord{reqID: "hint-2", model: "officeace/nope"}
	w := httptest.NewRecorder()
	g.writeUnknownModel(w, rec, rec.model, "officeace")

	if body := w.Body.String(); !strings.Contains(body, "officeace/glm-5.3") {
		t.Fatalf("unfiltered hint should list the plugin model: %s", body)
	}
}
