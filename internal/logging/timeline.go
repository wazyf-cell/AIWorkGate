package logging

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"aiworkgate/internal/model"
	"aiworkgate/internal/store"
)

// Unified timeline event types and categories (§5.3, §8.4).
const (
	TimelineAPI     = "api"
	TimelineRuntime = "runtime"
	TimelineAudit   = "audit"
	// TimelineProxy is one tunnelled CONNECT through a local egress listener;
	// TimelineConnectivity is one user-triggered connectivity probe. Both are
	// structured database rows, like api and audit.
	TimelineProxy        = "proxy"
	TimelineConnectivity = "connectivity"

	TimelineCategoryRequest   = "request"
	TimelineCategoryCheckin   = "checkin"
	TimelineCategoryLogin     = "login"
	TimelineCategoryScheduler = "scheduler"
	TimelineCategoryPlugin    = "plugin"
	TimelineCategoryProxy     = "proxy"
	TimelineCategoryConfig    = "config"
	TimelineCategoryAudit     = "audit"
	// TimelineCategoryConnectivity labels probe history inside the merged
	// stream, so "why was this account unreachable" sits beside the rows it
	// explains instead of living in its own panel.
	TimelineCategoryConnectivity = "connectivity"
	// TimelineCategoryAction marks a user action: one state-changing admin API
	// call. It is kept apart from "audit" (which records state changes) so the
	// timeline can show "what did the user click" without burying it in the
	// change history.
	TimelineCategoryAction  = "action"
	TimelineCategoryError   = "error"
	TimelineCategoryRuntime = "runtime"
)

const (
	defaultTimelineDays     = 7
	maxTimelineDays         = 366
	defaultTimelinePageSize = 100
	maxTimelinePageSize     = 200
	// maxTimelineFileBytes bounds how many bytes one page reads from one log
	// file. Paging resumes at a byte offset, so this only limits pathological
	// single lines or a search that must walk very far; it is no longer a
	// "newest N lines" cap.
	maxTimelineFileBytes = 4 << 20
	maxStreamReadBytes   = 1 << 20
)

// TimelineItem is one merged event. The unexported fields only back sorting
// and keyword matching.
type TimelineItem struct {
	TS       string `json:"ts"`
	Type     string `json:"type"`
	Category string `json:"category,omitempty"`
	Level    string `json:"level,omitempty"`
	Source   string `json:"source,omitempty"`
	Summary  string `json:"summary,omitempty"`
	ReqID    string `json:"req_id,omitempty"`
	// RowID identifies the database row behind an audit/proxy/connectivity
	// event so GET /api/logs/raw can fetch that one line.
	RowID     int64  `json:"row_id,omitempty"`
	Account   string `json:"account,omitempty"`
	Model     string `json:"model,omitempty"`
	Status    int    `json:"status,omitempty"`
	LatencyMS int64  `json:"latency_ms,omitempty"`

	at     time.Time
	search string

	// The fields below are paging keys, never serialized. Database events carry
	// their row id in seq; log-file events carry path + offset. At an equal
	// timestamp the merged order falls back to these, so a cursor can resume
	// exactly between two events that share a second (API rows only have
	// second precision).
	seq        int64
	filePath   string
	fileOffset int64
	fileEnd    int64
}

// TimelinePage is the body of GET /api/logs/timeline.
type TimelinePage struct {
	Items    []TimelineItem `json:"items"`
	PageSize int            `json:"page_size"`
	// HasMore reports that at least one source has older events. The client
	// keeps scrolling while it is true; NextCursor is how it asks for them.
	HasMore bool `json:"has_more"`
	// NextCursor is the opaque resume token; empty when nothing is left.
	NextCursor string `json:"next_cursor,omitempty"`
	// Truncated reports that a log file could not be read further within its
	// per-page byte budget; older lines of that file exist but this response
	// could not reach them. It is independent of HasMore: the cursor still
	// advances, so scrolling continues past the truncated window.
	Truncated bool `json:"truncated"`
	// SourceCounts reports how many events each type contributed to this page
	// (api/audit/proxy/connectivity/runtime). The two file sources (host JSONL
	// and plugin logs) both map to "runtime" and are summed. Absent when the
	// page is empty.
	SourceCounts map[string]int `json:"source_counts,omitempty"`
}

// PageCursor resumes a merged timeline page. It is opaque to clients: the host
// encodes it as base64 JSON and treats its contents as internal.
type PageCursor struct {
	// At is Unix milliseconds of the page boundary. Database sources resume at
	// (At, id) and file sources at their byte offsets.
	At int64 `json:"at,omitempty"`
	// Per-source id of the last emitted row at At; 0 means "no row of this
	// source was emitted at At", i.e. the next page may include all of them.
	API   int64            `json:"api,omitempty"`
	Audit int64            `json:"audit,omitempty"`
	Proxy int64            `json:"proxy,omitempty"`
	Conn  int64            `json:"conn,omitempty"`
	Files map[string]int64 `json:"files,omitempty"`
	// Seen holds "path|offset" keys of file events at At that were already
	// emitted. A resumed read starts at a line boundary and can see newer
	// lines from the same file (log files are not strictly chronological when
	// a line has no parseable timestamp), so the boundary instant is deduped
	// by key rather than guessed from time alone.
	Seen []string `json:"seen,omitempty"`
}

// EncodePageCursor renders the opaque cursor sent to clients.
func EncodePageCursor(c PageCursor) string {
	data, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(data)
}

