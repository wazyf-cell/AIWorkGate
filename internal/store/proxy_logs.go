package store

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"aiworkgate/internal/model"
)

const (
	defaultProxyLogLimit = 100
	// maxProxyLogLimit bounds one HTTP page of the proxy-log table.
	maxProxyLogLimit = 1000
	// maxProxyLogScan bounds one timeline scan of proxy rows.
	maxProxyLogScan = 50000
	// ProxyLogDefaultRetentionDays is the built-in window when the config does
	// not override it. Proxy rows are one per tunnelled CONNECT, so the volume
	// is higher than the request log and the window is shorter.
	ProxyLogDefaultRetentionDays = 7
)

// ProxyLogFilter selects proxy traffic rows. From/To are unix seconds and are
// compared against the stored `at` column; zero means unbounded. Result is an
// exact match ("ok"/"denied"/"failed"); Query is a case-insensitive substring
// over plugin, account, proxy, target and error.
type ProxyLogFilter struct {
	From      int64
	To        int64
	Result    string
	PluginID  string
	AccountID string
	Query     string
	Limit     int
	Offset    int
}

// InsertProxyLog writes one tunnel record. It never stores payload bytes.
func (s *Store) InsertProxyLog(row model.ProxyLog) (int64, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("store: insert proxy log: store is not open")
	}
	at := row.At
	if at <= 0 {
		at = nowUnix()
	}
	ts := strings.TrimSpace(row.TS)
	if ts == "" {
		ts = time.Unix(at, 0).Format(time.RFC3339)
	}
	result := strings.TrimSpace(row.Result)
	switch result {
	case model.ProxyResultOK, model.ProxyResultDenied, model.ProxyResultFailed:
	default:
		return 0, fmt.Errorf("store: insert proxy log: %w: unknown result %q", ErrInvalidArgument, result)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	res, err := s.db.ExecContext(ctx, `INSERT INTO proxy_logs
		(ts, at, plugin_id, account_id, proxy_id, proxy_label, target_host,
		 target_port, result, status, duration_ms, bytes_up, bytes_down, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ts, at, strings.TrimSpace(row.PluginID), strings.TrimSpace(row.AccountID),
		strings.TrimSpace(row.ProxyID), strings.TrimSpace(row.ProxyLabel),
		strings.TrimSpace(row.TargetHost), row.TargetPort, result, row.Status,
		row.DurationMS, row.BytesUp, row.BytesDown, strings.TrimSpace(row.Error))
	if err != nil {
		return 0, fmt.Errorf("store: insert proxy log: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: insert proxy log: last insert id: %w", err)
	}
	return id, nil
}

// QueryProxyLogs returns one page (newest first) and the pre-limit total.
func (s *Store) QueryProxyLogs(f ProxyLogFilter) ([]model.ProxyLog, int, error) {
	if s == nil || s.db == nil {
		return nil, 0, fmt.Errorf("store: query proxy logs: store is not open")
	}
	limit := f.Limit
	if limit <= 0 {
		limit = defaultProxyLogLimit
	}
	if limit > maxProxyLogLimit {
		limit = maxProxyLogLimit
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}
	where, args := proxyLogConds(f)
	ctx, cancel := s.ctx()
	defer cancel()

	var total int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM proxy_logs"+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: query proxy logs: count: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, proxyLogSelect+where+
		" ORDER BY at DESC, id DESC LIMIT ? OFFSET ?", append(args, limit, offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("store: query proxy logs: %w", err)
	}
	defer rows.Close()
	out, err := scanProxyLogs(rows)
	return out, total, err
}

// QueryProxyLogsTimelinePage returns up to limit rows older than the
// (beforeAt, beforeID) position inside the day window, newest first. beforeAt
// is Unix milliseconds and maps onto the row's `at` seconds; zero means "from
// the newest row", and a zero beforeID includes every row at beforeAt.
func (s *Store) QueryProxyLogsTimelinePage(fromDay, toDay string, beforeAt, beforeID int64, limit int) ([]model.ProxyLog, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = defaultProxyLogLimit
	}
	if limit > maxProxyLogScan {
		limit = maxProxyLogScan
	}
	from, to := localDayBounds(fromDay, toDay)
	where := " WHERE at >= ? AND at < ?"
	args := []any{from, to}
	if beforeAt > 0 {
		if beforeID <= 0 {
			beforeID = math.MaxInt64
		}
		where += " AND (at * 1000 < ? OR (at * 1000 = ? AND id < ?))"
		args = append(args, beforeAt, beforeAt, beforeID)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, proxyLogSelect+where+
		" ORDER BY at DESC, id DESC LIMIT ?", append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("store: proxy logs timeline page: %w", err)
	}
	defer rows.Close()
	return scanProxyLogs(rows)
}

// LatestProxyFailure returns the newest tunnel row with result=failed for one
// (plugin, account) whose `at` is at or after since (unix seconds). A pair with
// no such row yields (nil, nil): "nothing to report" is not an error. The
// gateway uses this read to explain a 5xx instead of only saying "Bad
// Gateway"; proxy_logs carries no credentials, so neither does the result.
func (s *Store) LatestProxyFailure(pluginID, accountID string, since int64) (*model.ProxyLog, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: latest proxy failure: store is not open")
	}
	pluginID = strings.TrimSpace(pluginID)
	accountID = strings.TrimSpace(accountID)
	if pluginID == "" || accountID == "" {
		return nil, nil
	}
	ctx, cancel := s.ctx()
	defer cancel()
	row := s.db.QueryRowContext(ctx, proxyLogSelect+
		" WHERE plugin_id = ? AND account_id = ? AND result = ? AND at >= ?"+
		" ORDER BY at DESC, id DESC LIMIT 1",
		pluginID, accountID, model.ProxyResultFailed, since)
	out, err := scanProxyLog(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: latest proxy failure: %w", err)
	}
	return &out, nil
}

// LatestProxyLog returns the newest tunnel row of one (plugin, account) whose
// `at` is at or after since, whatever its result. Egress health needs the
// newest evidence, not just the newest failure: an ok row after a failure is
// what proves the proxy recovered. No row yields (nil, nil).
func (s *Store) LatestProxyLog(pluginID, accountID string, since int64) (*model.ProxyLog, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: latest proxy log: store is not open")
	}
	pluginID = strings.TrimSpace(pluginID)
	accountID = strings.TrimSpace(accountID)
	if pluginID == "" || accountID == "" {
		return nil, nil
	}
	ctx, cancel := s.ctx()
	defer cancel()
	row := s.db.QueryRowContext(ctx, proxyLogSelect+
		" WHERE plugin_id = ? AND account_id = ? AND at >= ?"+
		" ORDER BY at DESC, id DESC LIMIT 1",
		pluginID, accountID, since)
	out, err := scanProxyLog(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: latest proxy log: %w", err)
	}
	return &out, nil
}

// MaxProxyLogID reports the newest row id, or 0.
func (s *Store) MaxProxyLogID() (int64, error) {
	if s == nil || s.db == nil {
		return 0, nil
	}
	ctx, cancel := s.ctx()
	defer cancel()
	var id int64
	if err := s.db.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(id), 0) FROM proxy_logs").Scan(&id); err != nil {
		return 0, fmt.Errorf("store: max proxy log id: %w", err)
	}
	return id, nil
}

// QueryProxyLogsSince returns rows newer than id inside the day window and the
// highest id seen, for the live timeline tail.
func (s *Store) QueryProxyLogsSince(id int64, fromDay, toDay string, limit int) ([]model.ProxyLog, int64, error) {
	if s == nil || s.db == nil {
		return nil, id, nil
	}
	if limit <= 0 {
		limit = defaultProxyLogLimit
	}
	from, to := localDayBounds(fromDay, toDay)
	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, proxyLogSelect+
		" WHERE id > ? AND at >= ? AND at < ? ORDER BY id ASC LIMIT ?", id, from, to, limit)
	if err != nil {
		return nil, id, fmt.Errorf("store: proxy logs since: %w", err)
	}
	defer rows.Close()
	out, err := scanProxyLogs(rows)
	last := id
	for _, row := range out {
		if row.ID > last {
			last = row.ID
		}
	}
	return out, last, err
}

// PruneProxyLogs removes rows older than cutoff and reports how many went.
func (s *Store) PruneProxyLogs(cutoff int64) (int64, error) {
	if s == nil || s.db == nil {
		return 0, nil
	}
	ctx, cancel := s.ctx()
	defer cancel()
	res, err := s.db.ExecContext(ctx, "DELETE FROM proxy_logs WHERE at < ?", cutoff)
	if err != nil {
		return 0, fmt.Errorf("store: prune proxy logs: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: prune proxy logs: rows affected: %w", err)
	}
	return n, nil
}

const proxyLogSelect = `SELECT id, ts, at, plugin_id, account_id, proxy_id,
	proxy_label, target_host, target_port, result, status, duration_ms,
	bytes_up, bytes_down, error FROM proxy_logs`

func proxyLogConds(f ProxyLogFilter) (string, []any) {
	var conds []string
	var args []any
	if f.From > 0 {
		conds = append(conds, "at >= ?")
		args = append(args, f.From)
	}
	if f.To > 0 {
		conds = append(conds, "at < ?")
		args = append(args, f.To)
	}
	if v := strings.TrimSpace(f.Result); v != "" {
		conds = append(conds, "result = ?")
		args = append(args, v)
	}
	if v := strings.TrimSpace(f.PluginID); v != "" {
		conds = append(conds, "plugin_id = ?")
		args = append(args, v)
	}
	if v := strings.TrimSpace(f.AccountID); v != "" {
		conds = append(conds, "account_id = ?")
		args = append(args, v)
	}
	if q := strings.ToLower(strings.TrimSpace(f.Query)); q != "" {
		like := "%" + q + "%"
		conds = append(conds, `(lower(plugin_id) LIKE ? OR lower(account_id) LIKE ?
			OR lower(proxy_label) LIKE ? OR lower(target_host) LIKE ? OR lower(error) LIKE ?)`)
		args = append(args, like, like, like, like, like)
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

func scanProxyLog(sc rowScanner) (model.ProxyLog, error) {
	var row model.ProxyLog
	if err := sc.Scan(&row.ID, &row.TS, &row.At, &row.PluginID, &row.AccountID,
		&row.ProxyID, &row.ProxyLabel, &row.TargetHost, &row.TargetPort,
		&row.Result, &row.Status, &row.DurationMS, &row.BytesUp, &row.BytesDown,
		&row.Error); err != nil {
		return model.ProxyLog{}, err
	}
	return row, nil
}

func scanProxyLogs(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]model.ProxyLog, error) {
	out := make([]model.ProxyLog, 0, 64)
	for rows.Next() {
		row, err := scanProxyLog(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan proxy log: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: scan proxy logs: %w", err)
	}
	return out, nil
}

// localDayBounds turns inclusive local days into a half-open unix window.
// Empty or unparseable bounds widen to the full table on that side.
func localDayBounds(fromDay, toDay string) (int64, int64) {
	loc := time.Local
	var from, to int64
	if fromDay != "" {
		if t, err := time.ParseInLocation("2006-01-02", fromDay, loc); err == nil {
			from = t.Unix()
		}
	}
	if toDay != "" {
		if t, err := time.ParseInLocation("2006-01-02", toDay, loc); err == nil {
			to = t.AddDate(0, 0, 1).Unix()
		}
	}
	return from, to
}
