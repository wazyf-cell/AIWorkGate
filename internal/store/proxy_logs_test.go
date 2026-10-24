package store

import (
	"testing"
	"time"

	"aiworkgate/internal/model"
)

func insertProxyRow(t *testing.T, s *Store, plugin, account, result string, at time.Time) model.ProxyLog {
	t.Helper()
	row := model.ProxyLog{
		TS: at.Format(time.RFC3339), At: at.Unix(),
		PluginID: plugin, AccountID: account, ProxyID: "px_1", ProxyLabel: "香港",
		TargetHost: "api.example.com", TargetPort: 443, Result: result,
		Status: 200, DurationMS: 12, BytesUp: 100, BytesDown: 200,
	}
	if result == model.ProxyResultFailed {
		row.Error = "dial tcp: timeout"
	}
	if _, err := s.InsertProxyLog(row); err != nil {
		t.Fatalf("InsertProxyLog: %v", err)
	}
	return row
}

func TestProxyLogInsertQueryAndSchema(t *testing.T) {
	s := openTestStore(t)
	if v, err := s.SchemaVersion(); err != nil || v != len(migrations) {
		t.Fatalf("schema version = %d, %v; want %d", v, err, len(migrations))
	}
	now := time.Now()
	insertProxyRow(t, s, "trae", "a1", model.ProxyResultOK, now)
	insertProxyRow(t, s, "trae", "a2", model.ProxyResultFailed, now.Add(-time.Minute))

	rows, total, err := s.QueryProxyLogs(ProxyLogFilter{Limit: 10})
	if err != nil {
		t.Fatalf("QueryProxyLogs: %v", err)
	}
	if total != 2 || len(rows) != 2 {
		t.Fatalf("rows=%d total=%d, want 2/2", len(rows), total)
	}
	if rows[0].TargetHost != "api.example.com" || rows[0].TargetPort != 443 ||
		rows[0].BytesUp != 100 || rows[0].BytesDown != 200 {
		t.Fatalf("row = %+v", rows[0])
	}
	// Newest first.
	if rows[0].At < rows[1].At {
		t.Fatalf("rows not newest-first: %d < %d", rows[0].At, rows[1].At)
	}

	failed, total, err := s.QueryProxyLogs(ProxyLogFilter{Result: model.ProxyResultFailed})
	if err != nil || total != 1 || len(failed) != 1 || failed[0].AccountID != "a2" {
		t.Fatalf("result filter = %+v total=%d err=%v", failed, total, err)
	}
	if failed[0].Error == "" {
		t.Fatalf("failed row lost its error text")
	}

	hit, total, err := s.QueryProxyLogs(ProxyLogFilter{Query: "TIMEOUT"})
	if err != nil || total != 1 {
		t.Fatalf("query filter total=%d err=%v", total, err)
	}
	_ = hit

	page, total, err := s.QueryProxyLogs(ProxyLogFilter{Limit: 1, Offset: 1})
	if err != nil || total != 2 || len(page) != 1 || page[0].AccountID != "a2" {
		t.Fatalf("paging = %+v total=%d err=%v", page, total, err)
	}
}

func TestProxyLogRejectsUnknownResult(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.InsertProxyLog(model.ProxyLog{Result: "weird"}); err == nil {
		t.Fatal("unknown result accepted")
	}
}

func TestProxyLogPruneAndLiveTail(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()
	insertProxyRow(t, s, "trae", "a1", model.ProxyResultOK, now)
	maxID, err := s.MaxProxyLogID()
	if err != nil || maxID == 0 {
		t.Fatalf("MaxProxyLogID = %d, %v", maxID, err)
	}
	insertProxyRow(t, s, "trae", "a2", model.ProxyResultOK, now)

	day := now.Format("2006-01-02")
	since, last, err := s.QueryProxyLogsSince(maxID, day, day, 10)
	if err != nil {
		t.Fatalf("QueryProxyLogsSince: %v", err)
	}
	if len(since) != 1 || since[0].AccountID != "a2" || last <= maxID {
		t.Fatalf("since = %+v last=%d", since, last)
	}

	if n, err := s.PruneProxyLogs(now.Add(time.Hour).Unix()); err != nil || n != 2 {
		t.Fatalf("PruneProxyLogs = %d, %v", n, err)
	}
	if _, total, _ := s.QueryProxyLogs(ProxyLogFilter{}); total != 0 {
		t.Fatalf("rows survived prune: %d", total)
	}
}

// TestLatestProxyFailure drives the read the gateway uses to explain a 5xx:
// newest "failed" tunnel for one (plugin, account) at or after since.
func TestLatestProxyFailure(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()
	insert := func(plugin, account, result, errText string, at time.Time) {
		t.Helper()
		row := model.ProxyLog{
			TS: at.Format(time.RFC3339), At: at.Unix(),
			PluginID: plugin, AccountID: account, ProxyID: "px_1074ee85",
			ProxyLabel: "香港 01", TargetHost: "opencode.ai", TargetPort: 443,
			Result: result, Error: errText,
		}
		if _, err := s.InsertProxyLog(row); err != nil {
			t.Fatalf("InsertProxyLog: %v", err)
		}
	}
	insert("opencode", "default", model.ProxyResultOK, "", now.Add(-30*time.Second))
	insert("opencode", "default", model.ProxyResultFailed, "old tunnel failure", now.Add(-30*time.Minute))
	insert("opencode", "default", model.ProxyResultFailed, "dial tcp 127.0.0.1:7897: connectex: refused", now.Add(-2*time.Minute))
	insert("opencode", "other", model.ProxyResultFailed, "another account", now.Add(-time.Minute))
	insert("trae", "default", model.ProxyResultFailed, "another plugin", now.Add(-time.Minute))

	since := now.Add(-10 * time.Minute).Unix()
	row, err := s.LatestProxyFailure("opencode", "default", since)
	if err != nil {
		t.Fatalf("LatestProxyFailure: %v", err)
	}
	if row == nil {
		t.Fatal("LatestProxyFailure = nil, want the in-window failed row")
	}
	if row.Error != "dial tcp 127.0.0.1:7897: connectex: refused" || row.ProxyLabel != "香港 01" {
		t.Fatalf("row = %+v, want the newest in-window failure", row)
	}

	// The only failure for this pair inside a narrower window is too old.
	row, err = s.LatestProxyFailure("opencode", "default", now.Add(-time.Minute).Unix())
	if err != nil || row != nil {
		t.Fatalf("narrow window = %+v, %v; want nil, nil", row, err)
	}
	// Unknown pairs and empty ids are a clean miss, not an error.
	for _, pair := range [][2]string{{"nope", "default"}, {"opencode", "nope"}, {"", ""}} {
		row, err := s.LatestProxyFailure(pair[0], pair[1], since)
		if err != nil || row != nil {
			t.Fatalf("LatestProxyFailure(%q, %q) = %+v, %v; want nil, nil", pair[0], pair[1], row, err)
		}
	}
}

func TestProxyLogTimelineWindow(t *testing.T) {
	s := openTestStore(t)
	day := time.Date(2026, 10, 20, 12, 0, 0, 0, time.Local)
	insertProxyRow(t, s, "trae", "a1", model.ProxyResultOK, day)
	insertProxyRow(t, s, "trae", "a2", model.ProxyResultOK, day.AddDate(0, 0, -2))
	rows, err := s.QueryProxyLogsTimelinePage("2026-10-20", "2026-10-20", 0, 0, 100)
	if err != nil {
		t.Fatalf("QueryProxyLogsTimelinePage: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("window rows = %d, want 1", len(rows))
	}
}