// ErrBadCursor marks a client-supplied timeline cursor that cannot be decoded,
// so the admin API can answer 400 instead of 500.
var ErrBadCursor = errors.New("logging: invalid timeline cursor")

// DecodePageCursor parses a cursor from the client. An empty value means the
// first page. Malformed or oversized cursors are rejected rather than guessed:
// a bogus cursor would silently skip history.
func DecodePageCursor(raw string) (PageCursor, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return PageCursor{}, nil
	}
	if len(raw) > 64<<10 {
		return PageCursor{}, fmt.Errorf("%w: cursor is too long", ErrBadCursor)
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return PageCursor{}, fmt.Errorf("%w: decode: %v", ErrBadCursor, err)
	}
	var c PageCursor
	if err := json.Unmarshal(data, &c); err != nil {
		return PageCursor{}, fmt.Errorf("%w: decode: %v", ErrBadCursor, err)
	}
	if c.At < 0 || len(c.Files) > 10000 {
		return PageCursor{}, fmt.Errorf("%w: cursor is out of range", ErrBadCursor)
	}
	return c, nil
}

// TimelineFilter selects merged events. Days are local YYYY-MM-DD, inclusive.
type TimelineFilter struct {
	FromDay string
	ToDay   string
	// FromAt/ToAt narrow the range to sub-day precision when the caller sends
	// RFC3339 timestamps (the UI's "last hour" preset). Zero means unbounded.
	FromAt     time.Time
	ToAt       time.Time
	Types      []string
	Categories []string
	Level      string
	Query      string
}

// Timeline merges api_logs, proxy_logs, connectivity probes, audit_log, host
// JSONL logs and plugin captured logs into one ordered event stream.
type Timeline struct {
	Store   *store.Store
	DataDir string
	Plugins func() []string
	Now     func() time.Time
}

// NewTimeline returns a reader over the host data directory.
func NewTimeline(st *store.Store, dataDir string, plugins func() []string) *Timeline {
	return &Timeline{Store: st, DataDir: dataDir, Plugins: plugins, Now: time.Now}
}

func (t *Timeline) now() time.Time {
	if t != nil && t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// parseTimeBound accepts either a local date (YYYY-MM-DD, whole day) or an
// RFC3339 timestamp. The returned day is used for the day-scoped sources; at is
// zero for a whole-day bound.
func parseTimeBound(raw string, loc *time.Location) (time.Time, time.Time, error) {
	if parsed, err := time.ParseInLocation(dateLayout, raw, loc); err == nil {
		return parsed, time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return parsed.In(loc), parsed.In(loc), nil
}

// ParseTimelineFilter interprets from/to, type, category, level and q.
// Repeatable and comma-separated values are accepted. Paging is parsed
// separately by ParseTimelinePage because the timeline uses cursors while the
// table endpoints still use page numbers.
func ParseTimelineFilter(values url.Values, now time.Time) (TimelineFilter, error) {
	loc := time.Local
	to := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	from := to.AddDate(0, 0, -(defaultTimelineDays - 1))
	var fromAt, toAt time.Time
	if raw := strings.TrimSpace(firstValue(values, "from")); raw != "" {
		parsed, at, err := parseTimeBound(raw, loc)
		if err != nil {
			return TimelineFilter{}, fmt.Errorf("from must be YYYY-MM-DD or RFC3339, got %q", raw)
		}
		from, fromAt = parsed, at
	}
	if raw := strings.TrimSpace(firstValue(values, "to")); raw != "" {
		parsed, at, err := parseTimeBound(raw, loc)
		if err != nil {
			return TimelineFilter{}, fmt.Errorf("to must be YYYY-MM-DD or RFC3339, got %q", raw)
		}
		to, toAt = parsed, at
	}
	if from.After(to) {
		from, to = to, from
	}
	if to.Sub(from) > time.Duration(maxTimelineDays-1)*24*time.Hour {
		from = to.AddDate(0, 0, -(maxTimelineDays - 1))
	}
	f := TimelineFilter{
		FromDay:    from.Format(dateLayout),
		ToDay:      to.Format(dateLayout),
		FromAt:     fromAt,
		ToAt:       toAt,
		Types:      multiValues(values, "type"),
		Categories: multiValues(values, "category"),
		Level:      strings.ToLower(strings.TrimSpace(firstValue(values, "level"))),
		Query:      strings.TrimSpace(firstValue(values, "q")),
	}
	return f, nil
}

// ParseTimelinePage reads the paging parameters. The merged timeline uses
// cursor; page is only for the table endpoints that share this query parser.
func ParseTimelinePage(values url.Values) (cursor string, page, pageSize int, err error) {
	page = 1
	if n, convErr := strconv.Atoi(strings.TrimSpace(firstValue(values, "page"))); convErr == nil && n > 0 {
		page = n
	}
	pageSize = defaultTimelinePageSize
	if n, convErr := strconv.Atoi(strings.TrimSpace(firstValue(values, "page_size"))); convErr == nil && n > 0 {
		pageSize = n
	}
	if pageSize > maxTimelinePageSize {
		pageSize = maxTimelinePageSize
	}
	return firstValue(values, "cursor"), page, pageSize, nil
}

func firstValue(values url.Values, key string) string {
	if values == nil {
		return ""
	}
	items := values[key]
	if len(items) == 0 {
		return ""
	}
	return strings.TrimSpace(items[0])
}

func multiValues(values url.Values, key string) []string {
	if values == nil {
		return nil
	}
	var out []string
	seen := make(map[string]bool)
	for _, raw := range values[key] {
		for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '|' }) {
			part = strings.TrimSpace(part)
			if part == "" || seen[part] {
				continue
			}
			seen[part] = true
			out = append(out, part)
		}
	}
	return out
}

