package store

import (
	"fmt"
	"testing"
	"time"

	"aiworkgate/internal/model"
)

// wantBatches reports how many statements a backlog of n rows needs. It exists
// so the fixture below can assert it really spans more than one batch; without
// that check the test would pass even if the loop were never entered.
func wantBatches(n int64) int64 {
	if n <= 0 {
		return 0
	}
	return (n + cleanupBatch - 1) / cleanupBatch
}

// A long backlog must not be removed in one statement: that holds the SQLite
// write lock for the whole scan and starves the request-log inserts sharing the
// database. This test pins that a backlog larger than one batch is fully
// removed, which is only reachable through the loop.
func TestCleanupAPIRequestLogsDrainsMultipleBatches(t *testing.T) {
	s := openTestStore(t)
	old := time.Now().AddDate(0, 0, -30)
	total := cleanupBatch + 10
	for i := 0; i < total; i++ {
		if err := s.InsertAPIRequestLog(model.APIRequestLog{
			TS:     old.Add(time.Duration(i) * time.Second).Format(time.RFC3339),
			ReqID:  fmt.Sprintf("old-%d", i),
			Source: "plugin:mock",
			Model:  "m",
			Status: 200,
		}); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	// One recent row must survive.
	if err := s.InsertAPIRequestLog(model.APIRequestLog{
		TS:     time.Now().Format(time.RFC3339),
		ReqID:  "keep",
		Source: "plugin:mock",
		Model:  "m",
		Status: 200,
	}); err != nil {
		t.Fatalf("insert recent: %v", err)
	}

	cutoff := time.Now().AddDate(0, 0, -3).Unix()
	n, err := s.CleanupAPIRequestLogs(cutoff)
	if err != nil {
		t.Fatalf("CleanupAPIRequestLogs: %v", err)
	}
	if n != int64(total) {
		t.Fatalf("deleted = %d, want %d", n, total)
	}
	if wantBatches(int64(total)) < 2 {
		t.Fatalf("fixture too small: %d rows fit in one batch", total)
	}

	_, remaining, err := s.QueryAPIRequestLogs(Filter{})
	if err != nil {
		t.Fatalf("QueryAPIRequestLogs: %v", err)
	}
	if remaining != 1 {
		t.Fatalf("remaining rows = %d, want only the recent one", remaining)
	}
}

// A cleanup that fits in one batch must stop after the first statement, not
// spin: the loop exits on n < cleanupBatch.
func TestCleanupAPIRequestLogsStopsOnPartialBatch(t *testing.T) {
	s := openTestStore(t)
	old := time.Now().AddDate(0, 0, -30)
	for i := 0; i < 3; i++ {
		if err := s.InsertAPIRequestLog(model.APIRequestLog{
			TS:     old.Add(time.Duration(i) * time.Second).Format(time.RFC3339),
			ReqID:  fmt.Sprintf("old-%d", i),
			Source: "plugin:mock",
			Model:  "m",
			Status: 200,
		}); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	n, err := s.CleanupAPIRequestLogs(time.Now().AddDate(0, 0, -3).Unix())
	if err != nil {
		t.Fatalf("CleanupAPIRequestLogs: %v", err)
	}
	if n != 3 {
		t.Fatalf("deleted = %d, want 3", n)
	}
}
