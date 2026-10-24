package app

// Proxy traffic log and raw source lines (Logs → 代理日志 / 原始行).
//
// proxyLogs serves the proxy tab with the same paging vocabulary the timeline
// uses. logRaw answers "what is the one line behind this event": a file source
// returns its literal line, a database source returns that row as one JSON
// object. Bodies are never included here; they stay behind /api/logs/body and
// its debug gate.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"aiworkgate/internal/logging"
	"aiworkgate/internal/model"
	"aiworkgate/internal/store"
	"aiworkgate/internal/webui"
)

const (
	// maxRawLine caps one raw line so a runaway plugin log line cannot be
	// streamed whole into the browser.
	maxRawLine = 64 << 10
	// rawScanLines bounds how many lines of one day file are searched for a
	// matching timestamp. Deeper history needs the day file itself, not a scan.
	rawScanLines = 20000
)

// proxyLogs backs GET /api/logs/proxy.
func (a *App) proxyLogs(query map[string][]string) (any, error) {
	if a.store == nil {
		return nil, errors.New("app: store is not available")
	}
	filter, err := a.timelineFilter(query)
	if err != nil {
		return nil, err
	}
	_, page, pageSize, err := logging.ParseTimelinePage(url.Values(query))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", webui.ErrInvalidRequest, err)
	}
	from, to := proxyQueryBounds(filter)

	result := strings.ToLower(strings.TrimSpace(queryFirst(query, "result")))
	switch result {
	case "", model.ProxyResultOK, model.ProxyResultDenied, model.ProxyResultFailed:
	default:
		return nil, fmt.Errorf("%w: result must be ok, denied or failed", webui.ErrInvalidRequest)
	}

	rows, total, err := a.store.QueryProxyLogs(store.ProxyLogFilter{
		From:      from,
		To:        to,
		Result:    result,
		PluginID:  queryFirst(query, "plugin"),
		AccountID: queryFirst(query, "account"),
		Query:     queryFirst(query, "q"),
		Limit:     pageSize,
		Offset:    (page - 1) * pageSize,
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"items":     rows,
		"total":     total,
		"page":      page,
		"page_size": pageSize,
	}, nil
}

// proxyQueryBounds turns a timeline filter into unix seconds. Sub-day bounds
// win when the caller sent RFC3339 timestamps (the UI's "last hour" preset).
func proxyQueryBounds(f logging.TimelineFilter) (int64, int64) {
	var from, to int64
	if !f.FromAt.IsZero() {
		from = f.FromAt.Unix()
	} else if f.FromDay != "" {
		if t, err := time.ParseInLocation("2006-01-02", f.FromDay, time.Local); err == nil {
			from = t.Unix()
		}
	}
	if !f.ToAt.IsZero() {
		to = f.ToAt.Unix()
	} else if f.ToDay != "" {
		if t, err := time.ParseInLocation("2006-01-02", f.ToDay, time.Local); err == nil {
			to = t.AddDate(0, 0, 1).Unix()
		}
	}
	return from, to
}

func queryFirst(query map[string][]string, key string) string {
	if query == nil {
		return ""
	}
	values := query[key]
	if len(values) == 0 {
		return ""
	}
	return strings.TrimSpace(values[0])
}

// logRaw backs GET /api/logs/raw. file selects the source, id names the row or
// the log file, ts locates the day, and summary (optional) disambiguates two
// events that share one second.
func (a *App) logRaw(query map[string][]string) (webui.LogRaw, error) {
	file := strings.ToLower(queryFirst(query, "file"))
	id := queryFirst(query, "id")
	ts := queryFirst(query, "ts")
	summary := queryFirst(query, "summary")

	switch file {
	case "api":
		if id == "" {
			return webui.LogRaw{}, fmt.Errorf("%w: id (req_id) is required", webui.ErrInvalidRequest)
		}
		return a.rawAPIRow(id)
	case "audit", "proxy", "connectivity":
		rowID, err := strconv.ParseInt(id, 10, 64)
		if err != nil || rowID <= 0 {
			return webui.LogRaw{}, fmt.Errorf("%w: id must be a row id", webui.ErrInvalidRequest)
		}
		return a.rawDBRow(file, rowID)
	case "host":
		return a.rawFileLine(filepath.Join("logs", "host-"+dayOf(ts)+".jsonl"), ts, id, summary)
	case "plugin":
		if id == "" {
			return webui.LogRaw{}, fmt.Errorf("%w: id (plugin id) is required", webui.ErrInvalidRequest)
		}
		safe, err := logging.SanitizePluginID(id)
		if err != nil {
			return webui.LogRaw{}, fmt.Errorf("%w: %v", webui.ErrInvalidRequest, err)
		}
		return a.rawFileLine(filepath.Join("logs", "plugins", safe+"-"+dayOf(ts)+".log"), ts, "", summary)
	default:
		return webui.LogRaw{}, fmt.Errorf("%w: unknown raw file %q", webui.ErrInvalidRequest, file)
	}
}

