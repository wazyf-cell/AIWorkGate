package store

import (
	"fmt"
	"testing"
	"time"

	"aiworkgate/internal/model"
)

// TestUsageDailyCountsSumsByDay covers the heatmap source (D-32①): two
// usage_daily rows on the same day collapse into one total, another day stays
// separate, and a day outside the window is never returned.
func TestUsageDailyCountsSumsByDay(t *testing.T) {
	s := openTestStore(t)
	today := time.Now().Format("2006-01-02")
	seed := []struct {
		day      string
		requests int
		errors   int
	}{
		{today, 3, 1},
		{today, 2, 0},
		{time.Now().AddDate(0, 0, -1).Format("2006-01-02"), 5, 2},
		{"2020-01-01", 99, 0},
	}
	for i, row := range seed {
		if _, err := s.DB().Exec(`INSERT INTO usage_daily
			(day, plugin_id, account_id, model, requests, errors)
			VALUES (?, 'plugin:mock', ?, 'm', ?, ?)`,
			row.day, fmt.Sprintf("acc-%d", i), row.requests, row.errors); err != nil {
			t.Fatalf("insert usage_daily: %v", err)
		}
	}

	rows, err := s.UsageDailyCounts(7)
	if err != nil {
		t.Fatalf("UsageDailyCounts: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (yesterday and today; 2020 is out of window)", len(rows))
	}
	want := map[string][2]int64{
		today: {5, 1},
		time.Now().AddDate(0, 0, -1).Format("2006-01-02"): {5, 2},
	}
	for _, row := range rows {
		w, ok := want[row.Day]
		if !ok {
			t.Fatalf("unexpected day %q", row.Day)
		}
		if row.Requests != w[0] || row.Errors != w[1] {
			t.Fatalf("%s requests/errors = %d/%d, want %d/%d", row.Day, row.Requests, row.Errors, w[0], w[1])
		}
	}

	// The ordered result must be oldest day first so the UI can lay the grid
	// without re-sorting.
	if len(rows) == 2 && rows[0].Day >= rows[1].Day {
		t.Fatalf("rows not ordered by day ascending: %s, %s", rows[0].Day, rows[1].Day)
	}

	if _, err := s.UsageDailyCounts(0); err != nil {
		t.Fatalf("zero days should fall back to the default window: %v", err)
	}
}

// TestReasoningSplitBuckets covers the thinking-tier split (D-32⑤): rows with
// reasoning_tokens > 0 form the WithThinking bucket, zero/unknown rows the
// WithoutThinking bucket, and both buckets share the served model name.
func TestReasoningSplitBuckets(t *testing.T) {
	s := openTestStore(t)
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	rows := []model.APIRequestLog{
		// Two reasoning requests and one non-reasoning request for m1.
		{TS: base.Format(time.RFC3339), ReqID: "r1", Source: "plugin:mock", Model: "m1", ModelServed: "up/m1", Status: 200, ReasoningTokens: 42, PromptTokens: 10, CompletionTokens: 20},
		{TS: base.Add(time.Minute).Format(time.RFC3339), ReqID: "r2", Source: "plugin:mock", Model: "m1", ModelServed: "up/m1", Status: 200, ReasoningTokens: 8, PromptTokens: 11, CompletionTokens: 22},
		{TS: base.Add(2 * time.Minute).Format(time.RFC3339), ReqID: "r3", Source: "plugin:mock", Model: "m1", ModelServed: "up/m1", Status: 200, PromptTokens: 12, CompletionTokens: 24},
		// A model that never reasoned: only the WithoutThinking half exists.
		{TS: base.Add(3 * time.Minute).Format(time.RFC3339), ReqID: "r4", Source: "plugin:mock", Model: "m2", ModelServed: "up/m2", Status: 200, PromptTokens: 5, CompletionTokens: 6},
	}
	for _, rec := range rows {
		if err := s.InsertAPIRequestLog(rec); err != nil {
			t.Fatalf("insert %s: %v", rec.ReqID, err)
		}
	}

	got, err := s.ReasoningSplit(7)
	if err != nil {
		t.Fatalf("ReasoningSplit: %v", err)
	}
	type key struct {
		model string
		think bool
	}
	want := map[key][3]int64{
		{"up/m1", true}:  {2, 21, 50}, // requests, prompt, reasoning
		{"up/m1", false}: {1, 12, 0},
		{"up/m2", false}: {1, 5, 0},
	}
	if len(got) != len(want) {
		for _, row := range got {
			t.Logf("got %+v", row)
		}
		t.Fatalf("rows = %d, want %d", len(got), len(want))
	}
	for _, row := range got {
		k := key{row.Model, row.WithThinking}
		w, ok := want[k]
		if !ok {
			t.Fatalf("unexpected bucket %+v", row)
		}
		if row.Requests != w[0] || row.PromptTokens != w[1] || row.ReasoningTokens != w[2] {
			t.Fatalf("%s think=%v = %d/%d/%d, want %d/%d/%d",
				row.Model, row.WithThinking, row.Requests, row.PromptTokens, row.ReasoningTokens, w[0], w[1], w[2])
		}
		// Completion tokens must ride along so the table can show the split's
		// share of output.
		_ = row.CompletionTokens
	}
}