// Query returns one page of the merged timeline, newest first. An empty
// cursor starts at the newest event; otherwise the cursor from the previous
// page decides where to resume.
//
// Every source is read with a keyset position (store: timestamp + row id;
// files: byte offset), so page N costs the same as page 1 and no fixed scan
// window can hide older events.
func (t *Timeline) Query(f TimelineFilter, rawCursor string, pageSize int) (TimelinePage, error) {
	out := TimelinePage{Items: []TimelineItem{}, PageSize: pageSize}
	if pageSize < 1 {
		pageSize = defaultTimelinePageSize
		out.PageSize = pageSize
	}
	if pageSize > maxTimelinePageSize {
		pageSize = maxTimelinePageSize
		out.PageSize = pageSize
	}
	cursor, err := DecodePageCursor(rawCursor)
	if err != nil {
		return out, err
	}
	if t == nil {
		return out, nil
	}
	// One extra row per source tells us whether older events exist without a
	// second round trip.
	want := pageSize + 1
	first := cursor.At == 0 && len(cursor.Files) == 0

	var items []TimelineItem
	more := false
	add := func(got []TimelineItem, hasMore bool) {
		items = append(items, got...)
		if hasMore {
			more = true
		}
	}
	if t.typeAllowed(TimelineAPI, f.Types) {
		got, hasMore := t.apiPage(f, cursor, want)
		add(got, hasMore)
	}
	if t.typeAllowed(TimelineAudit, f.Types) {
		got, hasMore := t.auditPage(f, cursor, want)
		add(got, hasMore)
	}
	if t.typeAllowed(TimelineProxy, f.Types) {
		got, hasMore := t.proxyPage(f, cursor, want)
		add(got, hasMore)
	}
	if t.typeAllowed(TimelineConnectivity, f.Types) {
		got, hasMore := t.connectivityPage(f, cursor, want)
		add(got, hasMore)
	}

	// File sources. Their paging state is a byte offset per file; a file that
	// is absent from a resumed cursor is exhausted.
	var files []runtimeFile
	if t.typeAllowed(TimelineRuntime, f.Types) {
		files = t.runtimeFiles(f)
	}
	seen := make(map[string]bool, len(cursor.Seen))
	for _, key := range cursor.Seen {
		seen[key] = true
	}
	fileStates := make([]runtimeFileState, 0, len(files))
	for _, spec := range files {
		end := int64(-1)
		if !first {
			offset, ok := cursor.Files[spec.path]
			if !ok || offset <= 0 {
				continue
			}
			end = offset
		}
		state := t.readRuntimeFile(spec, end, want)
		if state.err != nil {
			continue
		}
		// A resumed read can re-read lines from the previous page: the cut is a
		// byte offset, and log files are not strictly chronological when a line
		// has no parseable timestamp (it falls back to the file position). Dedupe
		// by identity — "path|offset" of every file item the previous page
		// emitted — rather than by time: a time boundary also drops events that
		// were read but never emitted because the page filled up first, which
		// silently loses them and can leave a page with nothing to advance on.
		if !first {
			kept := state.items[:0]
			for _, item := range state.items {
				if seen[fileItemKey(item)] {
					continue
				}
				kept = append(kept, item)
			}
			state.items = kept
		}
		fileStates = append(fileStates, state)
		items = append(items, state.items...)
		if state.more {
			more = true
		}
		if state.truncated {
			out.Truncated = true
		}
	}

	sortTimelineItems(items)

	// Walk the merged list once: the first pageSize matches are the page, and
	// the newest non-emitted match per file is where that file must resume.
	page := make([]TimelineItem, 0, pageSize)
	fileCut := make(map[string]int64, len(fileStates))
	matches := 0
	for _, item := range items {
		if !t.match(item, f) {
			continue
		}
		matches++
		if len(page) < pageSize {
			page = append(page, item)
			continue
		}
		if item.filePath != "" {
			// Resume just after the furthest non-emitted line of this file, so
			// every matching line that did not fit on this page is re-read.
			if end, ok := fileCut[item.filePath]; !ok || item.fileEnd > end {
				fileCut[item.filePath] = item.fileEnd
			}
		}
	}
	out.Items = page
	if matches > len(page) {
		more = true
	}

	// Boundary timestamp: the last emitted event, or the newest examined one
	// when a selective filter matched nothing (so the cursor still advances).
	var boundary time.Time
	if len(page) > 0 {
		boundary = page[len(page)-1].at
	} else if len(items) > 0 {
		boundary = items[0].at
	}
	next := PageCursor{At: boundary.UnixMilli()}
	if boundary.IsZero() {
		// Nothing was examined this round (only blank or unparseable file lines,
		// or the sources are already exhausted). Keep the database position and
		// let the per-file offsets carry the progress: a zero time would encode
		// as year 1 and be rejected as an invalid cursor.
		next.At = cursor.At
	}
	// Database sources resume with (At, id). When the page is non-empty the id
	// is the smallest id emitted at At, so rows at that instant that were cut
	// by the page (or filtered after it) come back; when the page is empty we
	// move past what was examined instead, otherwise a fully filtered page
	// could repeat forever.
	for _, item := range page {
		if item.at.UnixMilli() != next.At || item.filePath != "" {
			continue
		}
		switch item.Type {
		case TimelineAPI:
			next.API = minID(next.API, item.seq)
		case TimelineAudit:
			next.Audit = minID(next.Audit, item.seq)
		case TimelineProxy:
			next.Proxy = minID(next.Proxy, item.seq)
		case TimelineConnectivity:
			next.Conn = minID(next.Conn, item.seq)
		}
	}
	if len(page) == 0 {
		for _, item := range items {
			if item.at.UnixMilli() != next.At || item.filePath != "" {
				continue
			}
			switch item.Type {
			case TimelineAPI:
				next.API = minID(next.API, item.seq)
			case TimelineAudit:
				next.Audit = minID(next.Audit, item.seq)
			case TimelineProxy:
				next.Proxy = minID(next.Proxy, item.seq)
			case TimelineConnectivity:
				next.Conn = minID(next.Conn, item.seq)
			}
		}
	}

	next.Files = make(map[string]int64, len(fileStates))
	for _, state := range fileStates {
		end := state.next
		if cut, ok := fileCut[state.spec.path]; ok {
			end = cut
		}
		if end > 0 {
			next.Files[state.spec.path] = end
		}
	}
	if len(next.Files) == 0 {
		next.Files = nil
	}
	// File events already emitted need identity dedupe on the next read; rows
	// from the database are deduped by (time, id) in SQL. The set carries forward
	// across pages: a line can be re-read by several consecutive reads (log files
	// are not strictly chronological once timestamp-less lines fall back), so a
	// one-page memory would let it be emitted again two pages later. Entries the
	// new cut can no longer reach are pruned, which keeps the cursor small.
	seenKeys := make(map[string]bool, len(cursor.Seen)+len(page))
	for _, key := range cursor.Seen {
		path, offset := splitFileItemKey(key)
		cut, ok := next.Files[path]
		// A read ends before `cut`, so only lines below it can come back.
		if !ok || offset >= cut {
			continue
		}
		seenKeys[key] = true
	}
	for _, item := range page {
		if item.filePath != "" {
			seenKeys[fileItemKey(item)] = true
		}
	}
	if len(seenKeys) > 0 {
		next.Seen = make([]string, 0, len(seenKeys))
		for key := range seenKeys {
			next.Seen = append(next.Seen, key)
		}
		sort.Strings(next.Seen)
	}

	if matches > 0 {
		counts := make(map[string]int, 5)
		for _, item := range page {
			counts[item.Type]++
		}
		out.SourceCounts = counts
	}
	if more {
		out.HasMore = true
		out.NextCursor = EncodePageCursor(next)
	}
	return out, nil
}

