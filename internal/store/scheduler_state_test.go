package store

import "testing"

// TestSchedulerCooldownRoundTrip covers the durable cooldown row: upsert
// replaces the same (plugin, account, model) key, delete removes one row, and
// load returns every surviving row ordered by key.
func TestSchedulerCooldownRoundTrip(t *testing.T) {
	s := openTestStore(t)
	const now = int64(1_700_000_000)

	rows := []SchedulerCooldown{
		{PluginID: "mock", AccountID: "a1", ModelID: "", Until: now + 60, Reason: "status 500", Strikes: 0},
		{PluginID: "mock", AccountID: "a1", ModelID: "m1", Until: now + 120, Reason: "status 429", Strikes: 2},
		{PluginID: "other", AccountID: "b1", ModelID: "m1", Until: now + 30, Reason: "status 429", Strikes: 1},
	}
	for _, row := range rows {
		if err := s.UpsertSchedulerCooldown(row); err != nil {
			t.Fatalf("upsert %+v: %v", row, err)
		}
	}

	// A second upsert of the same key replaces deadline and strikes.
	if err := s.UpsertSchedulerCooldown(SchedulerCooldown{
		PluginID: "mock", AccountID: "a1", ModelID: "m1",
		Until: now + 240, Reason: "status 429", Strikes: 3,
	}); err != nil {
		t.Fatalf("upsert replacement: %v", err)
	}

	got, err := s.LoadSchedulerCooldowns(now)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("rows = %d, want 3 (%+v)", len(got), got)
	}
	// Ordered by plugin_id, account_id, model_id: "" sorts before "m1".
	if got[0].ModelID != "" || got[1].ModelID != "m1" || got[1].Strikes != 3 ||
		got[1].Until != now+240 || got[1].Reason != "status 429" {
		t.Fatalf("mock/a1 rows = %+v", got[:2])
	}
	if got[2].PluginID != "other" || got[2].AccountID != "b1" || got[2].Strikes != 1 {
		t.Fatalf("other/b1 row = %+v", got[2])
	}

	if err := s.DeleteSchedulerCooldown("mock", "a1", "m1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, err = s.LoadSchedulerCooldowns(now)
	if err != nil {
		t.Fatalf("load after delete: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("rows after delete = %d, want 2", len(got))
	}
	for _, row := range got {
		if row.PluginID == "mock" && row.AccountID == "a1" && row.ModelID == "m1" {
			t.Fatalf("deleted row still present: %+v", row)
		}
	}

	// Invalid arguments are rejected without touching the table.
	if err := s.UpsertSchedulerCooldown(SchedulerCooldown{PluginID: "", AccountID: "a", Until: now + 1}); err == nil {
		t.Fatal("upsert without plugin id accepted")
	}
	if err := s.UpsertSchedulerCooldown(SchedulerCooldown{PluginID: "p", AccountID: "a", Until: 0}); err == nil {
		t.Fatal("upsert without a deadline accepted")
	}
}

// TestSchedulerCooldownExpiryPruning proves that loading prunes expired rows:
// a restart must not resurrect a cooldown that is already over.
func TestSchedulerCooldownExpiryPruning(t *testing.T) {
	s := openTestStore(t)
	const now = int64(1_700_000_000)

	mustUpsert := func(row SchedulerCooldown) {
		t.Helper()
		if err := s.UpsertSchedulerCooldown(row); err != nil {
			t.Fatalf("upsert %+v: %v", row, err)
		}
	}
	mustUpsert(SchedulerCooldown{PluginID: "p", AccountID: "expired", Until: now - 1, Reason: "status 500"})
	mustUpsert(SchedulerCooldown{PluginID: "p", AccountID: "boundary", Until: now, Reason: "status 500"})
	mustUpsert(SchedulerCooldown{PluginID: "p", AccountID: "live", ModelID: "m1", Until: now + 1, Reason: "status 429", Strikes: 1})

	got, err := s.LoadSchedulerCooldowns(now)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 1 || got[0].AccountID != "live" {
		t.Fatalf("loaded = %+v, want only the live row", got)
	}

	// The expired rows were physically removed, not merely filtered.
	var count int
	if err := s.DB().QueryRow(`SELECT count(*) FROM scheduler_cooldowns`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("table rows = %d, want 1", count)
	}

	// Prune is idempotent and reports the number of removed rows.
	if n, err := s.PruneSchedulerCooldowns(now + 1); err != nil || n != 1 {
		t.Fatalf("prune = %d, %v; want 1", n, err)
	}
	if n, err := s.PruneSchedulerCooldowns(now + 1); err != nil || n != 0 {
		t.Fatalf("second prune = %d, %v; want 0", n, err)
	}
}
