package store

import (
	"database/sql"
	"testing"
	"time"

	"aiworkgate/internal/model"
)

// D-18⑦: usage_known records that the upstream reported a parseable usage
// object. Rows without it keep prompt/completion 0 but must read as
// "unknown", never as reported zeroes — including every historical row the
// migration stamps with the DEFAULT 0.

func TestUsageKnownColumnMigrated(t *testing.T) {
	s := openTestStore(t)

	version, err := s.SchemaVersion()
	if err != nil || version != len(migrations) {
		t.Fatalf("schema version = %d, %v; want %d", version, err, len(migrations))
	}
	rows, err := s.DB().Query(`PRAGMA table_info(api_logs)`)
	if err != nil {
		t.Fatalf("table_info api_logs: %v", err)
	}
	defer rows.Close()
	var (
		found    bool
		notNull  int
		dflt     sql.NullString
		declType string
	)
	for rows.Next() {
		var (
			cid    int
			name   string
			ctype  string
			nn     int
			dfl    sql.NullString
			pk     int
		)
		if err := rows.Scan(&cid, &name, &ctype, &nn, &dfl, &pk); err != nil {
			t.Fatalf("table_info scan: %v", err)
		}
		if name == "usage_known" {
			found, notNull, dflt, declType = true, nn, dfl, ctype
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("table_info iterate: %v", err)
	}
	if !found {
		t.Fatal("api_logs is missing usage_known after migration v23")
	}
	if notNull != 1 || dflt.String != "0" {
		t.Fatalf("usage_known decl = %q notnull=%d default=%q, want NOT NULL DEFAULT 0",
			declType, notNull, dflt.String)
	}
}

func TestUsageKnownInsertScanRoundTrip(t *testing.T) {
	s := openTestStore(t)
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	known := model.APIRequestLog{
		TS: base.Format(time.RFC3339), ReqID: "req-known", Source: "plugin:mock", Model: "gpt-4o",
		Status: 200, PromptTokens: 11, CompletionTokens: 7, CachedTokens: 3,
		UsageKnown: true,
	}
	unknown := model.APIRequestLog{
		TS: base.Add(time.Minute).Format(time.RFC3339), ReqID: "req-unknown", Source: "plugin:mock",
		Model: "gpt-4o", Status: 200, PromptTokens: 0, CompletionTokens: 0, UsageKnown: false,
	}
	for _, rec := range []model.APIRequestLog{known, unknown} {
		if err := s.InsertAPIRequestLog(rec); err != nil {
			t.Fatalf("insert %s: %v", rec.ReqID, err)
		}
	}

	// QueryAPIRequestLogs covers both the with-bodies and the NoBodies scan.
	rows, _, err := s.QueryAPIRequestLogs(Filter{})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	byReq := map[string]model.APIRequestLog{}
	for _, row := range rows {
		byReq[row.ReqID] = row
	}
	if got := byReq["req-known"]; !got.UsageKnown || got.PromptTokens != 11 || got.CompletionTokens != 7 {
		t.Fatalf("known row = %+v, want UsageKnown true with 11/7 tokens", got)
	}
	if got := byReq["req-unknown"]; got.UsageKnown || got.PromptTokens != 0 || got.CompletionTokens != 0 {
		t.Fatalf("unknown row = %+v, want UsageKnown false with 0/0 tokens", got)
	}

	noBodyRows, _, err := s.QueryAPIRequestLogs(Filter{NoBodies: true})
	if err != nil {
		t.Fatalf("query nobodies: %v", err)
	}
	for _, row := range noBodyRows {
		if want := row.ReqID == "req-known"; row.UsageKnown != want {
			t.Fatalf("NoBodies row %s UsageKnown = %v, want %v", row.ReqID, row.UsageKnown, want)
		}
	}

	// APILogByReqID (log_rows.go scan).
	rec, ok, err := s.APILogByReqID("req-known")
	if err != nil || !ok {
		t.Fatalf("by req id: %v, %v", ok, err)
	}
	if !rec.UsageKnown {
		t.Fatal("APILogByReqID lost usage_known on the known row")
	}
	rec, ok, err = s.APILogByReqID("req-unknown")
	if err != nil || !ok {
		t.Fatalf("by req id: %v, %v", ok, err)
	}
	if rec.UsageKnown {
		t.Fatal("APILogByReqID invented usage_known on the unknown row")
	}

	// QueryAPILogsTimelinePage (keyset scan).
	page, err := s.QueryAPILogsTimelinePage(Filter{}, 0, 0, 10)
	if err != nil {
		t.Fatalf("timeline page: %v", err)
	}
	if len(page) != 2 {
		t.Fatalf("timeline rows = %d, want 2", len(page))
	}
	if got := page[0].Log; got.ReqID != "req-unknown" || got.UsageKnown {
		t.Fatalf("timeline newest = %+v, want req-unknown with usage_known false", got)
	}
	if got := page[1].Log; got.ReqID != "req-known" || !got.UsageKnown {
		t.Fatalf("timeline older = %+v, want req-known with usage_known true", got)
	}

	// QueryAPILogsSince (SSE cursor scan).
	since, lastID, err := s.QueryAPILogsSince(0, "", "", 10)
	if err != nil {
		t.Fatalf("since: %v", err)
	}
	if len(since) != 2 {
		t.Fatalf("since rows = %d, want 2", len(since))
	}
	if lastID <= 0 {
		t.Fatalf("since cursor = %d, want the highest id", lastID)
	}
	// QueryAPILogsSince is oldest first.
	if since[0].ReqID != "req-known" || !since[0].UsageKnown {
		t.Fatalf("since oldest = %+v, want req-known with usage_known true", since[0])
	}
	if since[1].ReqID != "req-unknown" || since[1].UsageKnown {
		t.Fatalf("since newer = %+v, want req-unknown with usage_known false", since[1])
	}

	// UsageSessions (usage page conversation view).
	sessions, _, err := s.UsageSessions(UsageFilter{}, "", "", 10, 0)
	if err != nil {
		t.Fatalf("usage sessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("sessions = %d, want 2", len(sessions))
	}
	sessionByReq := map[string]model.APIRequestLog{}
	for _, row := range sessions {
		sessionByReq[row.ReqID] = row
	}
	if !sessionByReq["req-known"].UsageKnown {
		t.Fatal("UsageSessions lost usage_known on the known row")
	}
	if sessionByReq["req-unknown"].UsageKnown {
		t.Fatal("UsageSessions invented usage_known on the unknown row")
	}
}

func TestUsageKnownKeyScopedQueries(t *testing.T) {
	s := openTestStore(t)
	base := time.Now().Format(time.RFC3339)
	known := model.APIRequestLog{
		TS: base, ReqID: "req-k", Source: "plugin:mock", Model: "gpt-4o",
		KeyID: "key-1", Status: 200, PromptTokens: 5, CompletionTokens: 2, UsageKnown: true,
	}
	unknown := model.APIRequestLog{
		TS: base, ReqID: "req-u", Source: "plugin:mock", Model: "gpt-4o",
		KeyID: "key-1", Status: 200, UsageKnown: false,
	}
	for _, rec := range []model.APIRequestLog{known, unknown} {
		if err := s.InsertAPIRequestLog(rec); err != nil {
			t.Fatalf("insert %s: %v", rec.ReqID, err)
		}
	}

	logs, err := s.RecentKeyLogs("key-1", 10)
	if err != nil {
		t.Fatalf("recent key logs: %v", err)
	}
	if len(logs) != 2 {
		t.Fatalf("me log rows = %d, want 2", len(logs))
	}
	knownSeen, unknownSeen := false, false
	for _, row := range logs {
		if row.PromptTokens == 5 {
			knownSeen = true
			if !row.UsageKnown {
				t.Fatal("MeLogRow lost usage_known on the known row")
			}
		} else {
			unknownSeen = true
			if row.UsageKnown {
				t.Fatal("MeLogRow invented usage_known on the unknown row")
			}
		}
	}
	if !knownSeen || !unknownSeen {
		t.Fatalf("expected one known and one unknown row, got %+v", logs)
	}
}

func TestUsageKnownHistoricalRowsStayUnknown(t *testing.T) {
	// A pre-v23 database carries rows that simply never recorded the fact;
	// the DEFAULT 0 must keep them honestly unknown, not "reported zero".
	dir := testTempDir(t)
	createV1Database(t, dir)

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open v1 database: %v", err)
	}
	defer s.Close()

	rows, _, err := s.QueryAPIRequestLogs(Filter{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("api logs after migration = %d, %v; want 1", len(rows), err)
	}
	if rows[0].UsageKnown {
		t.Fatalf("historical row reads usage_known=true: %+v", rows[0])
	}
	if rows[0].PromptTokens != 0 || rows[0].CompletionTokens != 0 {
		t.Fatalf("historical row tokens = %d/%d, want 0/0", rows[0].PromptTokens, rows[0].CompletionTokens)
	}
}
