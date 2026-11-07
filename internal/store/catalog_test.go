package store

import (
	"errors"
	"testing"
	"time"

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

func TestSetModelCatalogEnabledGlob(t *testing.T) {
	s := openTestStore(t)
	if err := s.ReplaceModelCatalog("officeace", []model.ModelCatalogEntry{
		{ModelID: "glm-5.3"},
		{ModelID: "glm-air"},
		{ModelID: "deepseek-v4-pro"},
		{ModelID: "openai/gpt-4o"},
	}); err != nil {
		t.Fatalf("ReplaceModelCatalog: %v", err)
	}

	// D-33 批量禁用: the prefix glob hits both glm models and reports the
	// affected row count.
	n, err := s.SetModelCatalogEnabledGlob("officeace", "glm-*", false)
	if err != nil {
		t.Fatalf("SetModelCatalogEnabledGlob(glm-*): %v", err)
	}
	if n != 2 {
		t.Fatalf("glm-* disabled %d rows, want 2", n)
	}
	state := map[string]bool{}
	for _, e := range mustListCatalog(t, s, "officeace") {
		state[e.ModelID] = e.Enabled
	}
	if state["glm-5.3"] || state["glm-air"] {
		t.Fatalf("glob disable missed glm rows: %v", state)
	}
	if !state["deepseek-v4-pro"] || !state["openai/gpt-4o"] {
		t.Fatalf("glob disable touched unrelated rows: %v", state)
	}

	// `*` spans slashes, `?` spans exactly one character.
	if n, _ := s.SetModelCatalogEnabledGlob("officeace", "*", true); n != 4 {
		t.Fatalf("* enabled %d rows, want 4", n)
	}
	if n, _ := s.SetModelCatalogEnabledGlob("officeace", "glm-5?3", false); n != 1 {
		t.Fatalf("glm-5?3 matched %d rows, want glm-5.3 only", n)
	}
	// An exact pattern without wildcards behaves like the single switch.
	if n, _ := s.SetModelCatalogEnabledGlob("officeace", "glm-air", true); n != 1 {
		t.Fatalf("exact pattern re-enabled %d rows, want 1", n)
	}

	// Matching nothing is a 0-row answer, not an error; a blank pattern is
	// rejected so a stray empty field cannot wipe a whole catalog.
	if n, err := s.SetModelCatalogEnabledGlob("officeace", "kimi-*", false); err != nil || n != 0 {
		t.Fatalf("no-match = (%d, %v), want (0, nil)", n, err)
	}
	if _, err := s.SetModelCatalogEnabledGlob("officeace", "  ", false); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("blank pattern error = %v, want ErrInvalidArgument", err)
	}
	if _, err := s.SetModelCatalogEnabledGlob("", "*", false); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("blank plugin error = %v, want ErrInvalidArgument", err)
	}
}

func TestModelCatalogStateDisableSemantics(t *testing.T) {
	s := openTestStore(t)
	if err := s.ReplaceModelCatalog("p1", []model.ModelCatalogEntry{
		{ModelID: "m1"},
		{ModelID: "m2"},
	}); err != nil {
		t.Fatalf("ReplaceModelCatalog: %v", err)
	}
	if err := s.SetModelCatalogEnabled("p1", "m2", false); err != nil {
		t.Fatalf("SetModelCatalogEnabled: %v", err)
	}

	// D-33: the state query sees disabled rows, which is what separates the
	// 400 model_disabled answer from the D-60 404.
	if found, enabled, err := s.ModelCatalogState("p1", "m1"); err != nil || !found || !enabled {
		t.Fatalf("m1 state = (%v, %v, %v), want (true, true, nil)", found, enabled, err)
	}
	if found, enabled, err := s.ModelCatalogState("p1", "m2"); err != nil || !found || enabled {
		t.Fatalf("m2 state = (%v, %v, %v), want (true, false, nil)", found, enabled, err)
	}
	if found, _, err := s.ModelCatalogState("p1", "missing"); err != nil || found {
		t.Fatalf("missing state = (%v, _, %v), want (false, _, nil)", found, err)
	}
	if _, _, err := s.ModelCatalogState("", "m1"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("blank plugin error = %v, want ErrInvalidArgument", err)
	}
}

