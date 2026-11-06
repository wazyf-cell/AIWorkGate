package app

import (
	"testing"

	"aiworkgate/internal/model"
)

// D-35 serves(): the editor needs a yes/no when the catalog or manifest knows
// the model, and "unknown" (fail open) when there is no list at all.
func TestPluginServesModel(t *testing.T) {
	app, st := openAppWithStore(t)
	if serves, known := app.pluginServesModel("p1", "m1"); serves || known {
		t.Fatalf("no catalog yet: serves=%v known=%v, want unknown", serves, known)
	}

	if err := st.ReplaceModelCatalog("p1", []model.ModelCatalogEntry{{PluginID: "p1", ModelID: "m1"}}); err != nil {
		t.Fatalf("seed catalog: %v", err)
	}
	if serves, known := app.pluginServesModel("p1", "m1"); !serves || !known {
		t.Fatalf("served model: serves=%v known=%v", serves, known)
	}
	if serves, known := app.pluginServesModel("p1", "ghost"); serves || !known {
		t.Fatalf("missing model: serves=%v known=%v, want false/true", serves, known)
	}
	if serves, known := app.pluginServesModel("p2", "m1"); serves || known {
		t.Fatalf("unsynced plugin: serves=%v known=%v, want unknown", serves, known)
	}
}
