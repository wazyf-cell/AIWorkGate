package store

import (
	"strings"
	"testing"
	"time"
)

// explain returns the query plan of one statement as a single string.
func explain(t *testing.T, s *Store, query string, args ...any) string {
	t.Helper()
	rows, err := s.db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var (
			id, parent, notUsed int
			detail              string
		)
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("explain scan: %v", err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain iterate: %v", err)
	}
	return strings.Join(plan, " | ")
}

// The day bounds used to compare substr(ts, 1, 10), which wraps the column in a
// function and makes idx_api_logs_ts unusable: SQLite cannot seek on a computed
// value, so every usage query degraded to a full table scan. Half-open compares
// on the raw column keep the index available.
func TestAPILogDayFilterUsesTimestampIndex(t *testing.T) {
	s := openTestStore(t)
	today := time.Now().Format("2006-01-02")
	tomorrow, ok := nextDay(today)
	if !ok {
		t.Fatalf("nextDay(%q) did not parse", today)
	}

	sql := "SELECT id, model FROM api_logs WHERE " + dayRangeSQL("ts", today, today)
	plan := explain(t, s, sql, dayRangeArgs(today, today)...)
	if !strings.Contains(plan, "SEARCH") || !strings.Contains(plan, "idx_api_logs_ts") {
		t.Fatalf("api_logs day filter must SEARCH idx_api_logs_ts; plan = %s", plan)
	}

	// The old shape must not seek, otherwise this test could not tell a
	// regression from a pass. It may still name the index inside a SCAN (a
	// covering-index scan), which is exactly why SEARCH vs SCAN is the signal.
	oldPlan := explain(t, s,
		"SELECT id, model FROM api_logs WHERE substr(ts, 1, 10) >= ? AND substr(ts, 1, 10) <= ?",
		today, today)
	if strings.Contains(oldPlan, "SEARCH") {
		t.Fatalf("substr() form unexpectedly SEARCHed; plan = %s", oldPlan)
	}

	// Sanity: the exclusive upper bound must still include the whole day, so
	// the window has to span from today up to tomorrow, not just today.
	if tomorrow <= today {
		t.Fatalf("nextDay returned %q for %q", tomorrow, today)
	}
}

// Audit queries carry the same substr() problem through idx_audit_log_ts.
func TestAuditDayFilterUsesTimestampIndex(t *testing.T) {
	s := openTestStore(t)
	today := time.Now().Format("2006-01-02")

	plan := explain(t, s,
		"SELECT id FROM audit_log WHERE "+dayRangeSQL("ts", today, today),
		dayRangeArgs(today, today)...)
	if !strings.Contains(plan, "idx_audit_log_ts") {
		t.Fatalf("audit_log day filter does not use idx_audit_log_ts; plan = %s", plan)
	}
}

// An inclusive `ts <= day` compare silently drops every row of that day,
// because stored values always carry a time suffix. This test pins the
// off-by-one-day behaviour rather than trusting the reasoning in a comment.
func TestDayRangeUpperBoundIncludesWholeDay(t *testing.T) {
	s := openTestStore(t)
	today := time.Now().Format("2006-01-02")
	lateInDay := today + "T23:59:59+08:00"

	where, args := dayRangeConds(nil, nil, "ts", today, today)
	query := "SELECT COUNT(*) FROM api_logs WHERE " + strings.Join(where, " AND ")
	var count int
	if err := s.db.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("empty store returned %d rows", count)
	}

	insertLog(t, s, lateInDay, 200, 12, 0)

	where, args = dayRangeConds(nil, nil, "ts", today, today)
	query = "SELECT COUNT(*) FROM api_logs WHERE " + strings.Join(where, " AND ")
	if err := s.db.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatalf("count after seed: %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1: the day filter must include 23:59:59", count)
	}
}
