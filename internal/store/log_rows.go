package store

import (
	"database/sql"
	"fmt"

	"aiworkgate/internal/model"
)

// Single-row lookups backing GET /api/logs/raw. Bodies are deliberately absent
// from the API row: previews are served by /api/logs/body, which gates them
// behind the debug switch.

// APILogByReqID returns the request-log row for one req id. The two body
// columns are not selected.
func (s *Store) APILogByReqID(reqID string) (model.APIRequestLog, bool, error) {
	if s == nil || s.db == nil {
		return model.APIRequestLog{}, false, fmt.Errorf("store: api log by req id: store is not open")
	}
	ctx, cancel := s.ctx()
	defer cancel()
	row := s.db.QueryRowContext(ctx, `SELECT `+apiLogColumnsNoBodies+` FROM api_logs WHERE req_id = ? LIMIT 1`, reqID)

	var (
		rec     model.APIRequestLog
		stream  int64
		credits sql.NullFloat64
	)
	err := row.Scan(
		&rec.TS, &rec.ReqID, &rec.Source, &rec.Model, &rec.LogicalModel,
		&rec.KeyID, &rec.KeyName, &rec.Account, &rec.ConversationID, &stream, &rec.Status,
		&rec.LatencyMS, &rec.TTFBMS, &rec.FirstTokenAt,
		&rec.PromptTokens, &rec.CompletionTokens, &rec.CachedTokens, &credits, &rec.Error,
	)
	if err == sql.ErrNoRows {
		return model.APIRequestLog{}, false, nil
	}
	if err != nil {
		return model.APIRequestLog{}, false, fmt.Errorf("store: api log by req id: %w", err)
	}
	rec.Stream = stream != 0
	if credits.Valid {
		v := credits.Float64
		rec.Credits = &v
	}
	return rec, true, nil
}

// AuditByID returns one audit row.
func (s *Store) AuditByID(id int64) (model.AuditEntry, bool, error) {
	if s == nil || s.db == nil {
		return model.AuditEntry{}, false, fmt.Errorf("store: audit by id: store is not open")
	}
	ctx, cancel := s.ctx()
	defer cancel()
	row := s.db.QueryRowContext(ctx, `SELECT id, ts, COALESCE(category, ''), COALESCE(action, ''),
		COALESCE(target, ''), COALESCE(detail, ''), COALESCE(result, '')
		FROM audit_log WHERE id = ?`, id)

	var entry model.AuditEntry
	err := row.Scan(&entry.ID, &entry.TS, &entry.Category, &entry.Action,
		&entry.Target, &entry.Detail, &entry.Result)
	if err == sql.ErrNoRows {
		return model.AuditEntry{}, false, nil
	}
	if err != nil {
		return model.AuditEntry{}, false, fmt.Errorf("store: audit by id: %w", err)
	}
	return entry, true, nil
}

// ProxyLogByID returns one proxy tunnel row.
func (s *Store) ProxyLogByID(id int64) (model.ProxyLog, bool, error) {
	if s == nil || s.db == nil {
		return model.ProxyLog{}, false, fmt.Errorf("store: proxy log by id: store is not open")
	}
	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, proxyLogSelect+" WHERE id = ? LIMIT ?", id, 1)
	if err != nil {
		return model.ProxyLog{}, false, fmt.Errorf("store: proxy log by id: %w", err)
	}
	defer rows.Close()
	out, err := scanProxyLogs(rows)
	if err != nil || len(out) == 0 {
		return model.ProxyLog{}, false, err
	}
	return out[0], true, nil
}

// ConnectivityProbeByID returns one probe row.
func (s *Store) ConnectivityProbeByID(id int64) (ConnectivityProbe, bool, error) {
	if s == nil || s.db == nil {
		return ConnectivityProbe{}, false, fmt.Errorf("store: connectivity probe by id: store is not open")
	}
	ctx, cancel := s.ctx()
	defer cancel()
	row := s.db.QueryRowContext(ctx, `SELECT id, kind, target, target_id, ok, status,
		latency_ms, detail, error_code, created_at FROM connectivity_probes WHERE id = ?`, id)

	var (
		probe ConnectivityProbe
		ok    int64
	)
	err := row.Scan(&probe.ID, &probe.Kind, &probe.Target, &probe.TargetID, &ok,
		&probe.Status, &probe.LatencyMS, &probe.Detail, &probe.ErrorCode, &probe.CreatedAt)
	if err == sql.ErrNoRows {
		return ConnectivityProbe{}, false, nil
	}
	if err != nil {
		return ConnectivityProbe{}, false, fmt.Errorf("store: connectivity probe by id: %w", err)
	}
	probe.OK = ok != 0
	return probe, true, nil
}
