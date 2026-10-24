package webui

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"aiworkgate/internal/model"
	"aiworkgate/internal/store"
)

// Errors a host adapter wraps to map log endpoints onto HTTP statuses.
var (
	// ErrDebugDisabled is returned by LogBody when logging.debug_body is off.
	ErrDebugDisabled = errors.New("debug body logging is disabled")
	// ErrLogNotFound is returned by LogBody when no row carries req_id.
	ErrLogNotFound = errors.New("log row not found")
)

// LogBody is the body of GET /api/logs/body.
type LogBody struct {
	Body      string `json:"body"`
	Truncated bool   `json:"truncated"`
	Debug     bool   `json:"debug"`
}

// LogEvent is one live event of the SSE stream; Data is the timeline JSON.
type LogEvent struct {
	ID   string
	Data json.RawMessage
}

// LogPollResult is one poll of the live tail plus the advanced cursor.
type LogPollResult struct {
	Events []LogEvent
	Cursor string
}

// LogRaw is the body of GET /api/logs/raw: the one source line behind a
// timeline event. For a file source this is the literal JSONL/text line; for a
// database source it is that row rendered as one JSON object.
type LogRaw struct {
	Line      string `json:"line"`
	File      string `json:"file"`
	Truncated bool   `json:"truncated,omitempty"`
}

// handleProxyLogs serves GET /api/logs/proxy: one page of the proxy traffic
// log, filtered by the same query vocabulary as the timeline.
func (s *server) handleProxyLogs(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	if s.opts.ProxyLogs == nil {
		s.unavailable(w)
		return
	}
	payload, err := s.opts.ProxyLogs(r.URL.Query())
	if err != nil {
		writeUsageError(w, "proxy logs", err)
		return
	}
	if payload == nil {
		payload = map[string]any{"items": []any{}, "total": 0, "page": 1, "page_size": 100}
	}
	writeJSON(w, http.StatusOK, payload)
}

// handleLogRaw serves GET /api/logs/raw. An unknown or missing row answers 404
// with type log_row_not_found so the UI can say "该事件没有原始行" instead of
// looking like a page failure.
func (s *server) handleLogRaw(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	if s.opts.LogRaw == nil {
		s.unavailable(w)
		return
	}
	query := r.URL.Query()
	if strings.TrimSpace(query.Get("file")) == "" {
		writeError(w, http.StatusBadRequest, "file is required", "bad_request")
		return
	}
	raw, err := s.opts.LogRaw(query)
	switch {
	case errors.Is(err, ErrLogNotFound):
		writeError(w, http.StatusNotFound, "no raw line for this event", "log_row_not_found")
		return
	case errors.Is(err, ErrInvalidRequest):
		writeError(w, http.StatusBadRequest, err.Error(), "bad_request")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "log raw: "+err.Error(), "internal_error")
		return
	}
	writeJSON(w, http.StatusOK, raw)
}

// ---------------------------------------------------------------- usage

func (s *server) handleUsageSummary(w http.ResponseWriter, r *http.Request) {
	s.serveUsage(w, r, "usage summary", s.opts.UsageSummary)
}

func (s *server) handleUsageCredits(w http.ResponseWriter, r *http.Request) {
	s.serveUsage(w, r, "usage credits", s.opts.UsageCredits)
}

func (s *server) handleUsageExpiry(w http.ResponseWriter, r *http.Request) {
	s.serveUsage(w, r, "usage expiry", s.opts.UsageExpiry)
}

func (s *server) handleUsageSessions(w http.ResponseWriter, r *http.Request) {
	s.serveUsage(w, r, "usage sessions", s.opts.UsageSessions)
}

func (s *server) serveUsage(w http.ResponseWriter, r *http.Request, action string, fn func(map[string][]string) (any, error)) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	if fn == nil {
		s.unavailable(w)
		return
	}
	payload, err := fn(r.URL.Query())
	if err != nil {
		writeUsageError(w, action, err)
		return
	}
	if payload == nil {
		payload = map[string]any{}
	}
	writeJSON(w, http.StatusOK, payload)
}

// writeUsageError maps a host-side query error onto 400/500.
func writeUsageError(w http.ResponseWriter, action string, err error) {
	if errors.Is(err, ErrInvalidRequest) {
		writeError(w, http.StatusBadRequest, action+": "+err.Error(), "invalid_query")
		return
	}
	writeError(w, http.StatusInternalServerError, action+": "+err.Error(), "internal_error")
}

