package store

import (
	"fmt"
	"testing"
	"time"

	"aiworkgate/internal/model"
)

// seedManyAPILogs inserts n rows one second apart, all on today's local day so
// the timeline's day filter still matches them.
func seedManyAPILogs(t *testing.T, s *Store, n int) string {
	t.Helper()
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	for i := 0; i < n; i++ {
		rec := model.APIRequestLog{
			TS:     base.Add(time.Duration(i) * time.Second).Format(time.RFC3339),
			ReqID:  fmt.Sprintf("scan-%d", i),
			Source: "plugin:mock",
			Model:  "gpt-4o",
			Status: 200,
		}
		if err := s.InsertAPIRequestLog(rec); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	return base.Format("2006-01-02")
}

func atMillis(t *testing.T, raw string) int64 {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return parsed.UnixMilli()
}

// The HTTP paging path must stay clamped to one page. Widening it to the
// timeline scan limit would let a browser ask the database for 50000 rows.
func TestQueryAPIRequestLogsStaysPageClamped(t *testing.T) {
	s := openTestStore(t)
	seedManyAPILogs(t, s, 40)

	rows, total, err := s.QueryAPIRequestLogs(Filter{Limit: maxLogScanLimit})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if total != 40 {
		t.Fatalf("total = %d, want 40", total)
	}
	if len(rows) > maxLogLimit {
		t.Fatalf("paging returned %d rows, want at most %d", len(rows), maxLogLimit)
	}
}

// The timeline walks the request-log source with a keyset cursor: every row is
// returned exactly once, newest first, and no page depends on a growing offset.
func TestQueryAPILogsTimelinePageWalksEveryRowOnce(t *testing.T) {
	s := openTestStore(t)
	const n = 137
	seedManyAPILogs(t, s, n)

	from := time.Now().Add(-2 * time.Hour).Unix()
	to := time.Now().Unix()
	seen := make(map[string]bool, n)
	var beforeAt, beforeID int64
	pages := 0
	for {
		rows, err := s.QueryAPILogsTimelinePage(Filter{From: from, To: to, NoBodies: true}, beforeAt, beforeID, 25)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		if len(rows) == 0 {
			break
		}
		pages++
		for i, row := range rows {
			if seen[row.Log.ReqID] {
				t.Fatalf("row %q returned twice", row.Log.ReqID)
			}
			seen[row.Log.ReqID] = true
			if i > 0 {
				prev := rows[i-1]
				if atMillis(t, row.Log.TS) > atMillis(t, prev.Log.TS) {
					t.Fatalf("page order: %q newer than %q", row.Log.TS, prev.Log.TS)
				}
			}
		}
		last := rows[len(rows)-1]
		beforeAt, beforeID = atMillis(t, last.Log.TS), last.ID
		if pages > 20 {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(seen) != n {
		t.Fatalf("walked %d rows, want %d (pages=%d)", len(seen), n, pages)
	}
}

func TestListAuditTimelinePageWalksEveryRowOnce(t *testing.T) {
	s := openTestStore(t)
	const n = 120
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	for i := 0; i < n; i++ {
		entry := model.AuditEntry{
			TS:       base.Add(time.Duration(i) * time.Second).Format(time.RFC3339),
			Category: "config",
			Action:   fmt.Sprintf("audit-%d", i),
			Result:   "ok",
		}
		if _, err := s.AppendAudit(entry); err != nil {
			t.Fatalf("append audit %d: %v", i, err)
		}
	}
	day := base.Format("2006-01-02")

	// The legacy paging helper stays clamped.
	page, err := s.ListAuditBetween(day, day, maxLogScanLimit)
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	if len(page) > 5000 {
		t.Fatalf("paging audit returned %d rows, want at most 5000", len(page))
	}

	seen := make(map[int64]bool, n)
	var beforeAt, beforeID int64
	for pages := 0; ; pages++ {
		rows, err := s.ListAuditTimelinePage(day, day, beforeAt, beforeID, 17)
		if err != nil {
			t.Fatalf("timeline page: %v", err)
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			if seen[row.ID] {
				t.Fatalf("audit row %d returned twice", row.ID)
			}
			seen[row.ID] = true
		}
		last := rows[len(rows)-1]
		beforeAt, beforeID = atMillis(t, last.TS), last.ID
		if pages > 20 {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(seen) != n {
		t.Fatalf("walked %d audit rows, want %d", len(seen), n)
	}
}

func TestProxyAndConnectivityTimelinePagesWalkEveryRowOnce(t *testing.T) {
	s := openTestStore(t)
	const n = 33
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	day := base.Format("2006-01-02")
	for i := 0; i < n; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		if _, err := s.InsertProxyLog(model.ProxyLog{
			TS: at.Format(time.RFC3339), At: at.Unix(), Result: model.ProxyResultOK,
			TargetHost: "example.com", TargetPort: 443,
		}); err != nil {
			t.Fatalf("proxy row %d: %v", i, err)
		}
		if _, err := s.InsertConnectivityProbe(ConnectivityProbe{
			Kind: "chat", Target: "mock", TargetID: "m", OK: true, CreatedAt: at.Unix(),
		}); err != nil {
			t.Fatalf("probe %d: %v", i, err)
		}
	}

	var proxySeen int
	var beforeAt, beforeID int64
	for pages := 0; ; pages++ {
		rows, err := s.QueryProxyLogsTimelinePage(day, day, beforeAt, beforeID, 8)
		if err != nil {
			t.Fatalf("proxy page: %v", err)
		}
		if len(rows) == 0 {
			break
		}
		proxySeen += len(rows)
		last := rows[len(rows)-1]
		beforeAt, beforeID = last.At*1000, last.ID
		if pages > 10 {
			t.Fatal("proxy pagination did not terminate")
		}
	}
	if proxySeen != n {
		t.Fatalf("walked %d proxy rows, want %d", proxySeen, n)
	}

	var probeSeen int
	beforeAt, beforeID = 0, 0
	for pages := 0; ; pages++ {
		rows, err := s.ListConnectivityProbesTimelinePage(day, day, beforeAt, beforeID, 8)
		if err != nil {
			t.Fatalf("probe page: %v", err)
		}
		if len(rows) == 0 {
			break
		}
		probeSeen += len(rows)
		last := rows[len(rows)-1]
		beforeAt, beforeID = last.CreatedAt*1000, last.ID
		if pages > 10 {
			t.Fatal("probe pagination did not terminate")
		}
	}
	if probeSeen != n {
		t.Fatalf("walked %d probes, want %d", probeSeen, n)
	}
}
