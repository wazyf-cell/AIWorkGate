package store

// P14-4: MeEnabledCatalogModels backs the /me model selector's fallback
// universe — only rows the resource page still 接入-ed.

import (
	"testing"
	"time"

	"aiworkgate/internal/model"
)

func TestMeEnabledCatalogModelsListsOnlyEnabledRows(t *testing.T) {
	s := openTestStore(t)
	if err := s.ReplaceModelCatalog("p1", []model.ModelCatalogEntry{
		{ModelID: "m1"}, {ModelID: "m2"},
	}); err != nil {
		t.Fatalf("ReplaceModelCatalog p1: %v", err)
	}
	if err := s.ReplaceModelCatalog("p2", []model.ModelCatalogEntry{
		{ModelID: "m3"},
	}); err != nil {
		t.Fatalf("ReplaceModelCatalog p2: %v", err)
	}
	// The resource page switched m2 off; a re-sync keeps that choice.
	if err := s.SetModelCatalogEnabled("p1", "m2", false); err != nil {
		t.Fatalf("SetModelCatalogEnabled: %v", err)
	}
	if err := s.ReplaceModelCatalog("p1", []model.ModelCatalogEntry{
		{ModelID: "m1"}, {ModelID: "m2"},
	}); err != nil {
		t.Fatalf("re-sync p1: %v", err)
	}

	rows, err := s.MeEnabledCatalogModels()
	if err != nil {
		t.Fatalf("MeEnabledCatalogModels: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want the two enabled ids", rows)
	}
	if rows[0].PluginID != "p1" || rows[0].ModelID != "m1" {
		t.Fatalf("rows[0] = %+v, want p1/m1", rows[0])
	}
	if rows[1].PluginID != "p2" || rows[1].ModelID != "m3" {
		t.Fatalf("rows[1] = %+v, want p2/m3", rows[1])
	}

	if err := s.SetModelCatalogEnabled("p1", "m1", false); err != nil {
		t.Fatalf("SetModelCatalogEnabled m1: %v", err)
	}
	rows, err = s.MeEnabledCatalogModels()
	if err != nil {
		t.Fatalf("MeEnabledCatalogModels: %v", err)
	}
	if len(rows) != 1 || rows[0].PluginID != "p2" || rows[0].ModelID != "m3" {
		t.Fatalf("rows = %+v, want only p2/m3", rows)
	}
}

func TestMeEnabledCatalogModelsHonorsGraceWindow(t *testing.T) {
	s := openTestStore(t)
	if err := s.ReplaceModelCatalog("p1", []model.ModelCatalogEntry{
		{ModelID: "stay"}, {ModelID: "vanish"}, {ModelID: "expired"},
	}); err != nil {
		t.Fatalf("ReplaceModelCatalog: %v", err)
	}
	// D-38: one row whose grace period already ran out, one still inside it.
	stale := nowUnix() - int64(ModelCatalogGracePeriod/time.Second) - 60
	if _, err := s.db.Exec(`UPDATE model_catalog SET removed_at = ? WHERE plugin_id = 'p1' AND model_id = 'expired'`, stale); err != nil {
		t.Fatalf("backdate removed_at: %v", err)
	}
	if err := s.ReplaceModelCatalog("p1", []model.ModelCatalogEntry{
		{ModelID: "stay"},
	}); err != nil {
		t.Fatalf("ReplaceModelCatalog (drop vanish): %v", err)
	}

	rows, err := s.MeEnabledCatalogModels()
	if err != nil {
		t.Fatalf("MeEnabledCatalogModels: %v", err)
	}
	// The vanished model is still inside its 48h grace window, so it stays
	// listed like /v1/models does; the expired row is hidden (到期真删).
	ids := map[string]bool{}
	for _, row := range rows {
		ids[row.PluginID+"/"+row.ModelID] = true
	}
	if !ids["p1/stay"] || !ids["p1/vanish"] {
		t.Fatalf("rows = %v, want stay + vanish (in-grace rows stay available)", rows)
	}
	if ids["p1/expired"] {
		t.Fatalf("rows = %v, want the expired-grace row hidden", rows)
	}
}

func TestMeEnabledCatalogModelsEmptyAndClosedStore(t *testing.T) {
	s := openTestStore(t)
	rows, err := s.MeEnabledCatalogModels()
	if err != nil || len(rows) != 0 {
		t.Fatalf("empty catalog = %+v, %v; want no rows", rows, err)
	}
	var closed *Store
	if _, err := closed.MeEnabledCatalogModels(); err == nil {
		t.Fatalf("closed store must report an error")
	}
}
