package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"aiworkgate/internal/model"
)

// TestFoldCustomModelsIntoCatalog pins the D-37 fold: an attached custom model
// mirrors into model_catalog and model_overrides under the
// "custom:<connection_id>" namespace, a legacy row without a connection stays
// untouched, and re-running the fold never clobbers a human override.
func TestFoldCustomModelsIntoCatalog(t *testing.T) {
	s := openTestStore(t)

	conn, err := s.UpsertCustomConnection(CustomConnection{
		Label: "c1", Protocol: ProtocolOpenAI, BaseURL: "https://api.example.com/v1", Enabled: true,
	})
	if err != nil {
		t.Fatalf("UpsertCustomConnection: %v", err)
	}
	attached, err := s.UpsertCustomModel(CustomModel{
		ConnectionID: conn.ID, Name: "my-model", BaseURL: "https://api.example.com/v1",
		UpstreamModel: "upstream-x", Enabled: true,
		ContextWindow: 128000, MaxOutputTokens: 8192,
		ReasoningTiers: []string{"low", "high"}, DefaultReasoning: "high",
	})
	if err != nil {
		t.Fatalf("UpsertCustomModel attached: %v", err)
	}
	legacy, err := s.UpsertCustomModel(CustomModel{
		Name: "legacy", BaseURL: "https://old.example.com/v1", UpstreamModel: "old-x", Enabled: true,
	})
	if err != nil {
		t.Fatalf("UpsertCustomModel legacy: %v", err)
	}

	folded, err := s.FoldCustomModelsIntoCatalog()
	if err != nil {
		t.Fatalf("FoldCustomModelsIntoCatalog: %v", err)
	}
	if folded != 1 {
		t.Fatalf("folded = %d, want 1 attached row", folded)
	}

	// model_catalog row.
	var pluginID, modelID, display, vendor, thinking string
	var ctxLen, enabled, free int64
	err = s.DB().QueryRow(`SELECT plugin_id, model_id, display_name, vendor, thinking,
		context_length, enabled, free FROM model_catalog WHERE plugin_id = ?`, CustomModelPluginID(conn.ID)).
		Scan(&pluginID, &modelID, &display, &vendor, &thinking, &ctxLen, &enabled, &free)
	if err != nil {
		t.Fatalf("catalog row: %v", err)
	}
	if pluginID != "custom:"+conn.ID || modelID != "upstream-x" || display != "my-model" || vendor != "custom" {
		t.Fatalf("catalog row = %q/%q/%q/%q", pluginID, modelID, display, vendor)
	}
	if ctxLen != 128000 || enabled != 1 || free != 0 {
		t.Fatalf("catalog metadata = ctx %d enabled %d free %d", ctxLen, enabled, free)
	}
	if thinking != `["low","high"]` {
		t.Fatalf("catalog thinking = %q, want the JSON tier array", thinking)
	}

	// model_overrides row through the public read path (thinking must decode).
	overrides, err := s.GetModelOverrides(CustomModelPluginID(conn.ID))
	if err != nil {
		t.Fatalf("GetModelOverrides: %v", err)
	}
	entry, ok := overrides[attached.UpstreamModel]
	if !ok {
		t.Fatalf("override missing for %q: %+v", attached.UpstreamModel, overrides)
	}
	if entry.DisplayName == nil || *entry.DisplayName != "my-model" {
		t.Fatalf("override display = %+v", entry.DisplayName)
	}
	if entry.ContextLength == nil || *entry.ContextLength != 128000 {
		t.Fatalf("override context = %+v", entry.ContextLength)
	}
	if entry.MaxOutputTokens == nil || *entry.MaxOutputTokens != 8192 {
		t.Fatalf("override max output = %+v", entry.MaxOutputTokens)
	}
	if entry.Thinking == nil || len(*entry.Thinking) != 2 {
		t.Fatalf("override thinking = %+v", entry.Thinking)
	}

	// The unattached legacy row is not folded under a phantom namespace.
	var legacyRows int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM model_catalog WHERE model_id = ?`, legacy.UpstreamModel).
		Scan(&legacyRows); err != nil {
		t.Fatal(err)
	}
	if legacyRows != 0 {
		t.Fatalf("legacy row folded without a connection (%d rows)", legacyRows)
	}

	// A human override survives a re-fold; catalog rows stay unique.
	renamed := "Human Renamed"
	if err := s.PutModelOverride(CustomModelPluginID(conn.ID), attached.UpstreamModel,
		ModelOverride{DisplayName: &renamed}); err != nil {
		t.Fatalf("PutModelOverride: %v", err)
	}
	if _, err := s.FoldCustomModelsIntoCatalog(); err != nil {
		t.Fatalf("second fold: %v", err)
	}
	var catalogRows int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM model_catalog WHERE plugin_id = ?`, CustomModelPluginID(conn.ID)).
		Scan(&catalogRows); err != nil {
		t.Fatal(err)
	}
	if catalogRows != 1 {
		t.Fatalf("catalog rows after re-fold = %d, want 1 (idempotent)", catalogRows)
	}
	overrides, err = s.GetModelOverrides(CustomModelPluginID(conn.ID))
	if err != nil {
		t.Fatal(err)
	}
	if got := overrides[attached.UpstreamModel].DisplayName; got == nil || *got != renamed {
		t.Fatalf("re-fold clobbered the human override: %+v", got)
	}

	// The custom_models table itself is kept (read-only compatible).
	if all, err := s.ListCustomModels(); err != nil || len(all) != 2 {
		t.Fatalf("custom_models kept = %v rows (%v), want 2", len(all), err)
	}
}

