package usage

import (
	"context"
	"testing"
	"time"

	"aiworkgate/internal/model"
	"aiworkgate/internal/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(testTempDir(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func fixedNow() time.Time {
	return time.Date(2026, 10, 20, 12, 0, 0, 0, time.Local)
}

func mustInsertLog(t *testing.T, st *store.Store, rec model.APIRequestLog) {
	t.Helper()
	if err := st.InsertAPIRequestLog(rec); err != nil {
		t.Fatalf("InsertAPIRequestLog: %v", err)
	}
}

func newTestRoller(st *store.Store) *Roller {
	return &Roller{Store: st, Now: fixedNow, CatchUpDays: 30, Batch: 1000}
}

func TestRollerAggregatesAcrossMinutesWithoutDoubleCount(t *testing.T) {
	st := openTestStore(t)
	first := time.Date(2026, 10, 20, 11, 59, 0, 0, time.Local)
	second := time.Date(2026, 10, 20, 12, 0, 0, 0, time.Local)
	credits := 1.5
	mustInsertLog(t, st, model.APIRequestLog{
		TS: first.Format(time.RFC3339), Source: "plugin:mock", Account: "acc_1", Model: "m1",
		Status: 200, PromptTokens: 10, CompletionTokens: 20, CachedTokens: 7, Credits: &credits,
		LatencyMS: 100, TTFBMS: 40,
	})
	mustInsertLog(t, st, model.APIRequestLog{
		TS: second.Format(time.RFC3339), Source: "plugin:mock", Account: "acc_1", Model: "m1",
		Status: 500, PromptTokens: 5, CompletionTokens: 0, CachedTokens: 4,
		LatencyMS: 300, TTFBMS: 0,
	})

	r := newTestRoller(st)
	for i := 0; i < 2; i++ {
		n, err := r.RollOnce(context.Background())
		if err != nil {
			t.Fatalf("RollOnce #%d: %v", i, err)
		}
		if i == 0 && n != 2 {
			t.Fatalf("first roll rows = %d, want 2", n)
		}
		if i == 1 && n != 0 {
			t.Fatalf("second roll rows = %d, want 0 (no double count)", n)
		}
	}

	totals, err := st.UsageTotalsOf(store.UsageFilter{FromDay: "2026-10-20", ToDay: "2026-10-20"})
	if err != nil {
		t.Fatalf("UsageTotalsOf: %v", err)
	}
	if totals.Requests != 2 || totals.Errors != 1 {
		t.Fatalf("totals = %+v, want 2 requests / 1 error", totals)
	}
	if totals.PromptTokens != 15 || totals.CompletionTokens != 20 {
		t.Fatalf("tokens = %d/%d", totals.PromptTokens, totals.CompletionTokens)
	}
	if totals.Credits != 1.5 || totals.LatencyMSSum != 400 {
		t.Fatalf("credits/latency = %v/%d", totals.Credits, totals.LatencyMSSum)
	}
	if totals.TTFBMSSum != 40 || totals.TTFBCount != 1 {
		t.Fatalf("ttfb = %d/%d", totals.TTFBMSSum, totals.TTFBCount)
	}
	if totals.ActiveAccounts != 1 {
		t.Fatalf("active accounts = %d, want 1", totals.ActiveAccounts)
	}
	// cached_tokens is folded into usage_daily alongside prompt/completion
	// (plan 2026-10-21 §5); the two rows above carry 7 + 4.
	var cached int64
	if err := st.DB().QueryRow(`SELECT COALESCE(SUM(cached_tokens), 0) FROM usage_daily`).Scan(&cached); err != nil {
		t.Fatalf("read usage_daily.cached_tokens: %v", err)
	}
	if cached != 11 {
		t.Fatalf("usage_daily cached = %d, want 11", cached)
	}
}