// minID keeps the smallest non-zero id, which is the oldest row emitted at the
// boundary instant and therefore the exclusive upper bound for the next page.
// fileItemKey identifies one physical log line across pages.
func fileItemKey(item TimelineItem) string {
	return item.filePath + "|" + strconv.FormatInt(item.fileOffset, 10)
}

// splitFileItemKey is the inverse of fileItemKey. Plugin and host paths are
// plain paths, so the last separator is always the one before the offset.
func splitFileItemKey(key string) (string, int64) {
	idx := strings.LastIndexByte(key, '|')
	if idx < 0 {
		return key, -1
	}
	offset, err := strconv.ParseInt(key[idx+1:], 10, 64)
	if err != nil {
		return key[:idx], -1
	}
	return key[:idx], offset
}

func minID(current, candidate int64) int64 {
	if candidate <= 0 {
		return current
	}
	if current <= 0 || candidate < current {
		return candidate
	}
	return current
}

// sortTimelineItems applies the merged order. The final file/row-id keys make
// the order total, which is what lets a cursor resume between two events with
// the same timestamp.
func sortTimelineItems(items []TimelineItem) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if !a.at.Equal(b.at) {
			return a.at.After(b.at)
		}
		if a.Type != b.Type {
			return a.Type < b.Type
		}
		if a.ReqID != b.ReqID {
			return a.ReqID < b.ReqID
		}
		if a.Summary != b.Summary {
			return a.Summary < b.Summary
		}
		if a.filePath != b.filePath {
			return a.filePath < b.filePath
		}
		if a.fileOffset != b.fileOffset {
			return a.fileOffset > b.fileOffset
		}
		return a.seq > b.seq
	})
}

func (t *Timeline) typeAllowed(kind string, types []string) bool {
	if len(types) == 0 {
		return true
	}
	for _, raw := range types {
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "all", "":
			return true
		case kind:
			return true
		}
	}
	return false
}

