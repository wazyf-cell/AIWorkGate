package store

import (
	"errors"
	"testing"

	"aiworkgate/internal/model"
)

// TestModelCatalogSourcePersistsAndReads pins the v29 source column: a sync
// row defaults to "plugin", SetModelCatalogSource stamps the other layers,
// and the value survives a store restart (D-49 Pool 来源列持久化).
func TestModelCatalogSourcePersistsAndReads(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	entries := []model.ModelCatalogEntry{
		{PluginID: "p1", ModelID: "m1", DisplayName: "one", Enabled: true},
		{PluginID: "p1", ModelID: "m2", DisplayName: "two", Enabled: true, Source: model.ModelCatalogSourceVendor},
	}
	if err := st.ReplaceModelCatalog("p1", entries); err != nil {
		t.Fatalf("ReplaceModelCatalog: %v", err)
	}

	catalog, err := st.ListModelCatalog("p1")
	if err != nil || len(catalog) != 2 {
		t.Fatalf("ListModelCatalog = %v, %v", catalog, err)
	}
	byID := map[string]model.ModelCatalogEntry{}
	for _, entry := range catalog {
		byID[entry.ModelID] = entry
	}
	if got := byID["m1"].Source; got != model.ModelCatalogSourcePlugin {
		t.Fatalf("m1 source = %q, want plugin (sync default)", got)
	}
	if got := byID["m2"].Source; got != model.ModelCatalogSourceVendor {
		t.Fatalf("m2 source = %q, want vendor (entry field)", got)
	}

	// Snapshot restore stamps the whole plugin.
	if err := st.SetModelCatalogSource("p1", model.ModelCatalogSourceSnapshot); err != nil {
		t.Fatalf("SetModelCatalogSource: %v", err)
	}
	catalog, _ = st.ListModelCatalog("p1")
	for _, entry := range catalog {
		if entry.Source != model.ModelCatalogSourceSnapshot {
			t.Fatalf("%s source = %q, want snapshot", entry.ModelID, entry.Source)
		}
	}

	// Invalid layers are rejected.
	if err := st.SetModelCatalogSource("p1", "made_up"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("invalid source error = %v, want ErrInvalidArgument", err)
	}

	// Restart: the value persists.
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	st2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	catalog, err = st2.ListModelCatalog("p1")
	if err != nil || len(catalog) != 2 {
		t.Fatalf("reopened catalog = %v, %v", catalog, err)
	}
	for _, entry := range catalog {
		if entry.Source != model.ModelCatalogSourceSnapshot {
			t.Fatalf("after restart %s source = %q, want snapshot", entry.ModelID, entry.Source)
		}
	}
}

// TestAccountLastUsedRecorded pins the D-09 channel-level last used time:
// the throttled touch, the read path and the consumption-chain record.
func TestAccountLastUsedRecorded(t *testing.T) {
	st := openTestStore(t)
	if err := st.UpsertAccountMeta(model.AccountMeta{PluginID: "p1", AccountID: "acc_1"}); err != nil {
		t.Fatalf("UpsertAccountMeta: %v", err)
	}

	if err := st.TouchAccountLastUsed("p1", "acc_1", 1000); err != nil {
		t.Fatalf("TouchAccountLastUsed: %v", err)
	}
	got, found, err := st.AccountLastUsed("p1", "acc_1")
	if err != nil || !found || got != 1000 {
		t.Fatalf("AccountLastUsed = %d, %v, %v", got, found, err)
	}
	// Within the throttle window the older value wins.
	if err := st.TouchAccountLastUsed("p1", "acc_1", 1010); err != nil {
		t.Fatalf("touch throttled: %v", err)
	}
	got, _, _ = st.AccountLastUsed("p1", "acc_1")
	if got != 1000 {
		t.Fatalf("throttled touch moved last_used_at to %d, want 1000", got)
	}
	// Past the window the touch lands.
	if err := st.TouchAccountLastUsed("p1", "acc_1", 1100); err != nil {
		t.Fatalf("touch after window: %v", err)
	}
	got, _, _ = st.AccountLastUsed("p1", "acc_1")
	if got != 1100 {
		t.Fatalf("last_used_at = %d, want 1100", got)
	}

	// The consumption chain: a plugin request through InsertAPIRequestLog
	// refreshes the channel.
	if err := st.InsertAPIRequestLog(model.APIRequestLog{
		TS: "2026-11-08T00:00:00Z", ReqID: "req-1", Source: "plugin:p1", Model: "m",
		Account: "acc_1", Status: 200,
	}); err != nil {
		t.Fatalf("InsertAPIRequestLog: %v", err)
	}
	got, found, _ = st.AccountLastUsed("p1", "acc_1")
	if !found || got <= 1100 {
		t.Fatalf("consumption chain did not refresh last_used_at: %d, %v", got, found)
	}
	// Custom connections and the admin source have no channel row.
	if err := st.InsertAPIRequestLog(model.APIRequestLog{
		TS: "2026-11-08T00:00:00Z", ReqID: "req-2", Source: "custom:cm-1", Model: "m",
		Account: "acc_1", Status: 200,
	}); err != nil {
		t.Fatalf("InsertAPIRequestLog custom: %v", err)
	}
	if err := st.InsertAPIRequestLog(model.APIRequestLog{
		TS: "2026-11-08T00:00:00Z", ReqID: "req-3", Source: "a", Model: "m", Status: 200,
	}); err != nil {
		t.Fatalf("InsertAPIRequestLog a: %v", err)
	}

	// ListAccountMeta exposes the value.
	meta, err := st.ListAccountMeta()
	if err != nil || len(meta) != 1 {
		t.Fatalf("ListAccountMeta = %v, %v", meta, err)
	}
	if meta[0].LastUsedAt != got {
		t.Fatalf("ListAccountMeta last_used_at = %d, want %d", meta[0].LastUsedAt, got)
	}
}

// TestModelCatalogSourceColumnMigration checks the v29 columns exist on a
// freshly migrated database.
func TestModelCatalogSourceColumnMigration(t *testing.T) {
	st := openTestStore(t)
	cols := tableColumns(t, st, "model_catalog")
	if !cols["source"] {
		t.Fatalf("model_catalog missing source column: %v", cols)
	}
	acol := tableColumns(t, st, "account_meta")
	if !acol["last_used_at"] {
		t.Fatalf("account_meta missing last_used_at column: %v", acol)
	}
}