func TestRollerRecoversFromCrashWithCursorUnchanged(t *testing.T) {
	st := openTestStore(t)
	ts := fixedNow().Add(-time.Hour).Format(time.RFC3339)
	mustInsertLog(t, st, model.APIRequestLog{TS: ts, Source: "plugin:mock", Account: "acc_1", Model: "m1", Status: 200, PromptTokens: 3})

	// Simulate a crash inside the transaction: the upsert aborts, so the
	// cursor must not advance either.
	if _, err := st.DB().Exec(`CREATE TRIGGER fail_usage BEFORE INSERT ON usage_daily
		BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	r := newTestRoller(st)
	if _, err := r.RollOnce(context.Background()); err == nil {
		t.Fatal("RollOnce succeeded despite the failing trigger")
	}
	if content, ok, err := st.GetKV("usage_rollup_last_id"); err != nil || (ok && content != "") {
		t.Fatalf("cursor after failed roll = %q ok=%v err=%v", content, ok, err)
	}
	if totals, _ := st.UsageTotalsOf(store.UsageFilter{}); totals.Requests != 0 {
		t.Fatalf("rollup committed rows despite rollback: %+v", totals)
	}

	if _, err := st.DB().Exec(`DROP TRIGGER fail_usage`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	n, err := r.RollOnce(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("retry roll = %d, %v; want 1 row", n, err)
	}
	totals, err := st.UsageTotalsOf(store.UsageFilter{})
	if err != nil || totals.Requests != 1 || totals.PromptTokens != 3 {
		t.Fatalf("totals after retry = %+v err=%v", totals, err)
	}
	if _, err := r.RollOnce(context.Background()); err != nil {
		t.Fatalf("third roll: %v", err)
	}
	if totals, _ := st.UsageTotalsOf(store.UsageFilter{}); totals.Requests != 1 {
		t.Fatalf("rows counted twice after retry: %+v", totals)
	}
}

func TestRollerCatchUpAcrossDaysWithNullFields(t *testing.T) {
	st := openTestStore(t)
	days := []string{"2026-10-18", "2026-10-19", "2026-10-20"}
	for i, day := range days {
		ts, err := time.ParseInLocation("2006-01-02 15:04:05", day+" 09:00:00", time.Local)
		if err != nil {
			t.Fatalf("parse day: %v", err)
		}
		// A's rows carry NULL account/tokens; the model is what makes a row
		// real inference, so it is set.
		if _, err := st.DB().Exec(`INSERT INTO api_logs
			(ts, source, model, account, status, prompt_tokens, completion_tokens, latency_ms, ttfb_ms)
			VALUES (?, 'a', 'a-model', NULL, 200, NULL, NULL, NULL, NULL)`,
			ts.Format(time.RFC3339)); err != nil {
			t.Fatalf("insert null row %d: %v", i, err)
		}
		// A probe without a model (GET /v1/models, auth rejection) must not
		// create a usage bucket.
		if _, err := st.DB().Exec(`INSERT INTO api_logs
			(ts, source, model, status) VALUES (?, 'gateway', NULL, 200)`,
			ts.Format(time.RFC3339)); err != nil {
			t.Fatalf("insert probe row %d: %v", i, err)
		}
		mustInsertLog(t, st, model.APIRequestLog{
			TS: ts.Format(time.RFC3339), Source: "plugin:mock", Account: "acc_1", Model: "m1",
			Status: 200, PromptTokens: 1, CompletionTokens: 1, LatencyMS: 10,
		})
	}

	// n counts scanned log rows (3 days x 3 rows), while the probe rows are
	// filtered out of the buckets below.
	n, err := newTestRoller(st).RollOnce(context.Background())
	if err != nil || n != 9 {
		t.Fatalf("catch-up roll = %d, %v; want 9 scanned rows", n, err)
	}
	series, err := st.UsageSeriesOf(store.UsageFilter{FromDay: "2026-10-01", ToDay: "2026-10-31"})
	if err != nil {
		t.Fatalf("UsageSeriesOf: %v", err)
	}
	if len(series) != 6 {
		t.Fatalf("series rows = %d, want 6 (3 days x 2 plugins): %+v", len(series), series)
	}
	for _, row := range series {
		if row.PluginID == "a" {
			if row.AccountID != "" || row.PromptTokens != 0 {
				t.Fatalf("NULL fields not normalised: %+v", row)
			}
		}
	}
	var probeBuckets int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM usage_daily WHERE model = ''`).Scan(&probeBuckets); err != nil {
		t.Fatalf("count model-less buckets: %v", err)
	}
	if probeBuckets != 0 {
		t.Fatalf("model-less probe rows produced %d usage bucket(s)", probeBuckets)
	}

	// A's aggregate is addressable through the plugin filters too.
	totals, err := st.UsageTotalsOf(store.UsageFilter{Plugins: []string{"a"}})
	if err != nil || totals.Requests != 3 {
		t.Fatalf("A totals = %+v err=%v", totals, err)
	}
}

func TestRollerRunStopsWithContext(t *testing.T) {
	st := openTestStore(t)
	r := newTestRoller(st)
	r.Interval = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	r.Start(ctx)
	mustInsertLog(t, st, model.APIRequestLog{
		TS: fixedNow().Format(time.RFC3339), Source: "plugin:mock", Model: "m", Status: 200,
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if totals, _ := st.UsageTotalsOf(store.UsageFilter{}); totals.Requests == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	r.Stop()
	if totals, _ := st.UsageTotalsOf(store.UsageFilter{}); totals.Requests != 1 {
		t.Fatalf("background roller did not aggregate: %+v", totals)
	}
}
