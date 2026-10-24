package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aiworkgate/internal/model"
	"aiworkgate/internal/store"
)

// Benchmark dataset sizes. They approximate a busy install: enough rows and
// log lines that a full-window scan would be visible, while a cursor page only
// touches the newest pageSize+1 events per source.
const (
	benchTimelineDays        = 25
	benchTimelineAPIRows     = 1000
	benchTimelineAuditRows   = 5000
	benchTimelineLinesPerDay = 2000
	benchTimelineWindowDays  = 7 // defaultTimelineDays
)

// BenchmarkTimelineQueryCurrent measures the first page over a realistic
// 4-source dataset: api_logs, audit_log, host JSONL and plugin log files.
func BenchmarkTimelineQueryCurrent(b *testing.B) {
	tl, filter := benchTimeline(b, benchTimelineWindowDays)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		page, err := tl.Query(filter, "", maxTimelinePageSize)
		if err != nil {
			b.Fatalf("Query: %v", err)
		}
		if len(page.Items) == 0 {
			b.Fatal("Query returned no events; benchmark data was not loaded")
		}
	}
}

// BenchmarkTimelineQueryLongWindow pages across a window wide enough that the
// file sources hold far more lines than one page.
func BenchmarkTimelineQueryLongWindow(b *testing.B) {
	tl, filter := benchTimeline(b, benchTimelineDays)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		page, err := tl.Query(filter, "", maxTimelinePageSize)
		if err != nil {
			b.Fatalf("Query: %v", err)
		}
		if len(page.Items) == 0 {
			b.Fatal("Query returned no events; benchmark data was not loaded")
		}
	}
}

// BenchmarkTimelineQueryDeepPage measures page 21: with keyset paging its cost
// should match page 1 instead of re-reading every earlier event.
func BenchmarkTimelineQueryDeepPage(b *testing.B) {
	tl, filter := benchTimeline(b, benchTimelineWindowDays)
	cursor := ""
	for i := 0; i < 20; i++ {
		page, err := tl.Query(filter, cursor, maxTimelinePageSize)
		if err != nil {
			b.Fatalf("prime page %d: %v", i, err)
		}
		if page.NextCursor == "" {
			b.Fatalf("dataset ended before the deep page (page %d)", i)
		}
		cursor = page.NextCursor
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		page, err := tl.Query(filter, cursor, maxTimelinePageSize)
		if err != nil || len(page.Items) == 0 {
			b.Fatalf("deep page: %d items, err=%v", len(page.Items), err)
		}
	}
}

// benchTimeline builds the dataset on disk and returns a ready Timeline plus a
// filter that covers the last windowDays of it.
func benchTimeline(b *testing.B, windowDays int) (*Timeline, TimelineFilter) {
	b.Helper()
	dir, err := os.MkdirTemp("", "aiworkgate-timeline-bench-*")
	if err != nil {
		b.Fatalf("create temp dir: %v", err)
	}
	b.Cleanup(func() {
		for i := 0; i < 50; i++ {
			if err := os.RemoveAll(dir); err == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		_ = os.RemoveAll(dir)
	})
	st, err := store.Open(dir)
	if err != nil {
		b.Fatalf("store.Open: %v", err)
	}
	b.Cleanup(func() { _ = st.Close() })

	day := fixedNow().Format(dateLayout)
	firstDay := fixedNow().AddDate(0, 0, -(windowDays - 1)).Format(dateLayout)

	if err := seedBenchAPILogs(st); err != nil {
		b.Fatalf("seed api logs: %v", err)
	}
	if err := seedBenchAudit(st); err != nil {
		b.Fatalf("seed audit: %v", err)
	}
	writeBenchHostLogs(b, dir)
	writeBenchPluginLogs(b, dir)

	tl := NewTimeline(st, dir, func() []string { return []string{"mock"} })
	tl.Now = fixedNow
	return tl, TimelineFilter{FromDay: firstDay, ToDay: day}
}

func seedBenchAPILogs(st *store.Store) error {
	for i := 0; i < benchTimelineAPIRows; i++ {
		at := fixedNow().Add(-time.Duration(i) * 30 * time.Second)
		status := 200
		if i%50 == 0 {
			status = 502
		}
		if err := st.InsertAPIRequestLog(model.APIRequestLog{
			TS:           at.Format(time.RFC3339),
			ReqID:        fmt.Sprintf("req-%05d", i),
			Source:       "plugin:mock",
			Account:      fmt.Sprintf("acc_%02d", i%8),
			Model:        "mock/model-bench",
			LogicalModel: "bench-logical",
			Status:       status,
			LatencyMS:    int64(120 + i%800),
			Stream:       true,
		}); err != nil {
			return err
		}
	}
	return nil
}

func seedBenchAudit(st *store.Store) error {
	for i := 0; i < benchTimelineAuditRows; i++ {
		at := fixedNow().Add(-time.Duration(i) * 25 * time.Second)
		result := "ok"
		if i%7 == 0 {
			result = "error: denied"
		}
		if _, err := st.AppendAudit(model.AuditEntry{
			TS:       at.Format(time.RFC3339),
			Category: "config",
			Action:   "update",
			Target:   fmt.Sprintf("setting-%04d", i),
			Detail:   "benchmark audit entry",
			Result:   result,
		}); err != nil {
			return err
		}
	}
	return nil
}

func writeBenchHostLogs(b *testing.B, dir string) {
	b.Helper()
	for d := 0; d < benchTimelineDays; d++ {
		day := fixedNow().AddDate(0, 0, -d).Format(dateLayout)
		var sb strings.Builder
		sb.Grow(benchTimelineLinesPerDay * 160)
		for i := 0; i < benchTimelineLinesPerDay; i++ {
			at := fixedNow().AddDate(0, 0, -d).
				Add(time.Duration(i-benchTimelineLinesPerDay) * time.Second)
			module := "proxy"
			level := "info"
			msg := "request forwarded"
			switch i % 5 {
			case 0:
				module, msg = "schedule", "check-in ok"
			case 1:
				module, msg = "plugin", "plugin restarted"
			case 2:
				module, msg = "app", "settings saved"
			case 3:
				module, level, msg = "mystery", "error", "upstream 502"
			}
			fmt.Fprintf(&sb, `{"ts":%q,"level":%q,"module":%q,"msg":%q}`+"\n",
				at.Format(time.RFC3339), level, module, msg)
		}
		writeBenchFile(b, filepath.Join(dir, logsDirName, "host-"+day+".jsonl"), sb.String())
	}
}

func writeBenchPluginLogs(b *testing.B, dir string) {
	b.Helper()
	for d := 0; d < benchTimelineDays; d++ {
		day := fixedNow().AddDate(0, 0, -d).Format(dateLayout)
		var sb strings.Builder
		sb.Grow(benchTimelineLinesPerDay * 100)
		for i := 0; i < benchTimelineLinesPerDay; i++ {
			at := fixedNow().AddDate(0, 0, -d).
				Add(time.Duration(i-benchTimelineLinesPerDay) * time.Second)
			level := "INFO"
			if i%40 == 0 {
				level = "ERROR"
			}
			fmt.Fprintf(&sb, "%s %s captured line %d from plugin\n",
				at.Format("2006-01-02 15:04:05"), level, i)
		}
		writeBenchFile(b, filepath.Join(dir, logsDirName, pluginsDirName, "mock-"+day+".log"), sb.String())
	}
}

func writeBenchFile(b *testing.B, path, content string) {
	b.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		b.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		b.Fatalf("write %s: %v", path, err)
	}
}