// handleUsageExport streams a CSV attachment. The host builds the document
// into a buffer first so a late failure can still be reported as JSON.
func (s *server) handleUsageExport(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	if s.opts.UsageExport == nil {
		s.unavailable(w)
		return
	}
	var buf bytes.Buffer
	filename, err := s.opts.UsageExport(&buf, r.URL.Query())
	if err != nil {
		writeUsageError(w, "usage export", err)
		return
	}
	filename = safeFilename(filename, "usage.csv")
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

func safeFilename(name, fallback string) string {
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, "\r", "")
	name = strings.ReplaceAll(name, "\n", "")
	if idx := strings.LastIndexAny(name, `/\`); idx >= 0 {
		name = name[idx+1:]
	}
	name = strings.ReplaceAll(name, `"`, "")
	if name == "" || name == "." || name == ".." {
		return fallback
	}
	return name
}

// ---------------------------------------------------------------- request logs

// RequestLogsResult is the JSON payload of GET /api/logs/requests. Truncated
// is only set on the internal export pass.
type RequestLogsResult struct {
	Logs      []model.APIRequestLog `json:"logs"`
	Total     int                   `json:"total"`
	Truncated bool                  `json:"truncated,omitempty"`
	Error     string                `json:"error,omitempty"`
}

// RequestLogCSVHeader is the Chinese header row of the request-log export. It
// mirrors the on-screen table and never contains bodies or credentials.
var RequestLogCSVHeader = []string{
	"时间", "请求ID", "来源", "模型", "账号", "Key", "流式", "状态",
	"延迟ms", "首字ms", "输入Token", "输出Token", "缓存Token",
}

// RequestLogExportParam tells the host adapter that RequestLogs is serving a
// CSV export (larger cap, no bodies, newest 50k rows). It is never forwarded
// from a client list call.
const RequestLogExportParam = "export"

var errNoRequestLogRows = errors.New("宿主未返回请求日志数据")

// parseRequestLogQuery normalises the repeatable query into the flat map the
// RequestLogs callback expects and rejects invalid enum values. Unknown
// parameter names are ignored, as with the rest of the admin API.
func parseRequestLogQuery(values url.Values) (map[string]string, error) {
	query := make(map[string]string, len(values))
	for key, list := range values {
		if len(list) > 0 {
			query[key] = list[0]
		}
	}
	if raw, ok := query["status_class"]; ok {
		value := strings.ToLower(strings.TrimSpace(raw))
		if !store.ValidStatusClass(value) {
			return nil, fmt.Errorf("%w: status_class 只能是 2xx、4xx、5xx 或 error", ErrInvalidRequest)
		}
		query["status_class"] = value
	}
	if raw, ok := query["stream"]; ok {
		value := strings.ToLower(strings.TrimSpace(raw))
		if value != "" && value != "true" && value != "false" {
			return nil, fmt.Errorf("%w: stream 只能是 true 或 false", ErrInvalidRequest)
		}
		query["stream"] = value
	}
	return query, nil
}

// handleRequestLogsFiltered serves GET /api/logs/requests with the extended
// filter vocabulary. It sits in front of the legacy handler so invalid
// status_class/stream values answer 400 invalid_query.
func (s *server) handleRequestLogsFiltered(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	if s.opts.RequestLogs == nil {
		s.unavailable(w)
		return
	}
	query, err := parseRequestLogQuery(r.URL.Query())
	if err != nil {
		writeUsageError(w, "request logs", err)
		return
	}
	// The export marker is internal; clients cannot turn a list call into an
	// unbounded export.
	delete(query, RequestLogExportParam)
	writeJSON(w, http.StatusOK, s.opts.RequestLogs(query))
}

// handleRequestLogsExport streams the filtered request logs as an Excel
// friendly CSV attachment.
func (s *server) handleRequestLogsExport(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	if s.opts.RequestLogs == nil {
		s.unavailable(w)
		return
	}
	query, err := parseRequestLogQuery(r.URL.Query())
	if err != nil {
		writeUsageError(w, "request logs export", err)
		return
	}
	query[RequestLogExportParam] = "1"

	rows, truncated, err := extractRequestLogRows(s.opts.RequestLogs(query))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "请求日志导出失败："+err.Error(), "internal_error")
		return
	}

	// Build the document first so a late failure can still answer JSON.
	var buf bytes.Buffer
	if err := writeRequestLogsCSV(&buf, rows); err != nil {
		writeError(w, http.StatusInternalServerError, "请求日志导出失败："+err.Error(), "internal_error")
		return
	}

	filename := safeFilename("requests-"+time.Now().Format("20060102")+".csv", "requests.csv")
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if truncated {
		w.Header().Set("X-Export-Truncated", "true")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

// extractRequestLogRows accepts the payload shapes the host adapter (and older
// hosts) return for GET /api/logs/requests.
func extractRequestLogRows(payload any) ([]model.APIRequestLog, bool, error) {
	switch value := payload.(type) {
	case RequestLogsResult:
		return value.Logs, value.Truncated, nil
	case *RequestLogsResult:
		if value == nil {
			return nil, false, errNoRequestLogRows
		}
		return value.Logs, value.Truncated, nil
	case []model.APIRequestLog:
		return value, false, nil
	case map[string]any:
		rows, _ := value["logs"].([]model.APIRequestLog)
		if rows == nil {
			rows, _ = value["items"].([]model.APIRequestLog)
		}
		if rows == nil {
			return nil, false, errNoRequestLogRows
		}
		truncated, _ := value["truncated"].(bool)
		return rows, truncated, nil
	}
	return nil, false, errNoRequestLogRows
}

// writeRequestLogsCSV writes a UTF-8 BOM followed by the header and rows.
// encoding/csv quotes commas, quotes and newlines.
func writeRequestLogsCSV(w io.Writer, rows []model.APIRequestLog) error {
	if _, err := io.WriteString(w, "\ufeff"); err != nil {
		return err
	}
	writer := csv.NewWriter(w)
	if err := writer.Write(RequestLogCSVHeader); err != nil {
		return err
	}
	for _, rec := range rows {
		if err := writer.Write(requestLogCSVRow(rec)); err != nil {
			return err
		}
	}
	writer.Flush()
	return writer.Error()
}

func requestLogCSVRow(rec model.APIRequestLog) []string {
	key := rec.KeyName
	if key == "" {
		key = rec.KeyID
	}
	stream := "否"
	if rec.Stream {
		stream = "是"
	}
	return []string{
		safeCSVCell(exportTimestamp(rec.TS)),
		safeCSVCell(rec.ReqID),
		safeCSVCell(rec.Source),
		safeCSVCell(rec.Model),
		safeCSVCell(rec.Account),
		safeCSVCell(key),
		stream,
		strconv.Itoa(rec.Status),
		strconv.FormatInt(rec.LatencyMS, 10),
		strconv.FormatInt(rec.TTFBMS, 10),
		strconv.FormatInt(rec.PromptTokens, 10),
		strconv.FormatInt(rec.CompletionTokens, 10),
		strconv.FormatInt(rec.CachedTokens, 10),
	}
}

// safeCSVCell neutralises spreadsheet formula injection: a cell whose first
// byte is one of = + - @ TAB CR is prefixed with a single quote so Excel and
// friends treat it as text. Normal values are returned unchanged.
func safeCSVCell(value string) string {
	if value == "" {
		return value
	}
	switch value[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + value
	}
	return value
}

// exportTimestamp renders the stored RFC3339 timestamp as local wall-clock
// time; Excel treats the unparsable original as text.
func exportTimestamp(raw string) string {
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return raw
	}
	return parsed.Local().Format("2006-01-02 15:04:05")
}

