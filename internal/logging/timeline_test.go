package logging

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"aiworkgate/internal/model"
	"aiworkgate/internal/store"
)

func timelineTS(hour, minute int) time.Time {
	base := fixedNow()
	return time.Date(base.Year(), base.Month(), base.Day(), hour, minute, 0, 0, time.Local)
}

func newTestTimeline(t *testing.T) (*Timeline, *store.Store, string) {
	t.Helper()
	dir := testTempDir(t)
	st := openTestStore(t)
	tl := NewTimeline(st, dir, func() []string { return []string{"mock"} })
	tl.Now = fixedNow
	return tl, st, dir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func seedTimelineSources(t *testing.T, st *store.Store, dir string) {
	t.Helper()
	if err := st.InsertAPIRequestLog(model.APIRequestLog{
		TS: timelineTS(9, 0).Format(time.RFC3339), ReqID: "req-1", Source: "plugin:mock",
		Account: "acc_1", Model: "m1", Status: 200, LatencyMS: 120,
	}); err != nil {
		t.Fatalf("api log: %v", err)
	}
	if _, err := st.AppendAudit(model.AuditEntry{
		TS: timelineTS(9, 1).Format(time.RFC3339), Category: "account",
		Action: "rename", Target: "mock/acc_1", Result: "ok",
	}); err != nil {
		t.Fatalf("audit: %v", err)
	}

	hostLines := []string{
		`{"ts":"` + timelineTS(9, 2).Format(time.RFC3339) + `","level":"info","module":"schedule","msg":"check-in ok"}`,
		`{"ts":"` + timelineTS(9, 2).Add(time.Second).Format(time.RFC3339) + `","level":"info","module":"schedule","msg":"picked account"}`,
		`{"ts":"` + timelineTS(9, 2).Add(2*time.Second).Format(time.RFC3339) + `","level":"info","module":"plugin","msg":"restarted ok"}`,
		`{"ts":"` + timelineTS(9, 2).Add(3*time.Second).Format(time.RFC3339) + `","level":"info","module":"egress","msg":"proxy dial"}`,
		`{"ts":"` + timelineTS(9, 2).Add(4*time.Second).Format(time.RFC3339) + `","level":"info","module":"app","msg":"settings saved"}`,
		`{"ts":"` + timelineTS(9, 2).Add(5*time.Second).Format(time.RFC3339) + `","level":"info","module":"login","msg":"session started"}`,
		`{"ts":"` + timelineTS(9, 2).Add(6*time.Second).Format(time.RFC3339) + `","level":"error","module":"mystery","msg":"boom"}`,
	}
	hostPath := filepath.Join(dir, "logs", "host-"+fixedNow().Format(dateLayout)+".jsonl")
	writeFile(t, hostPath, joinLines(hostLines))

	pluginPath := filepath.Join(dir, "logs", "plugins", "mock-"+fixedNow().Format(dateLayout)+".log")
	writeFile(t, pluginPath, "2026-10-21 09:03:00 plugin ready\nplain line without time\n")
}

func joinLines(lines []string) string {
	out := ""
	for _, line := range lines {
		out += line + "\n"
	}
	return out
}

func TestTimelineMergesSourcesAndMapsCategories(t *testing.T) {
	tl, st, dir := newTestTimeline(t)
	seedTimelineSources(t, st, dir)

	filter := TimelineFilter{FromDay: fixedNow().Format(dateLayout), ToDay: fixedNow().Format(dateLayout)}
	page, err := tl.Query(filter, "", 200)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Items) != 11 {
		t.Fatalf("items = %d, want 11 (%+v)", len(page.Items), page.Items)
	}

	bySummary := map[string]TimelineItem{}
	for _, item := range page.Items {
		bySummary[item.Summary] = item
	}
	want := map[string]string{
		"m1 → 200":               TimelineCategoryRequest,
		"rename mock/acc_1 [ok]": TimelineCategoryAudit,
		"check-in ok":            TimelineCategoryCheckin,
		"picked account":         TimelineCategoryScheduler,
		"restarted ok":           TimelineCategoryPlugin,
		"proxy dial":             TimelineCategoryProxy,
		"settings saved":         TimelineCategoryConfig,
		"session started":        TimelineCategoryLogin,
		"boom":                   TimelineCategoryError,
	}
	for summary, category := range want {
		item, ok := bySummary[summary]
		if !ok {
			t.Fatalf("missing event %q in %+v", summary, page.Items)
		}
		if item.Category != category {
			t.Fatalf("%q category = %q, want %q", summary, item.Category, category)
		}
	}
	if item := bySummary["boom"]; item.Level != LevelError || item.Type != TimelineRuntime {
		t.Fatalf("error line = %+v", item)
	}
	if item := bySummary["m1 → 200"]; item.ReqID != "req-1" || item.Account != "acc_1" || item.Status != 200 {
		t.Fatalf("api item = %+v", item)
	}
	if item := bySummary["rename mock/acc_1 [ok]"]; item.Type != TimelineAudit || item.Source != "account" {
		t.Fatalf("audit item = %+v", item)
	}
	// Plugin logs default to info; an untimestamped line falls back to the day.
	var pluginLines int
	for _, item := range page.Items {
		if item.Type == TimelineRuntime && item.Source == "plugin:mock" {
			pluginLines++
			if item.Category != TimelineCategoryPlugin || item.Level != LevelInfo {
				t.Fatalf("plugin item = %+v", item)
			}
		}
	}
	if pluginLines != 2 {
		t.Fatalf("plugin lines = %d, want 2", pluginLines)
	}
	// Newest first: the plugin log (09:03) leads.
	if page.Items[0].Source != "plugin:mock" {
		t.Fatalf("first item = %+v, want the plugin line", page.Items[0])
	}
}

