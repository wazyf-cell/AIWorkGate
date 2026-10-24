package store

import (
	"testing"

	"aiworkgate/internal/model"
)

func TestUpdatePluginSettingsFollowGlobalMarkers(t *testing.T) {
	s := openTestStore(t)

	// A settings write with concurrency 0 and strict "跟随全局" (nil) must store
	// NULL for both, so the global value wins on read.
	if err := s.UpdatePluginSettings("mock", true, "", 0, "", nil, 500); err != nil {
		t.Fatalf("update follow: %v", err)
	}
	view, ok, err := s.GetPluginSettingsView("mock")
	if err != nil || !ok {
		t.Fatalf("get view ok=%v err=%v", ok, err)
	}
	if view.Concurrency != 0 {
		t.Fatalf("concurrency = %d, want 0 (跟随全局)", view.Concurrency)
	}
	if view.StrictSet {
		t.Fatalf("StrictSet = true, want false for a NULL strict column")
	}
	if view.InstalledAt != 500 {
		t.Fatalf("installed_at = %d, want 500", view.InstalledAt)
	}

	// An explicit override survives: concurrency 4 and strict=false are stored
	// concretely, so StrictSet is true and Strict is the user's false.
	off := false
	if err := s.UpdatePluginSettings("mock", true, model.StartupOnDemand, 4, "08:30", &off, 0); err != nil {
		t.Fatalf("update override: %v", err)
	}
	view, _, err = s.GetPluginSettingsView("mock")
	if err != nil {
		t.Fatalf("get view: %v", err)
	}
	if view.Concurrency != 4 || !view.StrictSet || view.Strict {
		t.Fatalf("override view = %+v, want concurrency 4, strict=false explicit", view)
	}
	// installed_at stays anchored to the first write when 0 is passed again.
	if view.InstalledAt != 500 {
		t.Fatalf("installed_at = %d, want 500 preserved", view.InstalledAt)
	}

	// ListPluginSettings reports the same StrictSet distinction per row.
	on := true
	if err := s.UpdatePluginSettings("follow", true, "", 0, "", nil, 1); err != nil {
		t.Fatalf("update follow row: %v", err)
	}
	if err := s.UpdatePluginSettings("override", true, "", 2, "", &on, 1); err != nil {
		t.Fatalf("update override row: %v", err)
	}
	rows, err := s.ListPluginSettings()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byID := map[string]PluginSetting{}
	for _, row := range rows {
		byID[row.PluginID] = row
	}
	if follow := byID["follow"]; follow.StrictSet {
		t.Fatalf("follow row StrictSet = true, want false")
	}
	if o := byID["override"]; !o.StrictSet || !o.Strict {
		t.Fatalf("override row = %+v, want StrictSet true and Strict true", o)
	}
}

// TestUpsertPluginSettingsKeepsExplicitStrict documents backward compatibility:
// a row written by install/Enable/Disable via the legacy Upsert path carries a
// concrete strict value, so StrictSet is true and that override keeps winning.
func TestUpsertPluginSettingsKeepsExplicitStrict(t *testing.T) {
	s := openTestStore(t)
	if err := s.UpsertPluginSettings("mock", true, model.StartupAlways, 3, "09:00", false, 10); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	view, ok, err := s.GetPluginSettingsView("mock")
	if err != nil || !ok {
		t.Fatalf("get view ok=%v err=%v", ok, err)
	}
	if !view.StrictSet || view.Strict {
		t.Fatalf("upsert row = %+v, want StrictSet true, Strict false", view)
	}
}