// ---------------------------------------------------------------- timeline

func (s *server) handleLogTimeline(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	if s.opts.LogTimeline == nil {
		s.unavailable(w)
		return
	}
	payload, err := s.opts.LogTimeline(r.URL.Query())
	if err != nil {
		writeUsageError(w, "log timeline", err)
		return
	}
	if payload == nil {
		payload = map[string]any{"items": []any{}, "page_size": 100, "has_more": false}
	}
	writeJSON(w, http.StatusOK, payload)
}

// handleLogBody returns the stored, already redacted request or response body.
func (s *server) handleLogBody(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	if s.opts.LogBody == nil {
		s.unavailable(w)
		return
	}
	query := r.URL.Query()
	reqID := strings.TrimSpace(query.Get("req_id"))
	if reqID == "" {
		writeError(w, http.StatusBadRequest, "req_id is required", "bad_request")
		return
	}
	which := strings.ToLower(strings.TrimSpace(query.Get("which")))
	if which == "" {
		which = "req"
	}
	if which != "req" && which != "resp" {
		writeError(w, http.StatusBadRequest, `which must be "req" or "resp"`, "bad_request")
		return
	}
	body, err := s.opts.LogBody(reqID, which)
	switch {
	case errors.Is(err, ErrDebugDisabled):
		writeError(w, http.StatusForbidden, "debug body logging is disabled", "debug_disabled")
		return
	case errors.Is(err, ErrLogNotFound):
		writeError(w, http.StatusNotFound, "no log row for req_id "+reqID, "not_found")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "log body: "+err.Error(), "internal_error")
		return
	}
	body.Debug = true
	writeJSON(w, http.StatusOK, body)
}

// handleLogStream streams timeline events as SSE. Polling uses an opaque
// cursor; a comment heartbeat keeps proxies from closing the connection.
func (s *server) handleLogStream(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	if s.opts.LogSeed == nil || s.opts.LogPoll == nil {
		s.unavailable(w)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is not supported", "internal_error")
		return
	}
	query := r.URL.Query()
	cursor, err := s.opts.LogSeed(query)
	if err != nil {
		writeUsageError(w, "log stream", err)
		return
	}

	header := w.Header()
	header.Set("Content-Type", "text/event-stream; charset=utf-8")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	pollEvery := s.streamPoll
	if pollEvery <= 0 {
		pollEvery = time.Second
	}
	heartbeatEvery := s.streamHeartbeat
	if heartbeatEvery <= 0 {
		heartbeatEvery = 15 * time.Second
	}
	pollTicker := time.NewTicker(pollEvery)
	defer pollTicker.Stop()
	heartbeat := time.NewTicker(heartbeatEvery)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-pollTicker.C:
			result, err := s.opts.LogPoll(query, cursor, 100)
			if err != nil {
				continue
			}
			if result.Cursor != "" {
				cursor = result.Cursor
			}
			for _, event := range result.Events {
				data := string(event.Data)
				if data == "" {
					data = "{}"
				}
				if _, err := fmt.Fprintf(w, "id: %s\nevent: log\ndata: %s\n\n", event.ID, data); err != nil {
					return
				}
			}
			if len(result.Events) > 0 {
				flusher.Flush()
			}
		}
	}
}