func (t *Timeline) match(item TimelineItem, f TimelineFilter) bool {
	if !f.FromAt.IsZero() && item.at.Before(f.FromAt) {
		return false
	}
	if !f.ToAt.IsZero() && !item.at.Before(f.ToAt) {
		return false
	}
	if !t.typeAllowed(item.Type, f.Types) {
		return false
	}
	if len(f.Categories) > 0 {
		ok := false
		for _, c := range f.Categories {
			if strings.EqualFold(strings.TrimSpace(c), item.Category) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if f.Level != "" && normalizeLevel(item.Level) != normalizeLevel(f.Level) {
		return false
	}
	if q := strings.ToLower(strings.TrimSpace(f.Query)); q != "" {
		hay := item.search
		if hay == "" {
			hay = strings.ToLower(item.Summary)
		}
		if !strings.Contains(hay, q) {
			return false
		}
	}
	return true
}

// ------------------------------------------------------------------ api

// apiPage reads one keyset page of request-log rows.
func (t *Timeline) apiPage(f TimelineFilter, cursor PageCursor, limit int) ([]TimelineItem, bool) {
	if t.Store == nil {
		return nil, false
	}
	from, to := dayBounds(f.FromDay, f.ToDay)
	rows, err := t.Store.QueryAPILogsTimelinePage(
		store.Filter{From: from, To: to, NoBodies: true}, cursor.At, cursor.API, limit)
	if err != nil {
		return nil, false
	}
	out := make([]TimelineItem, 0, len(rows))
	for _, row := range rows {
		item := apiItem(row.Log)
		item.seq = row.ID
		out = append(out, item)
	}
	return out, len(rows) == limit
}

// ------------------------------------------------------------------ audit

// auditPage reads one keyset page of audit rows.
func (t *Timeline) auditPage(f TimelineFilter, cursor PageCursor, limit int) ([]TimelineItem, bool) {
	if t.Store == nil {
		return nil, false
	}
	entries, err := t.Store.ListAuditTimelinePage(f.FromDay, f.ToDay, cursor.At, cursor.Audit, limit)
	if err != nil {
		return nil, false
	}
	out := make([]TimelineItem, 0, len(entries))
	for _, entry := range entries {
		out = append(out, auditItem(entry))
	}
	return out, len(entries) == limit
}

func auditFailed(result string) bool {
	r := strings.ToLower(strings.TrimSpace(result))
	return strings.Contains(r, "error") || strings.Contains(r, "fail")
}

// ------------------------------------------------------------------ proxy

// proxyPage reads one keyset page of tunnelled CONNECT rows.
func (t *Timeline) proxyPage(f TimelineFilter, cursor PageCursor, limit int) ([]TimelineItem, bool) {
	if t.Store == nil {
		return nil, false
	}
	rows, err := t.Store.QueryProxyLogsTimelinePage(f.FromDay, f.ToDay, cursor.At, cursor.Proxy, limit)
	if err != nil {
		return nil, false
	}
	out := make([]TimelineItem, 0, len(rows))
	for _, row := range rows {
		out = append(out, ProxyItem(row))
	}
	return out, len(rows) == limit
}

// ProxyItem renders one proxy tunnel row. It is exported because the proxy
// tab shows the same text the timeline shows.
func ProxyItem(row model.ProxyLog) TimelineItem {
	level := LevelInfo
	if row.Result != model.ProxyResultOK {
		level = LevelError
	}
	summary := fmt.Sprintf("%s:%d → %s", row.TargetHost, row.TargetPort, row.Result)
	if row.BytesUp > 0 || row.BytesDown > 0 {
		summary += fmt.Sprintf(" ↑%d ↓%d B", row.BytesUp, row.BytesDown)
	}
	if row.Error != "" {
		summary += ": " + row.Error
	}
	source := "proxy:" + row.ProxyID
	if row.ProxyLabel != "" {
		source = "proxy:" + row.ProxyLabel
	}
	item := TimelineItem{
		TS:        row.TS,
		Type:      TimelineProxy,
		Category:  TimelineCategoryProxy,
		Level:     level,
		Source:    source,
		Summary:   summary,
		Account:   proxyAccount(row),
		Status:    row.Status,
		LatencyMS: row.DurationMS,
		RowID:     row.ID,
		seq:       row.ID,
	}
	// `at` is the column the store pages on; using it here keeps the cursor
	// boundary and the SQL predicate on the same clock.
	if row.At > 0 {
		item.at = time.Unix(row.At, 0)
	} else {
		item.at = parseTS(row.TS)
	}
	item.search = strings.ToLower(strings.Join([]string{
		summary, row.PluginID, row.AccountID, row.ProxyID, row.ProxyLabel,
		row.TargetHost, row.Error, strconv.Itoa(row.TargetPort),
	}, " "))
	return item
}

func proxyAccount(row model.ProxyLog) string {
	if row.PluginID == "" {
		return row.AccountID
	}
	if row.AccountID == "" {
		return row.PluginID
	}
	return row.PluginID + "/" + row.AccountID
}

// ------------------------------------------------------------------ connectivity

// connectivityPage reads one keyset page of connectivity probes.
func (t *Timeline) connectivityPage(f TimelineFilter, cursor PageCursor, limit int) ([]TimelineItem, bool) {
	if t.Store == nil {
		return nil, false
	}
	rows, err := t.Store.ListConnectivityProbesTimelinePage(f.FromDay, f.ToDay, cursor.At, cursor.Conn, limit)
	if err != nil {
		return nil, false
	}
	out := make([]TimelineItem, 0, len(rows))
	for _, row := range rows {
		out = append(out, connectivityItem(row))
	}
	return out, len(rows) == limit
}

func connectivityItem(row store.ConnectivityProbe) TimelineItem {
	level := LevelInfo
	if !row.OK {
		level = LevelError
	}
	summary := row.Kind + " " + row.Target
	if row.OK {
		summary += fmt.Sprintf(" → ok %dms", row.LatencyMS)
	} else {
		summary += fmt.Sprintf(" → %s", row.ErrorCode)
		if row.Detail != "" {
			summary += ": " + row.Detail
		}
	}
	item := TimelineItem{
		TS:        time.Unix(row.CreatedAt, 0).Format(time.RFC3339),
		Type:      TimelineConnectivity,
		Category:  TimelineCategoryConnectivity,
		Level:     level,
		Source:    "diagnostics",
		Summary:   summary,
		Model:     row.TargetID,
		Status:    row.Status,
		LatencyMS: row.LatencyMS,
		RowID:     row.ID,
		seq:       row.ID,
	}
	item.at = time.Unix(row.CreatedAt, 0)
	item.search = strings.ToLower(strings.Join([]string{
		summary, row.Kind, row.Target, row.TargetID, row.ErrorCode, row.Detail,
	}, " "))
	return item
}

// ------------------------------------------------------------------ host JSONL

func hostItem(line, day string, index int) (TimelineItem, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return TimelineItem{}, false
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(line), &fields); err != nil {
		return TimelineItem{}, false
	}
	ts := stringField(fields, "ts")
	at := parseTS(ts)
	if at.IsZero() {
		at = fallbackTime(day, index)
		ts = at.Format(time.RFC3339)
	}
	level := normalizeLevel(stringField(fields, "level"))
	module := stringField(fields, "module")
	msg := stringField(fields, "msg")
	if msg == "" {
		msg = line
	}
	category := runtimeCategory(module, msg, level)
	item := TimelineItem{
		TS:       ts,
		Type:     TimelineRuntime,
		Category: category,
		Level:    level,
		Source:   module,
		Summary:  msg,
	}
	item.at = at
	item.search = strings.ToLower(line)
	return item, true
}

// runtimeCategory maps a host module/msg pair onto the §5.3 taxonomy.
func runtimeCategory(module, msg, level string) string {
	m := strings.ToLower(strings.TrimSpace(module))
	switch {
	case m == "schedule" || strings.HasPrefix(m, "schedule"):
		if strings.Contains(strings.ToLower(msg), "check-in") {
			return TimelineCategoryCheckin
		}
		return TimelineCategoryScheduler
	case strings.HasPrefix(m, "plugin"):
		return TimelineCategoryPlugin
	case m == "proxy" || m == "egress" ||
		strings.HasPrefix(m, "proxy") || strings.HasPrefix(m, "egress"):
		return TimelineCategoryProxy
	case m == "config" || m == "app" || strings.HasPrefix(m, "config"):
		return TimelineCategoryConfig
	case strings.HasPrefix(m, "notify"):
		return TimelineCategoryCheckin
	case strings.HasPrefix(m, "login"):
		return TimelineCategoryLogin
	default:
		if normalizeLevel(level) == LevelError {
			return TimelineCategoryError
		}
		return TimelineCategoryRuntime
	}
}

// ------------------------------------------------------------------ plugin logs

func pluginItem(pluginID, line, day string, index int) (TimelineItem, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return TimelineItem{}, false
	}
	at, ok := parseLineTime(line, day)
	if !ok {
		at = fallbackTime(day, index)
	}
	level := LevelInfo
	lower := strings.ToLower(line)
	switch {
	case strings.Contains(lower, "error") || strings.Contains(lower, "fatal") || strings.Contains(lower, "panic"):
		level = LevelError
	case strings.Contains(lower, "warn"):
		level = LevelWarn
	}
	item := TimelineItem{
		TS:       at.Format(time.RFC3339),
		Type:     TimelineRuntime,
		Category: TimelineCategoryPlugin,
		Level:    level,
		Source:   "plugin:" + pluginID,
		Summary:  line,
	}
	item.at = at
	item.search = lower
	return item, true
}

