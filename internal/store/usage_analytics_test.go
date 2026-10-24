package store

import (
	"testing"
	"time"

	"aiworkgate/internal/model"
)

func insertLog(t *testing.T, s *Store, ts string, status int, latencyMS, ttfbMS int64) {
	t.Helper()
	if err := s.InsertAPIRequestLog(model.APIRequestLog{
		TS:        ts,
		ReqID:     "req",
		Source:    "plugin:mock",
		Model:     "m",
		Status:    status,
		LatencyMS: latencyMS,
		TTFBMS:    ttfbMS,
	}); err != nil {
		t.Fatalf("InsertAPIRequestLog: %v", err)
	}
}

func insertModelLog(t *testing.T, s *Store, ts string, status int, modelID string, latencyMS, ttfbMS int64) {
	t.Helper()
	if err := s.InsertAPIRequestLog(model.APIRequestLog{
		TS:        ts,
		ReqID:     "req-" + modelID,
		Source:    "plugin:mock",
		Model:     modelID,
		Status:    status,
		LatencyMS: latencyMS,
		TTFBMS:    ttfbMS,
	}); err != nil {
		t.Fatalf("InsertAPIRequestLog: %v", err)
	}
}

func TestLatencyStatsNearestRank(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()
	today := now.Format("2006-01-02")
	noon := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.Local)
	tsToday := noon.Format(time.RFC3339)

	// Twenty successful requests with latencies 1..20 ms. The first ten also
	// carry TTFB 100..109 ms (streaming); the rest have none.
	for i := 1; i <= 20; i++ {
		ttfb := int64(0)
		if i <= 10 {
			ttfb = int64(99 + i)
		}
		insertLog(t, s, tsToday, 200, int64(i), ttfb)
	}
	// A failure on the same day must not enter any percentile.
	insertLog(t, s, tsToday, 500, 9999, 9999)

	rows, err := s.LatencyStats(1)
	if err != nil {
		t.Fatalf("LatencyStats: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want one day", rows)
	}
	row := rows[0]
	if row.Day != today {
		t.Fatalf("day = %q, want %q", row.Day, today)
	}
	if row.Requests != 20 {
		t.Fatalf("requests = %d, want 20 (status 500 excluded)", row.Requests)
	}
	// A's nearest rank: idx = ceil(p/100*n)-1.
	// n=20: p50 -> ceil(10)-1 = 9 -> sorted[9] = 10; p95 -> ceil(19)-1 = 18 -> 19.
	if row.P50MS == nil || *row.P50MS != 10 {
		t.Fatalf("p50 = %v, want 10", row.P50MS)
	}
	if row.P95MS == nil || *row.P95MS != 19 {
		t.Fatalf("p95 = %v, want 19", row.P95MS)
	}
	// TTFB has ten samples 100..109: p50 -> ceil(5)-1 = 4 -> 104; p95 -> ceil(9.5)-1 = 9 -> 109.
	if row.P50TTFBMS == nil || *row.P50TTFBMS != 104 {
		t.Fatalf("ttfb p50 = %v, want 104", row.P50TTFBMS)
	}
	if row.P95TTFBMS == nil || *row.P95TTFBMS != 109 {
		t.Fatalf("ttfb p95 = %v, want 109", row.P95TTFBMS)
	}
}

func TestLatencyStatsWindowAndEmpty(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()
	noon := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.Local)
	yesterday := noon.AddDate(0, 0, -1)
	insertLog(t, s, yesterday.Format(time.RFC3339), 200, 50, 0)

	oneDay, err := s.LatencyStats(1)
	if err != nil {
		t.Fatalf("LatencyStats(1): %v", err)
	}
	if len(oneDay) != 0 || oneDay == nil {
		t.Fatalf("one-day window = %#v, want non-nil empty slice", oneDay)
	}

	twoDays, err := s.LatencyStats(2)
	if err != nil {
		t.Fatalf("LatencyStats(2): %v", err)
	}
	if len(twoDays) != 1 || twoDays[0].Day != yesterday.Format("2006-01-02") {
		t.Fatalf("two-day window = %+v, want yesterday only", twoDays)
	}
	if twoDays[0].P50MS == nil || *twoDays[0].P50MS != 50 {
		t.Fatalf("p50 = %v, want 50", twoDays[0].P50MS)
	}
	// A single-sample TTFB-population is empty: percentiles stay nil.
	if twoDays[0].P50TTFBMS != nil || twoDays[0].P95TTFBMS != nil {
		t.Fatalf("ttfb percentiles = %v/%v, want nil", twoDays[0].P50TTFBMS, twoDays[0].P95TTFBMS)
	}
}