func (a *App) rawAPIRow(reqID string) (webui.LogRaw, error) {
	if a.store == nil {
		return webui.LogRaw{}, errors.New("app: store is not available")
	}
	row, found, err := a.store.APILogByReqID(reqID)
	if err != nil {
		return webui.LogRaw{}, err
	}
	if !found {
		return webui.LogRaw{}, webui.ErrLogNotFound
	}
	return rawFromRow("api", row)
}

func (a *App) rawDBRow(file string, id int64) (webui.LogRaw, error) {
	if a.store == nil {
		return webui.LogRaw{}, errors.New("app: store is not available")
	}
	switch file {
	case "audit":
		row, found, err := a.store.AuditByID(id)
		if err != nil {
			return webui.LogRaw{}, err
		}
		if !found {
			return webui.LogRaw{}, webui.ErrLogNotFound
		}
		return rawFromRow("audit", row)
	case "proxy":
		row, found, err := a.store.ProxyLogByID(id)
		if err != nil {
			return webui.LogRaw{}, err
		}
		if !found {
			return webui.LogRaw{}, webui.ErrLogNotFound
		}
		return rawFromRow("proxy", row)
	case "connectivity":
		row, found, err := a.store.ConnectivityProbeByID(id)
		if err != nil {
			return webui.LogRaw{}, err
		}
		if !found {
			return webui.LogRaw{}, webui.ErrLogNotFound
		}
		// The store type carries no JSON tags; render the same shape the admin
		// API already uses for probe history.
		return rawFromRow("connectivity", map[string]any{
			"id": row.ID, "kind": row.Kind, "target": row.Target, "target_id": row.TargetID,
			"ok": row.OK, "status": row.Status, "latency_ms": row.LatencyMS,
			"detail": row.Detail, "error_code": row.ErrorCode, "created_at": row.CreatedAt,
		})
	}
	return webui.LogRaw{}, fmt.Errorf("%w: unknown raw file %q", webui.ErrInvalidRequest, file)
}

func rawFromRow(file string, row any) (webui.LogRaw, error) {
	data, err := json.Marshal(row)
	if err != nil {
		return webui.LogRaw{}, err
	}
	return webui.LogRaw{Line: string(data), File: file}, nil
}

// rawFileLine returns the literal line of one day file whose timestamp matches
// ts. module and summary narrow the match when several events share a second.
func (a *App) rawFileLine(relPath, ts, module, summary string) (webui.LogRaw, error) {
	if a.dataDir == "" {
		return webui.LogRaw{}, errors.New("app: data directory is not available")
	}
	if dayOf(ts) == "" {
		return webui.LogRaw{}, fmt.Errorf("%w: ts is required for a file source", webui.ErrInvalidRequest)
	}
	lines, err := tailFileLines(filepath.Join(a.dataDir, relPath), rawScanLines)
	if err != nil {
		return webui.LogRaw{}, err
	}
	// Compare at second resolution: the timeline stores RFC3339 seconds while
	// the JSONL line may carry milliseconds.
	needle := ts[:min(len(ts), 19)]
	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		if !strings.Contains(line, needle) {
			continue
		}
		if module != "" && !lineMentionsModule(line, module) {
			continue
		}
		if summary != "" && !strings.Contains(line, summary) {
			continue
		}
		truncated := len(line) > maxRawLine
		if truncated {
			line = line[:maxRawLine]
		}
		return webui.LogRaw{Line: line, File: filepath.Base(relPath), Truncated: truncated}, nil
	}
	return webui.LogRaw{}, webui.ErrLogNotFound
}

// lineMentionsModule accepts both the JSON host line ("module":"app") and a
// plain plugin line that carries the module in its text.
func lineMentionsModule(line, module string) bool {
	module = strings.TrimSpace(module)
	if module == "" {
		return true
	}
	return strings.Contains(line, `"module":"`+module+`"`) ||
		strings.Contains(line, module)
}

// dayOf extracts the YYYY-MM-DD part of a timestamp, or "" when it has none.
func dayOf(ts string) string {
	ts = strings.TrimSpace(ts)
	if len(ts) >= 10 && ts[4] == '-' {
		return ts[:10]
	}
	return ""
}