func TestReplaceModelCatalogGracePeriod(t *testing.T) {
	s := openTestStore(t)
	if err := s.ReplaceModelCatalog("p1", []model.ModelCatalogEntry{
		{ModelID: "stay", ContextLength: 1000},
		{ModelID: "vanish", ContextLength: 2000},
		{ModelID: "expired"},
	}); err != nil {
		t.Fatalf("ReplaceModelCatalog: %v", err)
	}
	// Seed a row whose grace period already ran out: the next sync's cleanup
	// pass must physically delete it.
	stale := nowUnix() - int64(ModelCatalogGracePeriod/time.Second) - 60
	if _, err := s.db.Exec(`UPDATE model_catalog SET removed_at = ? WHERE plugin_id = 'p1' AND model_id = 'expired'`, stale); err != nil {
		t.Fatalf("backdate removed_at: %v", err)
	}

	if err := s.ReplaceModelCatalog("p1", []model.ModelCatalogEntry{
		{ModelID: "stay", ContextLength: 1000},
	}); err != nil {
		t.Fatalf("ReplaceModelCatalog (drop vanish): %v", err)
	}

	// D-38 宽限期: the vanished model is still listed, now carrying the
	// removal stamp; the expired row is gone from the listing but not yet
	// from the table.
	list := mustListCatalog(t, s, "p1")
	byID := map[string]model.ModelCatalogEntry{}
	for _, e := range list {
		byID[e.ModelID] = e
	}
	if len(list) != 2 {
		t.Fatalf("listed %d rows, want stay + vanish (expired hidden): %v", len(list), list)
	}
	if v, ok := byID["vanish"]; !ok || v.RemovedAt == 0 || v.Enabled != true {
		t.Fatalf("vanish row = %+v, want listed with RemovedAt and its enabled flag", v)
	}
	if _, ok := byID["expired"]; ok {
		t.Fatal("expired grace row still listed")
	}
	if found, enabled, err := s.ModelCatalogState("p1", "vanish"); err != nil || !found || !enabled {
		t.Fatalf("vanish state = (%v, %v, %v), want (true, true, nil) during grace", found, enabled, err)
	}
	if found, _, err := s.ModelCatalogState("p1", "expired"); err != nil || found {
		t.Fatalf("expired state = (%v, _, %v), want absent", found, err)
	}

	// A model that reappears inside the grace window loses its stamp.
	if err := s.ReplaceModelCatalog("p1", []model.ModelCatalogEntry{
		{ModelID: "stay"},
		{ModelID: "vanish"},
	}); err != nil {
		t.Fatalf("ReplaceModelCatalog (vanish returns): %v", err)
	}
	list = mustListCatalog(t, s, "p1")
	if len(list) != 2 {
		t.Fatalf("listed %d rows after return, want 2: %v", len(list), list)
	}
	for _, e := range list {
		if e.RemovedAt != 0 {
			t.Fatalf("returned model %q still stamped removed_at=%d", e.ModelID, e.RemovedAt)
		}
	}

	// Expiry cleanup: backdate the stamp, sync again, and the row is really
	// gone — not filtered, deleted.
	if _, err := s.db.Exec(`UPDATE model_catalog SET removed_at = ? WHERE plugin_id = 'p1' AND model_id = 'vanish'`, stale); err != nil {
		t.Fatalf("backdate removed_at: %v", err)
	}
	if err := s.ReplaceModelCatalog("p1", []model.ModelCatalogEntry{{ModelID: "stay", ContextLength: 1000}}); err != nil {
		t.Fatalf("ReplaceModelCatalog (cleanup): %v", err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM model_catalog WHERE plugin_id = 'p1' AND model_id = 'vanish'`).Scan(&count); err != nil {
		t.Fatalf("count vanish: %v", err)
	}
	if count != 0 {
		t.Fatalf("expired grace row survived cleanup, count = %d", count)
	}
	// The kept row still answers with its context window.
	if _, id, length := s.FindModelContextLength("stay"); id != "stay" || length != 1000 {
		t.Fatalf("FindModelContextLength(stay) = (_, %q, %d)", id, length)
	}
	if _, id, _ := s.FindModelContextLength("vanish"); id != "" {
		t.Fatalf("removed model still resolves a context window: %q", id)
	}
}

func mustListCatalog(t *testing.T, s *Store, pluginID string) []model.ModelCatalogEntry {
	t.Helper()
	list, err := s.ListModelCatalog(pluginID)
	if err != nil {
		t.Fatalf("ListModelCatalog: %v", err)
	}
	return list
}
