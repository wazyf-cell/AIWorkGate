package store

import "testing"

// TestConnectivityProbeRoundTrip covers insert/list ordering, both filters and
// the limit clamp.
func TestConnectivityProbeRoundTrip(t *testing.T) {
	s := openTestStore(t)
	const now = int64(1_700_000_000)

	rows := []ConnectivityProbe{
		{Kind: "a", Target: "a", TargetID: "127.0.0.1:7864", OK: true, Status: 200,
			LatencyMS: 12, Detail: "A 运行中 · 3 个模型", CreatedAt: now},
		{Kind: "plugin", Target: "officeace", TargetID: "officeace/glm-5.3", OK: true,
			Status: 200, LatencyMS: 42, Detail: "模型存在 · 9 个模型", CreatedAt: now + 1},
		{Kind: "plugin", Target: "officeace", TargetID: "officeace/missing", OK: false,
			Status: 200, LatencyMS: 7, Detail: "模型不存在 · 9 个模型",
			ErrorCode: "model_missing", CreatedAt: now + 2},
		{Kind: "custom_model", Target: "my-model", TargetID: "cm-abc", OK: false,
			Status: 401, LatencyMS: 30, Detail: "上游返回状态 401，请检查 API 地址、模型名和 API Key",
			ErrorCode: "upstream_error", CreatedAt: now + 3},
	}
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		id, err := s.InsertConnectivityProbe(row)
		if err != nil {
			t.Fatalf("insert %+v: %v", row, err)
		}
		if id <= 0 {
			t.Fatalf("insert id = %d, want positive", id)
		}
		ids = append(ids, id)
	}

	all, err := s.ListConnectivityProbes(20, "", "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != len(rows) {
		t.Fatalf("rows = %d, want %d (%+v)", len(all), len(rows), all)
	}
	// Newest first.
	if all[0].ID != ids[3] || all[3].ID != ids[0] {
		t.Fatalf("order = %v, want newest first", []int64{all[0].ID, all[3].ID})
	}
	if all[0].Kind != "custom_model" || all[0].OK || all[0].Status != 401 ||
		all[0].ErrorCode != "upstream_error" || all[0].TargetID != "cm-abc" {
		t.Fatalf("custom row = %+v", all[0])
	}
	if all[3].Detail != "A 运行中 · 3 个模型" || all[3].LatencyMS != 12 || !all[3].OK {
		t.Fatalf("a row = %+v", all[3])
	}

	byKind, err := s.ListConnectivityProbes(20, "plugin", "")
	if err != nil {
		t.Fatalf("list by kind: %v", err)
	}
	if len(byKind) != 2 || byKind[0].ID != ids[2] || byKind[1].ID != ids[1] {
		t.Fatalf("plugin rows = %+v", byKind)
	}
	byTarget, err := s.ListConnectivityProbes(20, "", "officeace/glm-5.3")
	if err != nil {
		t.Fatalf("list by target: %v", err)
	}
	if len(byTarget) != 1 || byTarget[0].ID != ids[1] {
		t.Fatalf("target rows = %+v", byTarget)
	}
	both, err := s.ListConnectivityProbes(20, "plugin", "officeace/missing")
	if err != nil || len(both) != 1 || both[0].ID != ids[2] {
		t.Fatalf("combined filters = %+v, %v", both, err)
	}

	limited, err := s.ListConnectivityProbes(1, "", "")
	if err != nil || len(limited) != 1 || limited[0].ID != ids[3] {
		t.Fatalf("limit 1 = %+v, %v", limited, err)
	}
	// Out-of-range limits clamp instead of failing: default 20, max 100.
	if got, err := s.ListConnectivityProbes(0, "", ""); err != nil || len(got) != 4 {
		t.Fatalf("default limit = %d rows, %v", len(got), err)
	}
	if got, err := s.ListConnectivityProbes(500, "", ""); err != nil || len(got) != 4 {
		t.Fatalf("clamped limit = %d rows, %v", len(got), err)
	}

	// An empty table serializes as an empty array, not null.
	empty, err := s.ListConnectivityProbes(20, "nope", "")
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("no-match list = %#v, %v; want non-nil empty", empty, err)
	}

	if _, err := s.InsertConnectivityProbe(ConnectivityProbe{Kind: ""}); err == nil {
		t.Fatal("insert without kind accepted")
	}
}

// TestConnectivityProbePruneOnInsert proves the documented retention: inserting
// a fresh row deletes rows older than 90 days and nothing newer.
func TestConnectivityProbePruneOnInsert(t *testing.T) {
	s := openTestStore(t)
	const now = int64(1_700_000_000)
	old := now - (ConnectivityProbeRetentionDays+1)*24*60*60

	if _, err := s.InsertConnectivityProbe(ConnectivityProbe{Kind: "a", TargetID: "old", CreatedAt: old}); err != nil {
		t.Fatalf("insert old: %v", err)
	}
	if _, err := s.InsertConnectivityProbe(ConnectivityProbe{Kind: "a", TargetID: "keep", CreatedAt: now}); err != nil {
		t.Fatalf("insert keep: %v", err)
	}
	// The old insert pruned relative to its own timestamp; only a live row
	// takes it out.
	rows, err := s.ListConnectivityProbes(20, "", "")
	if err != nil {
		t.Fatalf("list after prune: %v", err)
	}
	if len(rows) != 1 || rows[0].TargetID != "keep" {
		t.Fatalf("rows after prune = %+v, want only keep", rows)
	}

	// PruneConnectivityProbes is exported for explicit callers.
	if n, err := s.PruneConnectivityProbes(now + 1); err != nil || n != 1 {
		t.Fatalf("explicit prune = %d, %v; want 1", n, err)
	}
	if n, err := s.PruneConnectivityProbes(now + 1); err != nil || n != 0 {
		t.Fatalf("second prune = %d, %v; want 0", n, err)
	}
}
