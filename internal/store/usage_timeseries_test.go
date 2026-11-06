package store

import (
	"testing"

	"aiworkgate/internal/model"
)

func TestUsageTimeSeriesBuckets(t *testing.T) {
	s := openTestStore(t)
	logs := []model.APIRequestLog{
		{TS: "2026-10-18T23:30:00+08:00", ReqID: "r1", Source: "plugin:p1", Model: "m1", Status: 200,
			PromptTokens: 10, CompletionTokens: 5},
		{TS: "2026-10-19T01:10:00+08:00", ReqID: "r2", Source: "plugin:p1", Model: "m1", Status: 200,
			PromptTokens: 20, CompletionTokens: 10},
	}
	for _, log := range logs {
		if err := s.InsertAPIRequestLog(log); err != nil {
			t.Fatalf("InsertAPIRequestLog: %v", err)
		}
	}

	cases := []struct {
		granularity string
		want        []string
	}{
		{"day", []string{"2026-10-18", "2026-10-19"}},
		{"hour", []string{"2026-10-18T23:00", "2026-10-19T01:00"}},
		// Sunday 10-18 belongs to the week starting Monday 10-12; Monday 10-19
		// starts the next one.
		{"week", []string{"2026-10-12", "2026-10-19"}},
	}
	for _, tc := range cases {
		rows, err := s.UsageTimeSeries("2026-10-12", "2026-10-20", tc.granularity)
		if err != nil {
			t.Fatalf("UsageTimeSeries(%s): %v", tc.granularity, err)
		}
		if len(rows) != len(tc.want) {
			t.Fatalf("%s rows = %d, want %d: %+v", tc.granularity, len(rows), len(tc.want), rows)
		}
		for i, want := range tc.want {
			if rows[i].Bucket != want {
				t.Fatalf("%s bucket %d = %q, want %q", tc.granularity, i, rows[i].Bucket, want)
			}
		}
	}
}
