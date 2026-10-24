package store

import (
	"errors"
	"testing"
)

func TestCleanupClientKeyUsageKeepsDeletedKeyHistoryWithinWindow(t *testing.T) {
	s := openTestStore(t)
	if err := s.UpsertClientKey("k1", "retired", "agk_a", "hash-1", true, 0); err != nil {
		t.Fatalf("UpsertClientKey: %v", err)
	}
	if err := s.IncrementKeyUsage("k1", "2026-01-01", 1, 10, 5, 2, nil); err != nil {
		t.Fatalf("increment old: %v", err)
	}
	if err := s.IncrementKeyUsage("k1", "2026-06-01", 1, 10, 5, 2, nil); err != nil {
		t.Fatalf("increment recent: %v", err)
	}
	// Deleting the key keeps its counters, so the ranking keeps the history.
	if err := s.DeleteClientKey("k1"); err != nil {
		t.Fatalf("DeleteClientKey: %v", err)
	}
	var kept int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM client_key_usage WHERE key_id = 'k1'`).Scan(&kept); err != nil {
		t.Fatalf("count usage: %v", err)
	}
	if kept != 2 {
		t.Fatalf("usage rows after delete = %d, want 2 kept for history", kept)
	}

	n, err := s.CleanupClientKeyUsage("2026-05-01")
	if err != nil || n != 1 {
		t.Fatalf("CleanupClientKeyUsage = %d/%v, want 1", n, err)
	}
	var day string
	if err := s.DB().QueryRow(`SELECT day FROM client_key_usage WHERE key_id = 'k1'`).Scan(&day); err != nil {
		t.Fatalf("read remaining usage: %v", err)
	}
	if day != "2026-06-01" {
		t.Fatalf("remaining day = %q, want 2026-06-01", day)
	}

	if _, err := s.CleanupClientKeyUsage(" "); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty day = %v, want ErrInvalidArgument", err)
	}
}