func TestTimelineFiltersKeywordLevelCategoryTypeAndPaging(t *testing.T) {
	tl, st, dir := newTestTimeline(t)
	seedTimelineSources(t, st, dir)
	day := fixedNow().Format(dateLayout)

	filter := TimelineFilter{FromDay: day, ToDay: day, Query: "CHECK"}
	page, err := tl.Query(filter, "", 200)
	if err != nil {
		t.Fatalf("keyword query: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Summary != "check-in ok" {
		t.Fatalf("keyword result = %+v", page)
	}

	filter = TimelineFilter{FromDay: day, ToDay: day, Level: "error"}
	page, _ = tl.Query(filter, "", 200)
	if len(page.Items) != 1 || page.Items[0].Category != TimelineCategoryError {
		t.Fatalf("level filter = %+v", page)
	}

	filter = TimelineFilter{FromDay: day, ToDay: day, Categories: []string{"plugin"}}
	page, _ = tl.Query(filter, "", 200)
	if len(page.Items) != 3 {
		t.Fatalf("category filter items = %d, want 3", len(page.Items))
	}

	filter = TimelineFilter{FromDay: day, ToDay: day, Types: []string{"api"}}
	page, _ = tl.Query(filter, "", 200)
	if len(page.Items) != 1 || page.Items[0].Type != TimelineAPI {
		t.Fatalf("type filter = %+v", page)
	}

	filter = TimelineFilter{FromDay: day, ToDay: day, Types: []string{"audit"}}
	page, _ = tl.Query(filter, "", 200)
	if len(page.Items) != 1 || page.Items[0].Type != TimelineAudit {
		t.Fatalf("audit filter = %+v", page)
	}

	filter = TimelineFilter{FromDay: day, ToDay: day}
	first, err := tl.Query(filter, "", 3)
	if err != nil {
		t.Fatalf("paging: %v", err)
	}
	if !first.HasMore || first.NextCursor == "" || len(first.Items) != 3 {
		t.Fatalf("first page = %+v", first)
	}
	second, err := tl.Query(filter, first.NextCursor, 3)
	if err != nil {
		t.Fatalf("paging: %v", err)
	}
	if len(second.Items) != 3 {
		t.Fatalf("second page items = %d, want 3", len(second.Items))
	}
	seen := map[string]bool{}
	for _, item := range append(append([]TimelineItem{}, first.Items...), second.Items...) {
		key := item.TS + "|" + item.Type + "|" + item.Summary
		if seen[key] {
			t.Fatalf("pages overlap at %q", key)
		}
		seen[key] = true
	}
}

func TestTimelinePollAdvancesCursor(t *testing.T) {
	tl, st, dir := newTestTimeline(t)
	seedTimelineSources(t, st, dir)
	day := fixedNow().Format(dateLayout)
	filter := TimelineFilter{FromDay: day, ToDay: day}

	cursor, err := tl.Seed(filter)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	// Nothing new yet.
	items, cursor, err := tl.Poll(filter, cursor, 100)
	if err != nil || len(items) != 0 {
		t.Fatalf("empty poll = %d items err=%v", len(items), err)
	}

	// Append one api row, one audit entry and one host line.
	if err := st.InsertAPIRequestLog(model.APIRequestLog{
		TS: timelineTS(10, 0).Format(time.RFC3339), ReqID: "req-2", Source: "plugin:mock",
		Model: "m2", Status: 500,
	}); err != nil {
		t.Fatalf("api log: %v", err)
	}
	if _, err := st.AppendAudit(model.AuditEntry{
		TS: timelineTS(10, 0).Format(time.RFC3339), Category: "config", Action: "save",
	}); err != nil {
		t.Fatalf("audit: %v", err)
	}
	hostPath := filepath.Join(dir, "logs", "host-"+day+".jsonl")
	f, err := os.OpenFile(hostPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open host log: %v", err)
	}
	if _, err := f.WriteString(`{"ts":"` + timelineTS(10, 1).Format(time.RFC3339) + `","level":"info","module":"plugin","msg":"new line"}` + "\n"); err != nil {
		t.Fatalf("append host log: %v", err)
	}
	_ = f.Close()

	items, cursor, err = tl.Poll(filter, cursor, 100)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("poll items = %d, want 3: %+v", len(items), items)
	}
	if items[0].Type != TimelineAPI || items[len(items)-1].Summary != "new line" {
		t.Fatalf("poll order = %+v", items)
	}

	items, _, err = tl.Poll(filter, cursor, 100)
	if err != nil || len(items) != 0 {
		t.Fatalf("second poll = %d items err=%v", len(items), err)
	}

	encoded := cursor.Encode()
	decoded, err := DecodeTimelineCursor(encoded)
	if err != nil || decoded.API != cursor.API || decoded.Audit != cursor.Audit {
		t.Fatalf("cursor round trip = %+v err=%v", decoded, err)
	}
}