func TestKeyUsageSummaryAcrossDaysAndDeletedKey(t *testing.T) {
	s := openTestStore(t)
	if err := s.UpsertClientKey("k1", "laptop", "agk_a", "hash-1", true, 0); err != nil {
		t.Fatalf("UpsertClientKey k1: %v", err)
	}
	if err := s.UpsertClientKey("k2", "retired", "agk_b", "hash-2", true, 0); err != nil {
		t.Fatalf("UpsertClientKey k2: %v", err)
	}
	now := time.Now()
	today := ClientKeyUsageDay(now)
	yesterday := ClientKeyUsageDay(now.AddDate(0, 0, -1))
	credits := 1.5
	if err := s.IncrementKeyUsage("k1", yesterday, 2, 100, 40, 30, &credits); err != nil {
		t.Fatalf("increment k1 yesterday: %v", err)
	}
	if err := s.IncrementKeyUsage("k1", today, 1, 10, 5, 2, nil); err != nil {
		t.Fatalf("increment k1 today: %v", err)
	}
	if err := s.IncrementKeyUsage("k2", today, 3, 7, 1, 0, nil); err != nil {
		t.Fatalf("increment k2: %v", err)
	}
	if err := s.DeleteClientKey("k2"); err != nil {
		t.Fatalf("DeleteClientKey k2: %v", err)
	}

	rows, err := s.KeyUsageSummary(2)
	if err != nil {
		t.Fatalf("KeyUsageSummary: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2", rows)
	}
	// Both keys have three requests, so the tie breaks on key_id: k1 first.
	if rows[0].KeyID != "k1" || rows[1].KeyID != "k2" {
		t.Fatalf("order = %s/%s, want k1/k2", rows[0].KeyID, rows[1].KeyID)
	}
	byID := make(map[string]KeyUsageRow, len(rows))
	for _, row := range rows {
		byID[row.KeyID] = row
	}
	k1, ok := byID["k1"]
	if !ok {
		t.Fatalf("k1 missing from %+v", rows)
	}
	if k1.Name != "laptop" {
		t.Fatalf("k1 name = %q", k1.Name)
	}
	if k1.Requests != 3 || k1.PromptTokens != 110 || k1.CompletionTokens != 45 || k1.CachedTokens != 32 {
		t.Fatalf("k1 totals = %+v", k1)
	}
	if k1.Credits == nil || *k1.Credits != 1.5 {
		t.Fatalf("k1 credits = %v, want 1.5", k1.Credits)
	}

	k2, ok := byID["k2"]
	if !ok {
		t.Fatalf("deleted key k2 missing from %+v", rows)
	}
	if k2.Name != "k2" {
		t.Fatalf("deleted key name = %q, want the id", k2.Name)
	}
	if k2.Requests != 3 || k2.PromptTokens != 7 || k2.CompletionTokens != 1 || k2.CachedTokens != 0 {
		t.Fatalf("k2 totals = %+v", k2)
	}
	if k2.Credits != nil {
		t.Fatalf("k2 credits = %v, want nil (never reported)", *k2.Credits)
	}

	// A one-day window only sees today's rows.
	todayRows, err := s.KeyUsageSummary(1)
	if err != nil {
		t.Fatalf("KeyUsageSummary(1): %v", err)
	}
	for _, row := range todayRows {
		if row.KeyID == "k1" && row.Requests != 1 {
			t.Fatalf("today k1 = %+v, want 1 request", row)
		}
	}
}

