package store

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"aiworkgate/internal/model"
)

func boolPtr(v bool) *bool        { return &v }
func intPtr(v int) *int           { return &v }
func floatPtr(v float64) *float64 { return &v }

// createV1Database writes a database that only ran the version 1 migration,
// with a couple of rows a live host would already have.
func createV1Database(t *testing.T, dir string) {
	t.Helper()
	dsn := fmt.Sprintf("%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", filepath.ToSlash(filepath.Join(dir, dbFileName)))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer db.Close()
	for _, stmt := range migrations[0].sql {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("apply v1 statement: %v", err)
		}
	}
	if _, err := db.Exec(`INSERT INTO api_logs (ts, source, model, status) VALUES ('2026-10-01T10:00:00+08:00', 'a', 'gpt-4o', 200)`); err != nil {
		t.Fatalf("seed api log: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO quota_snapshots (plugin_id, account_id, remaining, total, at) VALUES ('mock', 'acc_1', 12, 20, 100)`); err != nil {
		t.Fatalf("seed quota: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatalf("set version: %v", err)
	}
}

func tableColumns(t *testing.T, s *Store, table string) map[string]bool {
	t.Helper()
	rows, err := s.DB().Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatalf("table_info %s: %v", table, err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var (
			cid       int
			name      string
			ctype     string
			notNull   int
			dfltValue sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dfltValue, &pk); err != nil {
			t.Fatalf("table_info scan: %v", err)
		}
		out[name] = true
	}
	return out
}

func TestMigrateV1DatabaseToV2(t *testing.T) {
	dir := testTempDir(t)
	createV1Database(t, dir)

	// A v1 database opens and migrates in place.
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open v1 database: %v", err)
	}
	defer s.Close()

	version, err := s.SchemaVersion()
	if err != nil || version != len(migrations) {
		t.Fatalf("user_version = %d, %v; want %d", version, err, len(migrations))
	}

	// Existing data survives.
	if _, total, err := s.QueryAPIRequestLogs(Filter{}); err != nil || total != 1 {
		t.Fatalf("api logs after migration = %d, %v; want 1", total, err)
	}
	snapshots, err := s.QueryQuotaSnapshots("mock", "acc_1", 0)
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("quota snapshots after migration = %+v, %v", snapshots, err)
	}

	// The new columns are present.
	logCols := tableColumns(t, s, "api_logs")
	for _, column := range []string{"conversation_id", "logical_model", "credits", "first_token_at"} {
		if !logCols[column] {
			t.Fatalf("api_logs is missing %s", column)
		}
	}
	quotaCols := tableColumns(t, s, "quota_snapshots")
	for _, column := range []string{"expires_at", "plan"} {
		if !quotaCols[column] {
			t.Fatalf("quota_snapshots is missing %s", column)
		}
	}
	for _, table := range []string{
		"account_meta", "account_groups", "pool_members", "pool_settings",
		"model_bindings", "model_binding_items", "model_catalog",
		"sticky_bindings", "usage_daily", "audit_log", "account_cache",
	} {
		if _, err := s.countRows(table); err != nil {
			t.Fatalf("table %s is not usable: %v", table, err)
		}
	}

	// Migrate stays a no-op the second time.
	if err := s.Migrate(); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if v, _ := s.SchemaVersion(); v != len(migrations) {
		t.Fatalf("version after re-run = %d", v)
	}
}