func TestParseTimelineFilter(t *testing.T) {
	filter, err := ParseTimelineFilter(nil, fixedNow())
	if err != nil {
		t.Fatalf("ParseTimelineFilter: %v", err)
	}
	if filter.FromDay != "2026-10-15" || filter.ToDay != "2026-10-21" {
		t.Fatalf("default range = %s..%s", filter.FromDay, filter.ToDay)
	}
	values := map[string][]string{
		"type":      {"api,runtime"},
		"category":  {"plugin"},
		"level":     {"Error"},
		"q":         {" boom "},
		"cursor":    {"abc"},
		"page_size": {"999"},
	}
	filter, err = ParseTimelineFilter(values, fixedNow())
	if err != nil {
		t.Fatalf("ParseTimelineFilter: %v", err)
	}
	if len(filter.Types) != 2 || len(filter.Categories) != 1 || filter.Level != "error" || filter.Query != "boom" {
		t.Fatalf("filter = %+v", filter)
	}
	cursor, page, pageSize, err := ParseTimelinePage(values)
	if err != nil {
		t.Fatalf("ParseTimelinePage: %v", err)
	}
	if cursor != "abc" || page != 1 || pageSize != maxTimelinePageSize {
		t.Fatalf("page parse = %q/%d/%d", cursor, page, pageSize)
	}
	// The table endpoints still page by number.
	_, page, _, err = ParseTimelinePage(map[string][]string{"page": {"3"}, "page_size": {"5"}})
	if err != nil || page != 3 {
		t.Fatalf("legacy page parse = %d, %v", page, err)
	}
}

func TestParseTimeBoundAcceptsDatesAndTimestamps(t *testing.T) {
	loc := time.Local
	day, at, err := parseTimeBound("2026-10-20", loc)
	if err != nil || !at.IsZero() || day.Hour() != 0 {
		t.Fatalf("date bound = %v, %v, %v", day, at, err)
	}
	parsed, precise, err := parseTimeBound("2026-10-20T17:00:00+08:00", loc)
	if err != nil || precise.IsZero() {
		t.Fatalf("timestamp bound = %v, %v, %v", parsed, precise, err)
	}
	if precise.Hour() != 17 || parsed.Format(dateLayout) != "2026-10-20" {
		t.Fatalf("timestamp bound parsed as %v / %v", parsed, precise)
	}
}