func TestModelUsageSummaryRankingAndNullCredits(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()
	today := now.Format("2006-01-02")
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")

	if err := s.UpsertUsageDaily(model.UsageDailyRow{
		Day: yesterday, PluginID: "p", AccountID: "a1", Model: "kimi-k2",
		Requests: 3, PromptTokens: 100, CompletionTokens: 40, Credits: 1.5,
	}); err != nil {
		t.Fatalf("upsert kimi yesterday: %v", err)
	}
	if err := s.UpsertUsageDaily(model.UsageDailyRow{
		Day: today, PluginID: "p", AccountID: "a1", Model: "kimi-k2",
		Requests: 2, PromptTokens: 50, CompletionTokens: 10, Credits: 0.5,
	}); err != nil {
		t.Fatalf("upsert kimi today: %v", err)
	}
	// Credits reported by no one: the column stays NULL, not 0.
	if _, err := s.db.Exec(`INSERT INTO usage_daily
		(day, plugin_id, account_id, model, requests, prompt_tokens, completion_tokens, credits)
		VALUES (?, 'p', 'a2', 'gpt-4o', 6, 10, 5, NULL)`, today); err != nil {
		t.Fatalf("insert gpt-4o: %v", err)
	}
	// Outside the two-day window, must not be aggregated.
	if err := s.UpsertUsageDaily(model.UsageDailyRow{
		Day: now.AddDate(0, 0, -10).Format("2006-01-02"), PluginID: "p",
		AccountID: "a1", Model: "old-model", Requests: 99,
	}); err != nil {
		t.Fatalf("upsert old: %v", err)
	}

	rows, err := s.ModelUsageSummary(2)
	if err != nil {
		t.Fatalf("ModelUsageSummary: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2", rows)
	}
	// Ordered by requests desc: gpt-4o (6) before kimi-k2 (5).
	if rows[0].Model != "gpt-4o" || rows[1].Model != "kimi-k2" {
		t.Fatalf("order = %s/%s, want gpt-4o/kimi-k2", rows[0].Model, rows[1].Model)
	}
	if rows[0].Credits != nil {
		t.Fatalf("gpt-4o credits = %v, want nil", *rows[0].Credits)
	}
	kimi := rows[1]
	if kimi.Requests != 5 || kimi.PromptTokens != 150 || kimi.CompletionTokens != 50 {
		t.Fatalf("kimi totals = %+v", kimi)
	}
	if kimi.Credits == nil || *kimi.Credits != 2.0 {
		t.Fatalf("kimi credits = %v, want 2.0", kimi.Credits)
	}

	// One-day window only sees today's rows.
	oneDay, err := s.ModelUsageSummary(1)
	if err != nil {
		t.Fatalf("ModelUsageSummary(1): %v", err)
	}
	for _, row := range oneDay {
		if row.Model == "kimi-k2" && row.Requests != 2 {
			t.Fatalf("today kimi = %+v, want 2 requests", row)
		}
	}
}

func TestModelLatencyStatsPerModel(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()
	noon := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.Local)
	tsToday := noon.Format(time.RFC3339)

	// kimi-k2: five successes 10..14 ms, first three with TTFB 100..102.
	for i := 10; i <= 14; i++ {
		ttfb := int64(0)
		if i <= 12 {
			ttfb = int64(i + 90)
		}
		insertModelLog(t, s, tsToday, 200, "kimi-k2", int64(i), ttfb)
	}
	// gpt-4o: two successes, no TTFB; the failure must be excluded.
	insertModelLog(t, s, tsToday, 200, "gpt-4o", 50, 0)
	insertModelLog(t, s, tsToday, 200, "gpt-4o", 100, 0)
	insertModelLog(t, s, tsToday, 500, "gpt-4o", 9999, 9999)

	rows, err := s.ModelLatencyStats(1)
	if err != nil {
		t.Fatalf("ModelLatencyStats: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2", rows)
	}
	// requests desc: kimi-k2 (5) first.
	byModel := make(map[string]ModelLatencyRow, len(rows))
	for _, row := range rows {
		byModel[row.Model] = row
	}
	kimi, ok := byModel["kimi-k2"]
	if !ok {
		t.Fatalf("kimi-k2 missing from %+v", rows)
	}
	// n=5: p50 -> ceil(2.5)-1 = 2 -> 12; p95 -> ceil(4.75)-1 = 4 -> 14.
	if kimi.Requests != 5 || kimi.P50MS == nil || *kimi.P50MS != 12 ||
		kimi.P95MS == nil || *kimi.P95MS != 14 {
		t.Fatalf("kimi latency = %+v", kimi)
	}
	// TTFB has three samples 100..102: p50 -> ceil(1.5)-1 = 1 -> 101.
	if kimi.P50TTFBMS == nil || *kimi.P50TTFBMS != 101 {
		t.Fatalf("kimi ttfb p50 = %v, want 101", kimi.P50TTFBMS)
	}

	gpt, ok := byModel["gpt-4o"]
	if !ok {
		t.Fatalf("gpt-4o missing from %+v", rows)
	}
	// n=2, sorted [50,100]: p50 -> ceil(1)-1 = 0 -> 50; p95 -> ceil(1.9)-1 = 1 -> 100.
	if gpt.Requests != 2 || gpt.P50MS == nil || *gpt.P50MS != 50 ||
		gpt.P95MS == nil || *gpt.P95MS != 100 {
		t.Fatalf("gpt-4o latency = %+v (status 500 must be excluded)", gpt)
	}
	if gpt.P50TTFBMS != nil || gpt.P95TTFBMS != nil {
		t.Fatalf("gpt-4o ttfb = %v/%v, want nil", gpt.P50TTFBMS, gpt.P95TTFBMS)
	}

	empty, err := s.ModelLatencyStats(1)
	if err != nil || empty == nil {
		t.Fatalf("ModelLatencyStats must return a non-nil slice: %v / %#v", err, empty)
	}
}