func (t *Timeline) pluginIDs() []string {
	seen := map[string]bool{}
	var out []string
	for _, raw := range t.Plugins() {
		raw = strings.TrimSpace(raw)
		if raw == "" || seen[raw] {
			continue
		}
		seen[raw] = true
		out = append(out, raw)
	}
	sort.Strings(out)
	return out
}

// ------------------------------------------------------------------ helpers

func parseTS(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
		if parsed, err := time.ParseInLocation(layout, raw, time.Local); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

// parseLineTime recognizes the timestamp shapes plugins commonly print. A
// time-only prefix is anchored to the file's day.
func parseLineTime(line, day string) (time.Time, bool) {
	head := line
	if len(head) > 64 {
		head = head[:64]
	}
	for _, layout := range []string{
		time.RFC3339Nano, time.RFC3339,
		"2006-01-02 15:04:05.000", "2006-01-02 15:04:05",
		"2006-01-02T15:04:05", "2006-01-02 15:04:05,000",
	} {
		if len(head) < len(layout) {
			continue
		}
		if parsed, err := time.ParseInLocation(layout, head[:len(layout)], time.Local); err == nil {
			return parsed, true
		}
	}
	base, err := time.ParseInLocation(dateLayout, day, time.Local)
	if err != nil {
		base = time.Now()
	}
	for _, layout := range []string{"15:04:05.000", "15:04:05"} {
		if len(head) < len(layout) {
			continue
		}
		if parsed, err := time.ParseInLocation(layout, head[:len(layout)], time.Local); err == nil {
			return time.Date(base.Year(), base.Month(), base.Day(),
				parsed.Hour(), parsed.Minute(), parsed.Second(), parsed.Nanosecond(), time.Local), true
		}
	}
	return time.Time{}, false
}

func fallbackTime(day string, index int) time.Time {
	base, err := time.ParseInLocation(dateLayout, day, time.Local)
	if err != nil {
		base = time.Now()
	}
	return base.Add(time.Duration(index) * time.Millisecond)
}

func dayBounds(fromDay, toDay string) (int64, int64) {
	var from, to int64
	if parsed, err := time.ParseInLocation(dateLayout, fromDay, time.Local); err == nil {
		from = parsed.Unix()
	}
	if parsed, err := time.ParseInLocation(dateLayout, toDay, time.Local); err == nil {
		to = parsed.AddDate(0, 0, 1).Add(-time.Second).Unix()
	}
	return from, to
}

func daysBetween(fromDay, toDay string) []string {
	from, err := time.ParseInLocation(dateLayout, fromDay, time.Local)
	if err != nil {
		return nil
	}
	to, err := time.ParseInLocation(dateLayout, toDay, time.Local)
	if err != nil {
		return nil
	}
	var out []string
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		out = append(out, d.Format(dateLayout))
		if len(out) > maxTimelineDays {
			break
		}
	}
	return out
}

