package store

import (
	"errors"
	"testing"
	"time"

	"aiworkgate/internal/model"
)

// seedFilterLogs inserts five rows that differ along every filter dimension.
func seedFilterLogs(t *testing.T, s *Store) {
	t.Helper()
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	rows := []model.APIRequestLog{
		{TS: base.Format(time.RFC3339), ReqID: "req-1", Source: "plugin:mock", Model: "Claude-3.5-Sonnet",
			Account: "acc_1", KeyID: "key-1", KeyName: "laptop", Stream: true, Status: 200,
			LatencyMS: 120, TTFBMS: 40, PromptTokens: 10, CompletionTokens: 5, CachedTokens: 2,
			BodyReq: "secret-request", BodyResp: "secret-response"},
		{TS: base.Add(time.Minute).Format(time.RFC3339), ReqID: "req-2", Source: "custom:cm-1", Model: "my-custom-model",
			Account: "acc_2", KeyID: "key-2", KeyName: "ci", Stream: false, Status: 401},
		{TS: base.Add(2 * time.Minute).Format(time.RFC3339), ReqID: "req-3", Source: "a", Model: "gpt-4o",
			Account: "acc_1", KeyID: "key-1", KeyName: "laptop", Stream: false, Status: 500,
			Error: "upstream exploded"},
		{TS: base.Add(3 * time.Minute).Format(time.RFC3339), ReqID: "req-4", Source: "plugin:mock", Model: "gpt-4o-mini",
			Account: "acc_2", KeyID: "key-2", KeyName: "ci", Stream: true, Status: 0},
		{TS: base.Add(4 * time.Minute).Format(time.RFC3339), ReqID: "req-5", Source: "plugin:other", Model: "claude-3-opus",
			Stream: false, Status: 302},
	}
	for _, rec := range rows {
		if err := s.InsertAPIRequestLog(rec); err != nil {
			t.Fatalf("insert %s: %v", rec.ReqID, err)
		}
	}
}

func reqIDs(rows []model.APIRequestLog) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.ReqID)
	}
	return out
}

func TestAPIRequestLogFilters(t *testing.T) {
	s := openTestStore(t)
	seedFilterLogs(t, s)

	cases := []struct {
		name   string
		filter Filter
		want   []string
	}{
		{"model substring case-insensitive", Filter{Model: "CLAUDE"}, []string{"req-5", "req-1"}},
		{"model partial token", Filter{Model: "gpt-4o"}, []string{"req-4", "req-3"}},
		{"model literal wildcard", Filter{Model: "%"}, nil},
		{"source plugin", Filter{Source: "plugin:mock"}, []string{"req-4", "req-1"}},
		{"source custom model", Filter{Source: "custom:cm-1"}, []string{"req-2"}},
		{"account", Filter{Account: "acc_2"}, []string{"req-4", "req-2"}},
		{"client key", Filter{KeyID: "key-1"}, []string{"req-3", "req-1"}},
		{"status 2xx", Filter{StatusClass: StatusClass2xx}, []string{"req-1"}},
		{"status 4xx", Filter{StatusClass: StatusClass4xx}, []string{"req-2"}},
		{"status 5xx", Filter{StatusClass: StatusClass5xx}, []string{"req-3"}},
		{"status error covers transport and 4xx/5xx", Filter{StatusClass: StatusClassError}, []string{"req-4", "req-3", "req-2"}},
		{"stream true", Filter{Stream: boolPtr(true)}, []string{"req-4", "req-1"}},
		{"stream false", Filter{Stream: boolPtr(false)}, []string{"req-5", "req-3", "req-2"}},
		{"keyword across columns", Filter{Keyword: "UPSTREAM"}, []string{"req-3"}},
		{"keyword account substring", Filter{Keyword: "acc_1"}, []string{"req-3", "req-1"}},
		{
			"combination source+model+stream",
			Filter{Source: "a", Model: "gpt", Stream: boolPtr(false)},
			[]string{"req-3"},
		},
		{
			"combination account+status_class+key",
			Filter{Account: "acc_2", StatusClass: StatusClassError, KeyID: "key-2"},
			[]string{"req-4", "req-2"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, total, err := s.QueryAPIRequestLogs(tc.filter)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			got := reqIDs(rows)
			if total != len(tc.want) || len(got) != len(tc.want) {
				t.Fatalf("got %v (total %d), want %v", got, total, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestAPIRequestLogFilterPaginationAndValidation(t *testing.T) {
	s := openTestStore(t)
	seedFilterLogs(t, s)

	rows, total, err := s.QueryAPIRequestLogs(Filter{Model: "gpt", Limit: 1, Offset: 1})
	if err != nil {
		t.Fatalf("paged query: %v", err)
	}
	if total != 2 || len(rows) != 1 || rows[0].ReqID != "req-3" {
		t.Fatalf("paged = %v total %d, want [req-3] / 2", reqIDs(rows), total)
	}

	if _, _, err := s.QueryAPIRequestLogs(Filter{StatusClass: "3xx"}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("unknown status_class error = %v, want ErrInvalidArgument", err)
	}
}

func TestExportAPIRequestLogs(t *testing.T) {
	s := openTestStore(t)
	seedFilterLogs(t, s)

	rows, truncated, err := s.ExportAPIRequestLogs(Filter{}, 10)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if truncated || len(rows) != 5 {
		t.Fatalf("export rows = %d truncated=%v, want 5/false", len(rows), truncated)
	}

	// Bodies never leave the store through the export path.
	rows, _, err = s.ExportAPIRequestLogs(Filter{Source: "plugin:mock"}, 10)
	if err != nil {
		t.Fatalf("export filtered: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("filtered export rows = %d, want 2", len(rows))
	}
	for _, row := range rows {
		if row.BodyReq != "" || row.BodyResp != "" {
			t.Fatalf("export leaked body for %s", row.ReqID)
		}
	}

	capped, truncated, err := s.ExportAPIRequestLogs(Filter{Source: "plugin:mock"}, 1)
	if err != nil {
		t.Fatalf("capped export: %v", err)
	}
	if !truncated || len(capped) != 1 || capped[0].ReqID != "req-4" {
		t.Fatalf("capped export = %v truncated=%v, want [req-4]/true", reqIDs(capped), truncated)
	}

	if _, _, err := s.ExportAPIRequestLogs(Filter{StatusClass: "nope"}, 10); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("invalid class export error = %v, want ErrInvalidArgument", err)
	}
}
