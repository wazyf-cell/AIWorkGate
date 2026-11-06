package store

import (
	"testing"

	"aiworkgate/internal/model"
)

func TestUsageClearRecordsWithoutDeletingLogs(t *testing.T) {
	s := openTestStore(t)
	if err := s.InsertAPIRequestLog(model.APIRequestLog{
		TS: "2026-10-20T10:00:00+08:00", ReqID: "r1", Source: "plugin:p", Model: "m", Status: 200,
	}); err != nil {
		t.Fatalf("InsertAPIRequestLog: %v", err)
	}
	before, _, err := s.QueryAPILogsSince(0, "2026-10-19", "2026-10-21", 100)
	if err != nil {
		t.Fatalf("QueryAPILogsSince: %v", err)
	}

	first, err := s.RecordUsageClear(1_800_000_000, "  reset  ")
	if err != nil {
		t.Fatalf("RecordUsageClear: %v", err)
	}
	if first.ID == 0 || first.ClearedAt != 1_800_000_000 || first.Note != "reset" {
		t.Fatalf("clear = %+v", first)
	}
	second, err := s.RecordUsageClear(1_800_000_100, "")
	if err != nil {
		t.Fatalf("RecordUsageClear: %v", err)
	}
	if second.ClearedAt != 1_800_000_100 {
		t.Fatalf("second = %+v", second)
	}

	clears, err := s.ListUsageClears(10)
	if err != nil {
		t.Fatalf("ListUsageClears: %v", err)
	}
	if len(clears) != 2 || clears[0].ID != second.ID || clears[1].ID != first.ID {
		t.Fatalf("clears = %+v, want newest first", clears)
	}

	after, _, err := s.QueryAPILogsSince(0, "2026-10-19", "2026-10-21", 100)
	if err != nil {
		t.Fatalf("QueryAPILogsSince after clear: %v", err)
	}
	if len(after) != len(before) || len(after) != 1 {
		t.Fatalf("api_logs rows = before %d after %d, want 1 kept", len(before), len(after))
	}
}
