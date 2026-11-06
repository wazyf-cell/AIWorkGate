package store

import (
	"errors"
	"testing"

	"aiworkgate/internal/model"
)

func TestFindModelContextLength(t *testing.T) {
	s := openTestStore(t)
	if err := s.ReplaceModelCatalog("officeace", []model.ModelCatalogEntry{
		{ModelID: "glm-5.3", ContextLength: 200000},
		{ModelID: "deepseek-v4-pro"},
	}); err != nil {
		t.Fatalf("ReplaceModelCatalog: %v", err)
	}
	if err := s.ReplaceModelCatalog("codearts", []model.ModelCatalogEntry{
		{ModelID: "openai/gpt-4o", ContextLength: 128000},
	}); err != nil {
		t.Fatalf("ReplaceModelCatalog: %v", err)
	}

	cases := []struct {
		model   string
		plugin  string
		modelID string
		context int
	}{
		{"glm-5.3", "officeace", "glm-5.3", 200000},
		{"officeace/glm-5.3", "officeace", "glm-5.3", 200000},
		{"gpt-4o", "codearts", "openai/gpt-4o", 128000},
		{"codearts/openai/gpt-4o", "codearts", "openai/gpt-4o", 128000},
		{"deepseek-v4-pro", "", "", 0},
		{"missing", "", "", 0},
		{"", "", "", 0},
	}
	for _, tc := range cases {
		plugin, modelID, length := s.FindModelContextLength(tc.model)
		if plugin != tc.plugin || modelID != tc.modelID || length != tc.context {
			t.Fatalf("FindModelContextLength(%q) = (%q, %q, %d), want (%q, %q, %d)",
				tc.model, plugin, modelID, length, tc.plugin, tc.modelID, tc.context)
		}
	}
}

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
