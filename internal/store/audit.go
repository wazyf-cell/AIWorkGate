package store

import (
	"fmt"
	"strings"
	"time"

	"aiworkgate/internal/model"
)

// AppendAudit records one management operation and returns its row id. The
// detail column must never contain credentials.
func (s *Store) AppendAudit(entry model.AuditEntry) (int64, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("store: append audit: store is not open")
	}
	ts := strings.TrimSpace(entry.TS)
	if ts == "" {
		ts = time.Now().Format(time.RFC3339)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	res, err := s.db.ExecContext(ctx, `INSERT INTO audit_log (ts, category, action, target, detail, result)
		VALUES (?, ?, ?, ?, ?, ?)`,
		ts, nullStr(strings.TrimSpace(entry.Category)), nullStr(strings.TrimSpace(entry.Action)),
		nullStr(strings.TrimSpace(entry.Target)), nullStr(entry.Detail), nullStr(strings.TrimSpace(entry.Result)))
	if err != nil {
		return 0, fmt.Errorf("store: append audit: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: append audit: last id: %w", err)
	}
	return id, nil
}

// ListAudit returns the newest entries first. limit <= 0 falls back to 100 and
// is capped at 1000. category filters one category when set.
func (s *Store) ListAudit(category string, limit int) ([]model.AuditEntry, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: list audit: store is not open")
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	q := `SELECT id, ts, COALESCE(category, ''), COALESCE(action, ''),
		COALESCE(target, ''), COALESCE(detail, ''), COALESCE(result, '')
		FROM audit_log`
	var args []any
	if category = strings.TrimSpace(category); category != "" {
		q += ` WHERE category = ?`
		args = append(args, category)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list audit: %w", err)
	}
	defer rows.Close()

	var out []model.AuditEntry
	for rows.Next() {
		var entry model.AuditEntry
		if err := rows.Scan(&entry.ID, &entry.TS, &entry.Category, &entry.Action,
			&entry.Target, &entry.Detail, &entry.Result); err != nil {
			return nil, fmt.Errorf("store: list audit: scan: %w", err)
		}
		out = append(out, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list audit: iterate: %w", err)
	}
	return out, nil
}

// PruneAudit deletes every entry recorded before the given unix time and
// returns the number of rows removed. audit_log.ts is an RFC3339 string, so the
// cutoff is formatted the same way, exactly like CleanupAPIRequestLogs does for
// api_logs; without this call the table grows forever.
func (s *Store) PruneAudit(olderThan int64) (int64, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("store: prune audit: store is not open")
	}
	if olderThan <= 0 {
		return 0, fmt.Errorf("store: prune audit: %w: olderThan must be positive", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	res, err := s.db.ExecContext(ctx, `DELETE FROM audit_log WHERE ts < ?`,
		time.Unix(olderThan, 0).Format(time.RFC3339))
	if err != nil {
		return 0, fmt.Errorf("store: prune audit: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: prune audit: rows affected: %w", err)
	}
	return n, nil
}