func normalizeLevel(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "warn", "warning":
		return LevelWarn
	case "error", "fatal", "panic":
		return LevelError
	default:
		return LevelInfo
	}
}

func stringField(fields map[string]any, key string) string {
	v, ok := fields[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// ------------------------------------------------------------------ stream cursor

// TimelineCursor is the opaque SSE resume token: last row ids plus per-file
// byte offsets.
type TimelineCursor struct {
	API          int64            `json:"api,omitempty"`
	Audit        int64            `json:"audit,omitempty"`
	Proxy        int64            `json:"proxy,omitempty"`
	Connectivity int64            `json:"connectivity,omitempty"`
	Host         map[string]int64 `json:"host,omitempty"`
	Plugins      map[string]int64 `json:"plugins,omitempty"`
}

// Encode renders the cursor for the SSE id field.
func (c TimelineCursor) Encode() string {
	data, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return string(data)
}

// DecodeTimelineCursor parses an SSE id; an empty value yields a zero cursor.
func DecodeTimelineCursor(raw string) (TimelineCursor, error) {
	var c TimelineCursor
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return c, nil
	}
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return TimelineCursor{}, fmt.Errorf("logging: decode cursor: %w", err)
	}
	return c, nil
}

// Seed returns a cursor positioned at "now": stream clients only see events
// written after the connection opened.
func (t *Timeline) Seed(f TimelineFilter) (TimelineCursor, error) {
	cur := TimelineCursor{Host: map[string]int64{}, Plugins: map[string]int64{}}
	if t == nil {
		return cur, nil
	}
	if t.Store != nil {
		if t.typeAllowed(TimelineAPI, f.Types) {
			id, err := t.Store.MaxAPILogID()
			if err != nil {
				return cur, err
			}
			cur.API = id
		}
		if t.typeAllowed(TimelineAudit, f.Types) {
			id, err := t.Store.MaxAuditID()
			if err != nil {
				return cur, err
			}
			cur.Audit = id
		}
		if t.typeAllowed(TimelineProxy, f.Types) {
			id, err := t.Store.MaxProxyLogID()
			if err != nil {
				return cur, err
			}
			cur.Proxy = id
		}
		if t.typeAllowed(TimelineConnectivity, f.Types) {
			id, err := t.Store.MaxConnectivityProbeID()
			if err != nil {
				return cur, err
			}
			cur.Connectivity = id
		}
	}
	if !t.typeAllowed(TimelineRuntime, f.Types) || t.DataDir == "" {
		return cur, nil
	}
	day := t.now().Format(dateLayout)
	name := "host-" + day + ".jsonl"
	cur.Host[name] = fileSize(filepath.Join(t.DataDir, logsDirName, name))
	if t.Plugins != nil {
		for _, id := range t.pluginIDs() {
			safe, err := SanitizePluginID(id)
			if err != nil {
				continue
			}
			name := safe + "-" + day + ".log"
			cur.Plugins[name] = fileSize(filepath.Join(t.DataDir, logsDirName, pluginsDirName, name))
		}
	}
	return cur, nil
}

