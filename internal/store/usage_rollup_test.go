package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"aiworkgate/internal/model"
)

func rollAll(t *testing.T, s *Store) {
	t.Helper()
	rolled, _, more, err := s.RollUsage(context.Background(), time.Time{}, 1000)
	if err != nil {
		t.Fatalf("RollUsage: %v", err)
	}
	if more {
		t.Fatalf("RollUsage reported more batches after %d rows", rolled)
	}
}

func TestRollUsageCountsRejectionsAsNeutralAndTransportFailuresAsErrors(t *testing.T) {
	s := openTestStore(t)
	ts := time.Now().Format(time.RFC3339)
	for _, rec := range []model.APIRequestLog{
		{TS: ts, Source: "plugin:mock", Account: "acc", Model: "m", Status: 200},
		{TS: ts, Source: "plugin:mock", Account: "acc", Model: "m", Status: 500},
		{TS: ts, Source: "plugin:mock", Account: "acc", Model: "m", Status: 0}, // transport failure
		{TS: ts, Status: 401}, // key-auth rejection: never reached a model
		{TS: ts, Source: "custom:local", Account: "acc", Model: "m", Status: 200},
	} {
		if err := s.InsertAPIRequestLog(rec); err != nil {
			t.Fatalf("InsertAPIRequestLog %+v: %v", rec, err)
		}
	}
	rollAll(t, s)

	rows, err := s.UsageSeriesOf(UsageFilter{})
	if err != nil {
		t.Fatalf("UsageSeriesOf: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("usage rows = %+v, want plugin:mock and custom:local", rows)
	}
	for _, row := range rows {
		switch row.PluginID {
		case "plugin:mock":
			if row.Requests != 3 || row.Errors != 2 {
				t.Fatalf("plugin:mock = %+v, want 3 requests / 2 errors (status 0 and 500)", row)
			}
		case "custom:local":
			if row.Requests != 1 || row.Errors != 0 {
				t.Fatalf("custom:local = %+v, want 1 request / 0 errors", row)
			}
		default:
			t.Fatalf("rejection row leaked into usage_daily: %+v", row)
		}
	}
}

func TestRollUsageTTFBCountsOnlySuccessfulRequests(t *testing.T) {
	s := openTestStore(t)
	ts := time.Now().Format(time.RFC3339)
	for _, rec := range []model.APIRequestLog{
		{TS: ts, Source: "plugin:mock", Account: "acc", Model: "m", Status: 200, TTFBMS: 50},
		{TS: ts, Source: "plugin:mock", Account: "acc", Model: "m", Status: 500, TTFBMS: 70},
		{TS: ts, Source: "plugin:mock", Account: "acc", Model: "m", Status: 0, TTFBMS: 30},
	} {
		if err := s.InsertAPIRequestLog(rec); err != nil {
			t.Fatalf("InsertAPIRequestLog: %v", err)
		}
	}
	rollAll(t, s)

	totals, err := s.UsageTotalsOf(UsageFilter{})
	if err != nil {
		t.Fatalf("UsageTotalsOf: %v", err)
	}
	// The latency charts use status=200 only, so the rollup must match them.
	if totals.TTFBMSSum != 50 || totals.TTFBCount != 1 {
		t.Fatalf("ttfb = %d/%d, want 50/1 (only the status-200 row)", totals.TTFBMSSum, totals.TTFBCount)
	}
}

func TestRollUsageCreditsKeepNullSemantics(t *testing.T) {
	s := openTestStore(t)
	ts := time.Now().Format(time.RFC3339)
	credits := 1.25
	for _, rec := range []model.APIRequestLog{
		{TS: ts, Source: "plugin:mock", Account: "acc", Model: "unknown", Status: 200},
		{TS: ts, Source: "plugin:mock", Account: "acc", Model: "unknown", Status: 200},
		{TS: ts, Source: "plugin:mock", Account: "acc", Model: "mixed", Status: 200, Credits: &credits},
		{TS: ts, Source: "plugin:mock", Account: "acc", Model: "mixed", Status: 200},
	} {
		if err := s.InsertAPIRequestLog(rec); err != nil {
			t.Fatalf("InsertAPIRequestLog: %v", err)
		}
	}
	rollAll(t, s)

	var unknown, mixed sql.NullFloat64
	if err := s.DB().QueryRow(`SELECT credits FROM usage_daily WHERE model = 'unknown'`).Scan(&unknown); err != nil {
		t.Fatalf("read unknown credits: %v", err)
	}
	if unknown.Valid {
		t.Fatalf("unknown credits = %v, want NULL", unknown.Float64)
	}
	if err := s.DB().QueryRow(`SELECT credits FROM usage_daily WHERE model = 'mixed'`).Scan(&mixed); err != nil {
		t.Fatalf("read mixed credits: %v", err)
	}
	if !mixed.Valid || mixed.Float64 != credits {
		t.Fatalf("mixed credits = %+v, want the known 1.25", mixed)
	}

	rows, err := s.ModelUsageSummary(1)
	if err != nil {
		t.Fatalf("ModelUsageSummary: %v", err)
	}
	byModel := make(map[string]ModelUsageRow, len(rows))
	for _, row := range rows {
		byModel[row.Model] = row
	}
	if got := byModel["unknown"]; got.Credits != nil {
		t.Fatalf("unknown model credits = %v, want nil", *got.Credits)
	}
	if got := byModel["mixed"]; got.Credits == nil || *got.Credits != credits {
		t.Fatalf("mixed model credits = %v, want 1.25", got.Credits)
	}
}