// ------------------------------------------------------------------ paging

// writeHostDays seeds dayCount host JSONL files of lineCount lines each, one
// file per day ending at fixedNow.
func writeHostDays(t *testing.T, dir string, dayCount, lineCount int) {
	t.Helper()
	for d := 0; d < dayCount; d++ {
		day := fixedNow().AddDate(0, 0, -d)
		lines := make([]string, 0, lineCount)
		for i := 0; i < lineCount; i++ {
			at := day.Add(time.Duration(i-lineCount) * time.Second)
			lines = append(lines, `{"ts":"`+at.Format(time.RFC3339)+
				`","level":"info","module":"app","msg":"host line"}`)
		}
		path := filepath.Join(dir, "logs", "host-"+day.Format(dateLayout)+".jsonl")
		writeFile(t, path, joinLines(lines))
	}
}

// dayRange returns the filter covering the last dayCount days ending at
// fixedNow.
func dayRange(dayCount int) TimelineFilter {
	return TimelineFilter{
		FromDay: fixedNow().AddDate(0, 0, -(dayCount - 1)).Format(dateLayout),
		ToDay:   fixedNow().Format(dateLayout),
	}
}

func eventKey(item TimelineItem) string {
	return item.TS + "|" + item.Type + "|" + item.Source + "|" + item.Summary
}

