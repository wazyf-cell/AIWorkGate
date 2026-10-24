package app

import (
	"testing"
	"time"

	"aiworkgate/internal/config"
	"aiworkgate/internal/model"
	"aiworkgate/internal/store"
	"aiworkgate/internal/usage"
)

func openRetentionStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestCleanupUsageFollowsQueryWindow(t *testing.T) {
	st := openRetentionStore(t)
	now := time.Now()
	within := now.AddDate(0, 0, -200).Format("2006-01-02")
	beyond := now.AddDate(0, 0, -400).Format("2006-01-02")
	for _, day := range []string{within, beyond} {
		if err := st.UpsertUsageDaily(model.UsageDailyRow{
			Day: day, PluginID: "plugin:mock", AccountID: "acc", Model: "m", Requests: 1,
		}); err != nil {
			t.Fatalf("UpsertUsageDaily %s: %v", day, err)
		}
		if _, err := st.DB().Exec(`INSERT INTO client_key_usage (day, key_id, requests)
			VALUES (?, 'k1', 1)`, day); err != nil {
			t.Fatalf("insert client_key_usage %s: %v", day, err)
		}
	}

	app := &App{store: st, dataDir: t.TempDir()}
	app.cleanupFiles(config.Default(), now)

	rows, err := st.UsageSeriesOf(store.UsageFilter{})
	if err != nil {
		t.Fatalf("UsageSeriesOf: %v", err)
	}
	if len(rows) != 1 || rows[0].Day != within {
		t.Fatalf("usage rows = %+v, want only the 200-day-old row (query window is 366 days)", rows)
	}
	var kept, pruned int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM client_key_usage WHERE day = ?`, within).Scan(&kept); err != nil {
		t.Fatalf("count kept key usage: %v", err)
	}
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM client_key_usage WHERE day = ?`, beyond).Scan(&pruned); err != nil {
		t.Fatalf("count pruned key usage: %v", err)
	}
	if kept != 1 || pruned != 0 {
		t.Fatalf("key usage kept/pruned = %d/%d, want 1/0", kept, pruned)
	}
}

func TestStartupRollupRunsBeforeRetentionCleanup(t *testing.T) {
	st := openRetentionStore(t)
	cfg := config.Default()
	cfg.Retention.APIRequestDays = 7

	old := time.Now().AddDate(0, 0, -20)
	day := old.Format("2006-01-02")
	if err := st.InsertAPIRequestLog(model.APIRequestLog{
		TS: old.Format(time.RFC3339), Source: "plugin:mock", Account: "acc",
		Model: "m", Status: 200,
	}); err != nil {
		t.Fatalf("InsertAPIRequestLog: %v", err)
	}

	app := &App{store: st, dataDir: t.TempDir(), roller: usage.NewRoller(st, nil)}
	app.startupRollup()
	app.cleanupFiles(cfg, time.Now())

	if _, total, err := st.QueryAPIRequestLogs(store.Filter{}); err != nil || total != 0 {
		t.Fatalf("api_logs after cleanup = %d (err %v), want 0", total, err)
	}
	totals, err := st.UsageTotalsOf(store.UsageFilter{FromDay: day, ToDay: day})
	if err != nil {
		t.Fatalf("UsageTotalsOf: %v", err)
	}
	if totals.Requests != 1 {
		t.Fatalf("rolled requests = %d, want the row folded before deletion", totals.Requests)
	}
}
