package store

import (
	"errors"
	"testing"

	"aiworkgate/internal/model"
)

func TestModelCatalogEnableSwitch(t *testing.T) {
	s := openTestStore(t)
	entries := []model.ModelCatalogEntry{
		{ModelID: "glm-5.3", DisplayName: "GLM 5.3"},
		{ModelID: "deepseek-v4-pro"},
	}
	if err := s.ReplaceModelCatalog("officeace", entries); err != nil {
		t.Fatalf("ReplaceModelCatalog: %v", err)
	}
	list, err := s.ListModelCatalog("officeace")
	if err != nil {
		t.Fatalf("ListModelCatalog: %v", err)
	}
	for _, entry := range list {
		if !entry.Enabled {
			t.Fatalf("new catalog row %q is disabled by default", entry.ModelID)
		}
	}

	if err := s.SetModelCatalogEnabled("officeace", "glm-5.3", false); err != nil {
		t.Fatalf("SetModelCatalogEnabled: %v", err)
	}
	if err := s.SetModelCatalogEnabled("officeace", "missing", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing model error = %v, want ErrNotFound", err)
	}

	// A re-sync must keep the user's choice for models that still exist.
	if err := s.ReplaceModelCatalog("officeace", append(entries, model.ModelCatalogEntry{ModelID: "new-model"})); err != nil {
		t.Fatalf("ReplaceModelCatalog: %v", err)
	}
	byID := map[string]model.ModelCatalogEntry{}
	list, _ = s.ListModelCatalog("officeace")
	for _, entry := range list {
		byID[entry.ModelID] = entry
	}
	if byID["glm-5.3"].Enabled {
		t.Fatal("sync re-enabled a model the user disabled")
	}
	if !byID["deepseek-v4-pro"].Enabled || !byID["new-model"].Enabled {
		t.Fatalf("other models lost their default: %+v", byID)
	}

	// Enabling again is reflected immediately.
	if err := s.SetModelCatalogEnabled("officeace", "glm-5.3", true); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	list, _ = s.ListModelCatalog("officeace")
	for _, entry := range list {
		if entry.ModelID == "glm-5.3" && !entry.Enabled {
			t.Fatal("model stayed disabled after re-enable")
		}
	}
}