// TestTotalClientKeyCost pins the D-42② cost sum the gateway budget reads.
func TestTotalClientKeyCost(t *testing.T) {
	s := openTestStore(t)
	cost := func(v float64) *float64 { return &v }
	rows := []model.APIRequestLog{
		{ReqID: "c1", KeyID: "k1", CostUSD: cost(0.5)},
		{ReqID: "c2", KeyID: "k1", CostUSD: cost(1.5)},
		{ReqID: "c3", KeyID: "k2", CostUSD: cost(10)},
		{ReqID: "c4", KeyID: "k1"}, // no configured price: NOT counted as zero cost
	}
	for _, rec := range rows {
		if err := s.InsertAPIRequestLog(rec); err != nil {
			t.Fatalf("insert %s: %v", rec.ReqID, err)
		}
	}
	total, err := s.TotalClientKeyCost("k1")
	if err != nil {
		t.Fatalf("TotalClientKeyCost: %v", err)
	}
	if total != 2.0 {
		t.Fatalf("k1 total = %v, want 2.0", total)
	}
	if total, err = s.TotalClientKeyCost("k2"); err != nil || total != 10 {
		t.Fatalf("k2 total = %v (%v)", total, err)
	}
	if _, err := s.TotalClientKeyCost("  "); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty key error = %v, want ErrInvalidArgument", err)
	}
}

// TestMigrateV26ToV28FoldsAndAddsKeyColumns upgrades a v26 database the same
// way a real installation would and checks the D-37 fold, the D-42② columns,
// the pre-migration backup and idempotency.
func TestMigrateV26ToV28FoldsAndAddsKeyColumns(t *testing.T) {
	dir := t.TempDir()

	// Build a v26 database by replaying migrations 1..26 exactly like Migrate.
	dsn := fmt.Sprintf("%s?_pragma=busy_timeout(5000)", filepath.ToSlash(filepath.Join(dir, dbFileName)))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	ctx := context.Background()
	for i := 0; i < 26; i++ {
		tx, err := db.Begin()
		if err != nil {
			t.Fatalf("v%d begin: %v", i+1, err)
		}
		for _, stmt := range migrations[i].sql {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("v%d sql: %v", i+1, err)
			}
		}
		for _, col := range migrations[i].columns {
			if err := ensureColumn(ctx, tx, col); err != nil {
				t.Fatalf("v%d column %s.%s: %v", i+1, col.table, col.column, err)
			}
		}
		if migrations[i].hook != nil {
			if err := migrations[i].hook(ctx, tx); err != nil {
				t.Fatalf("v%d hook: %v", i+1, err)
			}
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			t.Fatalf("v%d user_version: %v", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("v%d commit: %v", i+1, err)
		}
	}
	// Seed one connection + attached custom model at v26 (the state D-37 folds).
	if _, err := db.ExecContext(ctx, `INSERT INTO custom_connections
		(id, label, protocol, base_url, enabled, created_at, updated_at)
		VALUES ('conn-1', 'c1', 'openai', 'https://api.example.com/v1', 1, 1, 1)`); err != nil {
		t.Fatalf("seed connection: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO custom_models
		(id, name, base_url, upstream_model, enabled, created_at, updated_at,
		 connection_id, context_window, max_output_tokens, input_types, reasoning_tiers, default_reasoning)
		VALUES ('cm-1', 'folded', 'https://api.example.com/v1', 'm-x', 1, 1, 1,
		 'conn-1', 64000, 4096, '["text","image"]', '["low","high"]', 'high')`); err != nil {
		t.Fatalf("seed custom model: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Open() runs the pending v27/v28 steps (and the pre-migration backup).
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open after v26: %v", err)
	}
	defer s.Close()

	var version int
	if err := s.DB().QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != len(migrations) {
		t.Fatalf("user_version = %d, want %d", version, len(migrations))
	}

	var catalogRows, overrideRows int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM model_catalog WHERE plugin_id = 'custom:conn-1' AND model_id = 'm-x'`).
		Scan(&catalogRows); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM model_overrides WHERE plugin_id = 'custom:conn-1' AND model = 'm-x'`).
		Scan(&overrideRows); err != nil {
		t.Fatal(err)
	}
	if catalogRows != 1 || overrideRows != 1 {
		t.Fatalf("folded rows catalog=%d overrides=%d, want 1 and 1", catalogRows, overrideRows)
	}
	overrides, err := s.GetModelOverrides("custom:conn-1")
	if err != nil {
		t.Fatalf("GetModelOverrides: %v", err)
	}
	if got := overrides["m-x"]; got.Thinking == nil || len(*got.Thinking) != 2 {
		t.Fatalf("migrated override thinking = %+v", got.Thinking)
	}

	// D-42② columns (tableColumns is the shared helper in accounts_test.go).
	cols := tableColumns(t, s, "client_keys")
	for _, want := range []string{"read_only", "cost_budget_usd"} {
		if !cols[want] {
			t.Fatalf("client_keys is missing %q after v28 (columns %v)", want, cols)
		}
	}

	// Backup file written before the upgrade.
	matches, err := filepath.Glob(filepath.Join(dir, dbFileName+".bak-v26-v*"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("pre-migration backup missing: %v %v", matches, err)
	}

	// Idempotent: a second migrate changes nothing.
	if err := s.Migrate(); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	var again int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM model_catalog WHERE plugin_id = 'custom:conn-1'`).Scan(&again); err != nil {
		t.Fatal(err)
	}
	if again != catalogRows {
		t.Fatalf("re-migrate changed catalog rows: %d -> %d", catalogRows, again)
	}
}