// TestTimelineCursorWalksEveryEventOnce is the core pagination contract: page
// N resumes exactly where N-1 stopped, with no duplicate and no gap.
func TestTimelineCursorWalksEveryEventOnce(t *testing.T) {
	tl, _, dir := newTestTimeline(t)
	const days, perDay = 3, 150
	writeHostDays(t, dir, days, perDay)
	want := days * perDay

	seen := map[string]bool{}
	var lastTS time.Time
	cursor := ""
	pages := 0
	for {
		page, err := tl.Query(dayRange(days), cursor, 40)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		for _, item := range page.Items {
			key := eventKey(item)
			if seen[key] {
				t.Fatalf("event %q returned twice", key)
			}
			seen[key] = true
			at := parseTS(item.TS)
			if !lastTS.IsZero() && at.After(lastTS) {
				t.Fatalf("page order broke: %s after %s", item.TS, lastTS.Format(time.RFC3339))
			}
			lastTS = at
		}
		if page.NextCursor == "" {
			if page.HasMore {
				t.Fatal("HasMore is true but NextCursor is empty")
			}
			break
		}
		cursor = page.NextCursor
		if pages > 30 {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(seen) != want {
		t.Fatalf("walked %d events, want %d (pages=%d)", len(seen), want, pages)
	}
}

// The old reader kept only the last 2000 lines of a file, so a busy day was
// permanently truncated. Cursor paging must reach the oldest line.
func TestTimelineFilePagingReachesEveryLine(t *testing.T) {
	tl, _, dir := newTestTimeline(t)
	const lines = 3000
	writeHostDays(t, dir, 1, lines)

	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		page, err := tl.Query(dayRange(1), cursor, maxTimelinePageSize)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		for _, item := range page.Items {
			seen[eventKey(item)] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		if pages > 40 {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(seen) != lines {
		t.Fatalf("walked %d distinct events, want %d (pages=%d)", len(seen), lines, pages)
	}
}

// A resumed read must dedupe by identity, not by time. A page can fill up from
// another source before it reaches lines of this file, so "newer than the
// boundary" is not proof that an event was already emitted: dropping those
// lines loses them silently and can leave the next page with nothing to
// advance on (a zero boundary used to encode as an invalid year-1 cursor).
func TestTimelineResumeKeepsUnseenLinesNewerThanBoundary(t *testing.T) {
	tl, _, dir := newTestTimeline(t)
	day := fixedNow().Format(dateLayout)
	path := filepath.Join(dir, "logs", "plugins", "mock-"+day+".log")
	writeFile(t, path, "2026-10-21 09:30:00 a\n2026-10-21 09:31:00 b\n2026-10-21 09:32:00 c\n")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	cursor := EncodePageCursor(PageCursor{
		At:    timelineTS(9, 30).Add(30 * time.Second).UnixMilli(),
		Files: map[string]int64{path: info.Size()},
	})
	page, err := tl.Query(dayRange(1), cursor, 50)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Items) != 3 {
		t.Fatalf("resumed page = %d items, want 3", len(page.Items))
	}

	// Replaying the same lines with their identities seen must yield nothing.
	again := EncodePageCursor(PageCursor{
		At:    page.Items[len(page.Items)-1].at.UnixMilli(),
		Files: map[string]int64{path: info.Size()},
		Seen:  []string{fileItemKey(page.Items[0]), fileItemKey(page.Items[1]), fileItemKey(page.Items[2])},
	})
	page, err = tl.Query(dayRange(1), again, 50)
	if err != nil {
		t.Fatalf("Query replay: %v", err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("replay returned %d items, want 0", len(page.Items))
	}
}

// Timestamp-less plugin lines fall back to "day start + position in the read
// window", so the same physical line changes its sort time between pages. The
// walk must still emit every physical line once: dedupe is by byte offset, and
// the seen set has to survive more than one page while that line is re-read.
func TestTimelineCursorWalksTimestampLessFileLinesOnce(t *testing.T) {
	tl, st, dir := newTestTimeline(t)
	day := fixedNow().Format(dateLayout)
	path := filepath.Join(dir, "logs", "plugins", "mock-"+day+".log")
	var lines []string
	for i := 0; i < 700; i++ {
		if i%3 == 0 {
			lines = append(lines, fmt.Sprintf("plain line %d", i))
			continue
		}
		at := timelineTS(8, 0).Add(time.Duration(i) * time.Second)
		lines = append(lines, fmt.Sprintf("%s timed line %d", at.Format("2006-01-02 15:04:05"), i))
	}
	writeFile(t, path, joinLines(lines))
	for i := 0; i < 200; i++ {
		at := timelineTS(7, 0).Add(time.Duration(i) * time.Second)
		if err := st.InsertAPIRequestLog(model.APIRequestLog{
			TS: at.Format(time.RFC3339), ReqID: fmt.Sprintf("req-%03d", i), Source: "plugin:mock",
			Status: 200, LatencyMS: 10,
		}); err != nil {
			t.Fatalf("insert api log %d: %v", i, err)
		}
	}

	fileKeys := map[string]bool{}
	reqIDs := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		page, err := tl.Query(dayRange(1), cursor, 50)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		for _, item := range page.Items {
			if item.filePath != "" {
				key := fileItemKey(item)
				if fileKeys[key] {
					t.Fatalf("page %d re-emitted %s", pages, key)
				}
				fileKeys[key] = true
			}
			if item.Type == TimelineAPI {
				if reqIDs[item.ReqID] {
					t.Fatalf("page %d re-emitted api row %s", pages, item.ReqID)
				}
				reqIDs[item.ReqID] = true
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		if pages > 100 {
			t.Fatalf("pagination did not terminate after %d pages", pages)
		}
	}
	if len(fileKeys) != len(lines) {
		t.Fatalf("walked %d file lines, want %d (pages=%d)", len(fileKeys), len(lines), pages)
	}
	if len(reqIDs) != 200 {
		t.Fatalf("walked %d api rows, want 200", len(reqIDs))
	}
}

func TestTimelineQueryReportsSourceCountsForThePage(t *testing.T) {
	tl, st, dir := newTestTimeline(t)
	seedTimelineSources(t, st, dir)

	page, err := tl.Query(dayRange(1), "", maxTimelinePageSize)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if page.Truncated {
		t.Fatal("Truncated = true, want false for 11 events")
	}
	if page.SourceCounts[TimelineAPI] != 1 {
		t.Fatalf("SourceCounts[api] = %d, want 1", page.SourceCounts[TimelineAPI])
	}
	if page.SourceCounts[TimelineAudit] != 1 {
		t.Fatalf("SourceCounts[audit] = %d, want 1", page.SourceCounts[TimelineAudit])
	}
	// 7 host lines + 2 plugin lines.
	if page.SourceCounts[TimelineRuntime] != 9 {
		t.Fatalf("SourceCounts[runtime] = %d, want 9", page.SourceCounts[TimelineRuntime])
	}
	if len(page.Items) != 11 || page.HasMore {
		t.Fatalf("page = %d items, has_more=%v", len(page.Items), page.HasMore)
	}
	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatalf("marshal page: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal page: %v", err)
	}
	if body["has_more"] != false || body["truncated"] != false {
		t.Fatalf("json flags = %v", body)
	}
	counts, ok := body["source_counts"].(map[string]any)
	if !ok || counts[TimelineRuntime] != float64(9) {
		t.Fatalf("json source_counts = %v", body["source_counts"])
	}
}

func TestTimelineQuerySourceCountsOnlyListsRequestedTypes(t *testing.T) {
	tl, st, dir := newTestTimeline(t)
	seedTimelineSources(t, st, dir)

	filter := dayRange(1)
	filter.Types = []string{TimelineAPI}
	page, err := tl.Query(filter, "", maxTimelinePageSize)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.SourceCounts) != 1 || page.SourceCounts[TimelineAPI] != 1 {
		t.Fatalf("SourceCounts = %+v, want only api:1", page.SourceCounts)
	}
	if page.Truncated {
		t.Fatal("Truncated = true, want false")
	}
}

func TestTimelineQueryRejectsMalformedCursor(t *testing.T) {
	tl, _, _ := newTestTimeline(t)
	if _, err := tl.Query(dayRange(1), "!!!not-base64!!!", 10); err == nil {
		t.Fatal("malformed cursor was accepted")
	}
	// A negative boundary is out of range, not a valid resume point.
	negative := EncodePageCursor(PageCursor{At: -1})
	if _, err := DecodePageCursor(negative); err == nil {
		t.Fatal("negative cursor accepted")
	}
}

func TestReadLinesBeforeWalksBackwards(t *testing.T) {
	dir := testTempDir(t)
	path := filepath.Join(dir, "walk.log")
	writeFile(t, path, "l1\nl2\nl3\nl4\nl5\n")

	lines, next, exhausted, truncated, err := readLinesBefore(path, 0, 2, maxTimelineFileBytes)
	if err != nil || exhausted || truncated {
		t.Fatalf("first read = %v/%v/%v/%v", lines, next, exhausted, truncated)
	}
	if len(lines) != 2 || lines[0].text != "l4" || lines[1].text != "l5" {
		t.Fatalf("first read lines = %+v, want l4,l5", lines)
	}
	lines, next, exhausted, _, err = readLinesBefore(path, next, 2, maxTimelineFileBytes)
	if err != nil || exhausted || len(lines) != 2 || lines[0].text != "l2" || lines[1].text != "l3" {
		t.Fatalf("second read = %+v/%v/%v", lines, next, exhausted)
	}
	lines, next, exhausted, _, err = readLinesBefore(path, next, 2, maxTimelineFileBytes)
	if err != nil || !exhausted || len(lines) != 1 || lines[0].text != "l1" || next != 0 {
		t.Fatalf("third read = %+v/%v/%v", lines, next, exhausted)
	}

	// A file without a trailing newline still yields its last line.
	noNL := filepath.Join(dir, "no-newline.log")
	writeFile(t, noNL, "a\nb")
	lines, next, exhausted, _, err = readLinesBefore(noNL, 0, 1, maxTimelineFileBytes)
	if err != nil || exhausted || len(lines) != 1 || lines[0].text != "b" {
		t.Fatalf("no-newline read = %+v/%v/%v/%v", lines, next, exhausted, err)
	}
	lines, _, exhausted, _, err = readLinesBefore(noNL, next, 1, maxTimelineFileBytes)
	if err != nil || !exhausted || len(lines) != 1 || lines[0].text != "a" {
		t.Fatalf("no-newline second read = %+v/%v/%v", lines, exhausted, err)
	}

	if lines, _, exhausted, _, err := readLinesBefore(filepath.Join(dir, "missing.log"), 0, 1, maxTimelineFileBytes); err != nil || !exhausted || lines != nil {
		t.Fatalf("missing file = %v/%v/%v", lines, exhausted, err)
	}
}