func TestAccountMetaRoundTripAndNulls(t *testing.T) {
	s := openTestStore(t)

	meta := model.AccountMeta{
		PluginID:        "mock",
		AccountID:       "acc_1",
		Alias:           "主号",
		GroupID:         "grp_work",
		EnabledOverride: boolPtr(false),
		Priority:        intPtr(0),
		Weight:          floatPtr(0.5),
		Note:            "note",
	}
	if err := s.UpsertAccountMeta(meta); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, ok, err := s.GetAccountMeta("mock", "acc_1")
	if err != nil || !ok {
		t.Fatalf("get ok=%v err=%v", ok, err)
	}
	if got.Alias != "主号" || got.GroupID != "grp_work" || got.Note != "note" {
		t.Fatalf("meta = %+v", got)
	}
	if got.EnabledOverride == nil || *got.EnabledOverride {
		t.Fatalf("enabled_override = %v, want explicit false", got.EnabledOverride)
	}
	if got.Priority == nil || *got.Priority != 0 {
		t.Fatalf("priority = %v, want explicit 0 (not NULL)", got.Priority)
	}
	if got.Weight == nil || *got.Weight != 0.5 {
		t.Fatalf("weight = %v, want 0.5", got.Weight)
	}
	if got.UpdatedAt == 0 {
		t.Fatal("updated_at not set")
	}

	// Upsert with nil pointers clears the optional columns back to NULL.
	if err := s.UpsertAccountMeta(model.AccountMeta{PluginID: "mock", AccountID: "acc_1", Alias: "renamed"}); err != nil {
		t.Fatalf("upsert clear: %v", err)
	}
	got, _, _ = s.GetAccountMeta("mock", "acc_1")
	if got.EnabledOverride != nil || got.Priority != nil || got.Weight != nil || got.GroupID != "" {
		t.Fatalf("clearing upsert kept values: %+v", got)
	}
	if got.Alias != "renamed" {
		t.Fatalf("alias = %q", got.Alias)
	}

	// Alias-only writes leave the rest of the overlay alone.
	if err := s.UpsertAccountMeta(model.AccountMeta{
		PluginID: "mock", AccountID: "acc_1", Priority: intPtr(7), EnabledOverride: boolPtr(true),
	}); err != nil {
		t.Fatalf("upsert priority: %v", err)
	}
	if err := s.SetAccountAlias("mock", "acc_1", " 新别名 "); err != nil {
		t.Fatalf("set alias: %v", err)
	}
	got, _, _ = s.GetAccountMeta("mock", "acc_1")
	if got.Alias != "新别名" {
		t.Fatalf("alias = %q, want trimmed", got.Alias)
	}
	if got.Priority == nil || *got.Priority != 7 || got.EnabledOverride == nil || !*got.EnabledOverride {
		t.Fatalf("alias write clobbered other fields: %+v", got)
	}

	// Group-only writes behave the same.
	if err := s.SetAccountGroup("mock", "acc_1", "grp_1"); err != nil {
		t.Fatalf("set group: %v", err)
	}
	got, _, _ = s.GetAccountMeta("mock", "acc_1")
	if got.GroupID != "grp_1" || got.Alias != "新别名" {
		t.Fatalf("group write = %+v", got)
	}

	if err := s.SetAccountAlias("mock", "acc_1", ""); err != nil {
		t.Fatalf("clear alias: %v", err)
	}
	got, _, _ = s.GetAccountMeta("mock", "acc_1")
	if got.Alias != "" {
		t.Fatalf("alias = %q, want cleared", got.Alias)
	}

	if _, ok, _ := s.GetAccountMeta("mock", "missing"); ok {
		t.Fatal("missing meta reported as found")
	}
	if _, ok, _ := s.GetAccountMeta("", "acc_1"); ok {
		t.Fatal("empty plugin id accepted")
	}

	rows, err := s.ListAccountMeta()
	if err != nil || len(rows) != 1 {
		t.Fatalf("list = %+v, %v", rows, err)
	}
	if err := s.DeleteAccountMeta("mock", "acc_1"); err != nil {
		t.Fatalf("delete meta: %v", err)
	}
	if _, ok, _ := s.GetAccountMeta("mock", "acc_1"); ok {
		t.Fatal("meta survived deletion")
	}
}

