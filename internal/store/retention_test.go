package store

import (
	"testing"
	"time"

	"aiworkgate/internal/model"
)

func TestCleanupAPILogBodiesKeepsRows(t *testing.T) {
	s := openTestStore(t)
	old := time.Now().AddDate(0, 0, -10)
	recent := time.Now()
	for _, rec := range []model.APIRequestLog{
		{TS: old.Format(time.RFC3339), ReqID: "req-old", BodyReq: "old-request", BodyResp: "old-response"},
		{TS: recent.Format(time.RFC3339), ReqID: "req-new", BodyReq: "new-request", BodyResp: "new-response"},
	} {
		if err := s.InsertAPIRequestLog(rec); err != nil {
			t.Fatalf("InsertAPIRequestLog: %v", err)
		}
	}

	cutoff := time.Now().AddDate(0, 0, -3).Unix()
	n, err := s.CleanupAPILogBodies(cutoff)
	if err != nil {
		t.Fatalf("CleanupAPILogBodies: %v", err)
	}
	if n != 1 {
		t.Fatalf("rows updated = %d, want 1", n)
	}

	rows, total, err := s.QueryAPIRequestLogs(Filter{})
	if err != nil || total != 2 {
		t.Fatalf("rows after cleanup = %d err=%v, want both rows kept", total, err)
	}
	for _, row := range rows {
		switch row.ReqID {
		case "req-old":
			if row.BodyReq != "" || row.BodyResp != "" {
				t.Fatalf("old bodies not cleared: %+v", row)
			}
		case "req-new":
			if row.BodyReq != "new-request" || row.BodyResp != "new-response" {
				t.Fatalf("recent bodies lost: %+v", row)
			}
		}
	}

	// Body lookup still resolves the row after the bodies were dropped.
	req, _, found, err := s.APILogBodies("req-old")
	if err != nil || !found || req != "" {
		t.Fatalf("APILogBodies old = %q found=%v err=%v", req, found, err)
	}
}

func TestPruneAuditRemovesOnlyOldRows(t *testing.T) {
	s := openTestStore(t)
	old := time.Now().AddDate(0, 0, -40)
	middle := time.Now().AddDate(0, 0, -5)
	for _, at := range []time.Time{old, middle} {
		if _, err := s.AppendAudit(model.AuditEntry{
			TS: at.Format(time.RFC3339), Category: model.AuditAccount,
			Action: "rename", Target: "mock/acc_1", Result: "ok",
		}); err != nil {
			t.Fatalf("append audit: %v", err)
		}
	}
	// An entry without an explicit ts gets the current time and must survive.
	if _, err := s.AppendAudit(model.AuditEntry{Category: model.AuditGroup, Action: "delete", Target: "grp_1"}); err != nil {
		t.Fatalf("append audit: %v", err)
	}

	removed, err := s.PruneAudit(time.Now().AddDate(0, 0, -30).Unix())
	if err != nil {
		t.Fatalf("prune audit: %v", err)
	}
	if removed != 1 {
		t.Fatalf("rows removed = %d, want 1", removed)
	}
	entries, err := s.ListAudit("", 10)
	if err != nil || len(entries) != 2 {
		t.Fatalf("entries after prune = %+v, err %v; want the two recent rows", entries, err)
	}
	for _, entry := range entries {
		if entry.TS == old.Format(time.RFC3339) {
			t.Fatalf("old entry survived the prune: %+v", entry)
		}
	}

	if _, err := s.PruneAudit(0); err == nil {
		t.Fatal("prune with a zero cutoff accepted")
	}
}

func TestCleanupUsageDaily(t *testing.T) {
	s := openTestStore(t)
	for _, day := range []string{"2026-01-01", "2026-06-01"} {
		if _, err := s.DB().Exec(`INSERT INTO usage_daily
			(day, plugin_id, account_id, model, requests) VALUES (?, 'plugin:mock', 'acc', 'm', 1)`, day); err != nil {
			t.Fatalf("insert usage: %v", err)
		}
	}
	n, err := s.CleanupUsageDaily("2026-05-01")
	if err != nil || n != 1 {
		t.Fatalf("CleanupUsageDaily = %d, %v; want 1", n, err)
	}
	rows, err := s.UsageSeriesOf(UsageFilter{})
	if err != nil || len(rows) != 1 || rows[0].Day != "2026-06-01" {
		t.Fatalf("series after cleanup = %+v err=%v", rows, err)
	}
}