// Poll returns events appended since cur (oldest first) and the advanced
// cursor. It only tails today's files, which is what a live view needs.
func (t *Timeline) Poll(f TimelineFilter, cur TimelineCursor, limit int) ([]TimelineItem, TimelineCursor, error) {
	if limit <= 0 {
		limit = defaultTimelinePageSize
	}
	if limit > maxTimelinePageSize {
		limit = maxTimelinePageSize
	}
	if cur.Host == nil {
		cur.Host = map[string]int64{}
	}
	if cur.Plugins == nil {
		cur.Plugins = map[string]int64{}
	}
	if t == nil {
		return nil, cur, nil
	}
	var items []TimelineItem

	if t.Store != nil && t.typeAllowed(TimelineAPI, f.Types) {
		rows, lastID, err := t.Store.QueryAPILogsSince(cur.API, f.FromDay, f.ToDay, limit)
		if err != nil {
			return nil, cur, err
		}
		cur.API = lastID
		for _, row := range rows {
			items = append(items, apiItem(row))
		}
	}
	if t.Store != nil && t.typeAllowed(TimelineAudit, f.Types) {
		entries, err := t.Store.ListAuditSince(cur.Audit, f.FromDay, f.ToDay, limit)
		if err != nil {
			return nil, cur, err
		}
		for _, entry := range entries {
			if entry.ID > cur.Audit {
				cur.Audit = entry.ID
			}
			items = append(items, auditItem(entry))
		}
	}
	if t.Store != nil && t.typeAllowed(TimelineProxy, f.Types) {
		rows, lastID, err := t.Store.QueryProxyLogsSince(cur.Proxy, f.FromDay, f.ToDay, limit)
		if err != nil {
			return nil, cur, err
		}
		cur.Proxy = lastID
		for _, row := range rows {
			items = append(items, ProxyItem(row))
		}
	}
	if t.Store != nil && t.typeAllowed(TimelineConnectivity, f.Types) {
		rows, lastID, err := t.Store.ListConnectivityProbesSince(cur.Connectivity, f.FromDay, f.ToDay, limit)
		if err != nil {
			return nil, cur, err
		}
		cur.Connectivity = lastID
		for _, row := range rows {
			items = append(items, connectivityItem(row))
		}
	}
	if t.typeAllowed(TimelineRuntime, f.Types) && t.DataDir != "" {
		day := t.now().Format(dateLayout)
		hostName := "host-" + day + ".jsonl"
		hostPath := filepath.Join(t.DataDir, logsDirName, hostName)
		lines, next, err := readFileFrom(hostPath, cur.Host[hostName])
		if err != nil {
			return nil, cur, err
		}
		cur.Host[hostName] = next
		for i, line := range lines {
			if item, ok := hostItem(line, day, i); ok {
				items = append(items, item)
			}
		}
		if t.Plugins != nil {
			for _, id := range t.pluginIDs() {
				safe, err := SanitizePluginID(id)
				if err != nil {
					continue
				}
				name := safe + "-" + day + ".log"
				path := filepath.Join(t.DataDir, logsDirName, pluginsDirName, name)
				lines, next, err := readFileFrom(path, cur.Plugins[name])
				if err != nil {
					return nil, cur, err
				}
				cur.Plugins[name] = next
				for i, line := range lines {
					if item, ok := pluginItem(id, line, day, i); ok {
						items = append(items, item)
					}
				}
			}
		}
	}

	filtered := items[:0]
	for _, item := range items {
		if t.match(item, f) {
			filtered = append(filtered, item)
		}
	}
	items = filtered
	sort.SliceStable(items, func(i, j int) bool {
		if !items[i].at.Equal(items[j].at) {
			return items[i].at.Before(items[j].at)
		}
		return items[i].Type < items[j].Type
	})
	if len(items) > limit {
		items = items[:limit]
	}
	return items, cur, nil
}

// apiItem/auditItem are shared with Query so both paths render identically.
func apiItem(row model.APIRequestLog) TimelineItem {
	level := LevelInfo
	if row.Status >= 400 {
		level = LevelError
	}
	modelName := row.Model
	if modelName == "" {
		modelName = "?"
	}
	summary := fmt.Sprintf("%s → %d", modelName, row.Status)
	item := TimelineItem{
		TS:        row.TS,
		Type:      TimelineAPI,
		Category:  TimelineCategoryRequest,
		Level:     level,
		Source:    row.Source,
		Summary:   summary,
		ReqID:     row.ReqID,
		Account:   row.Account,
		Model:     row.Model,
		Status:    row.Status,
		LatencyMS: row.LatencyMS,
	}
	item.at = parseTS(row.TS)
	item.search = strings.ToLower(strings.Join([]string{
		summary, row.Source, row.Account, row.Model, row.LogicalModel,
		row.ReqID, row.Error, strconv.Itoa(row.Status),
	}, " "))
	return item
}

func auditItem(entry model.AuditEntry) TimelineItem {
	summary := strings.TrimSpace(entry.Action + " " + entry.Target)
	if entry.Detail != "" {
		summary += ": " + entry.Detail
	}
	if entry.Result != "" {
		summary += " [" + entry.Result + "]"
	}
	level := LevelInfo
	if auditFailed(entry.Result) {
		level = LevelError
	}
	// A recorded user action is stored in audit_log with category "action", but
	// it is not an audit event: keeping it in its own category lets the UI show
	// the click trail separately from state changes.
	category := TimelineCategoryAudit
	if entry.Category == "action" {
		category = TimelineCategoryAction
	}
	item := TimelineItem{
		TS:       entry.TS,
		Type:     TimelineAudit,
		Category: category,
		Level:    level,
		Source:   entry.Category,
		Summary:  summary,
		RowID:    entry.ID,
		seq:      entry.ID,
	}
	item.at = parseTS(entry.TS)
	item.search = strings.ToLower(strings.Join([]string{
		summary, entry.Category, entry.Action, entry.Target, entry.Detail, entry.Result,
	}, " "))
	return item
}

// readFileFrom returns complete lines appended after offset and the next
// offset. A shortened file (rotation) restarts at zero.
func readFileFrom(path string, offset int64) ([]string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, offset, nil
		}
		return nil, offset, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, offset, err
	}
	if st.Size() < offset {
		offset = 0
	}
	if st.Size() == offset {
		return nil, offset, nil
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxStreamReadBytes))
	if err != nil {
		return nil, offset, err
	}
	end := bytes.LastIndexByte(data, '\n')
	if end < 0 {
		return nil, offset, nil
	}
	chunk := data[:end]
	var lines []string
	for _, line := range strings.Split(string(chunk), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		lines = append(lines, line)
	}
	return lines, offset + int64(end+1), nil
}

func fileSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}