func TestAccountCacheRoundTrip(t *testing.T) {
	s := openTestStore(t)

	accounts := []model.Account{
		{ID: "acc_1", Label: "one", Enabled: true, Status: model.AccountOK, Quota: model.Quota{Remaining: 1}},
		{ID: "acc_2", Label: "two", Enabled: false, Status: model.AccountExpired},
	}
	if err := s.ReplaceAccountCache("mock", accounts); err != nil {
		t.Fatalf("replace cache: %v", err)
	}
	cached, updatedAt, err := s.ListAccountCache("mock")
	if err != nil {
		t.Fatalf("list cache: %v", err)
	}
	if len(cached) != 2 || cached[0].ID != "acc_1" || cached[0].Label != "one" {
		t.Fatalf("cached = %+v", cached)
	}
	if updatedAt == 0 {
		t.Fatal("updated_at not set")
	}

	// Replacing drops accounts the plugin stopped reporting.
	if err := s.ReplaceAccountCache("mock", accounts[:1]); err != nil {
		t.Fatalf("replace cache again: %v", err)
	}
	cached, _, _ = s.ListAccountCache("mock")
	if len(cached) != 1 || cached[0].ID != "acc_1" {
		t.Fatalf("cached after replace = %+v", cached)
	}

	// A single-row upsert replaces the payload.
	if err := s.UpsertAccountCache("mock", "acc_1", `{"id":"acc_1","label":"renamed","enabled":true,"status":"ok"}`); err != nil {
		t.Fatalf("upsert cache: %v", err)
	}
	cached, _, _ = s.ListAccountCache("mock")
	if len(cached) != 1 || cached[0].Label != "renamed" {
		t.Fatalf("cached after upsert = %+v", cached)
	}

	// Rows for another plugin are untouched.
	if err := s.ReplaceAccountCache("other", accounts); err != nil {
		t.Fatalf("replace other: %v", err)
	}
	if err := s.DeleteAccountCache("mock", "acc_1"); err != nil {
		t.Fatalf("delete cache: %v", err)
	}
	if cached, _, _ := s.ListAccountCache("mock"); len(cached) != 0 {
		t.Fatalf("mock cache = %+v", cached)
	}
	if cached, _, _ := s.ListAccountCache("other"); len(cached) != 2 {
		t.Fatalf("other cache = %+v", cached)
	}
	if err := s.DeletePluginAccountCache("other"); err != nil {
		t.Fatalf("delete plugin cache: %v", err)
	}
	if cached, _, _ := s.ListAccountCache("other"); len(cached) != 0 {
		t.Fatalf("other cache survived: %+v", cached)
	}
}

func TestDeleteAccountDataCascade(t *testing.T) {
	s := openTestStore(t)

	if err := s.UpsertAccountMeta(model.AccountMeta{PluginID: "mock", AccountID: "acc_1", Alias: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceAccountCache("mock", []model.Account{{ID: "acc_1"}, {ID: "acc_2"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertPoolMember(model.PoolMember{PluginID: "mock", AccountID: "acc_1", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAccountEgress(model.AccountEgress{PluginID: "mock", AccountID: "acc_1", Mode: model.EgressDirect}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertStickyBinding(model.StickyBinding{
		Scope: model.StickyScopeConversation, Key: "conv-1", PluginID: "mock", AccountID: "acc_1", ExpiresAt: 999,
	}); err != nil {
		t.Fatal(err)
	}
	// Another account's data must survive.
	if err := s.UpsertPoolMember(model.PoolMember{PluginID: "mock", AccountID: "acc_2", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteAccountData("mock", "acc_1"); err != nil {
		t.Fatalf("delete account data: %v", err)
	}
	if _, ok, _ := s.GetAccountMeta("mock", "acc_1"); ok {
		t.Fatal("meta survived")
	}
	if cached, _, _ := s.ListAccountCache("mock"); len(cached) != 1 || cached[0].ID != "acc_2" {
		t.Fatalf("cache = %+v", cached)
	}
	if members, _ := s.ListPoolMembers("mock"); len(members) != 1 || members[0].AccountID != "acc_2" {
		t.Fatalf("members = %+v", members)
	}
	if egress, _ := s.ListAccountEgress("mock"); len(egress) != 0 {
		t.Fatalf("egress = %+v", egress)
	}
	if sticky, _ := s.ListStickyBindings(""); len(sticky) != 0 {
		t.Fatalf("sticky = %+v", sticky)
	}
	if err := s.DeleteAccountData("mock", ""); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty account id = %v", err)
	}
}
