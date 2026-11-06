package store

import (
	"fmt"
	"strings"
	"time"

	"aiworkgate/internal/model"
)

// RecordUsageClear appends one clear marker. Historical api_logs rows are never
// touched (D-54: only the event is recorded).
func (s *Store) RecordUsageClear(at int64, note string) (model.UsageClear, error) {
	if s == nil || s.db == nil {
		return model.UsageClear{}, fmt.Errorf("store: record usage clear: store is not open")
	}
	if at <= 0 {
		at = time.Now().Unix()
	}
	note = strings.TrimSpace(note)
	ctx, cancel := s.ctx()
	defer cancel()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO usage_clears (cleared_at, note) VALUES (?, ?)`, at, note)
	if err != nil {
		return model.UsageClear{}, fmt.Errorf("store: record usage clear: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return model.UsageClear{}, fmt.Errorf("store: record usage clear: %w", err)
	}
	return model.UsageClear{ID: id, ClearedAt: at, Note: note}, nil
}

// ListUsageClears returns the newest markers first.
func (s *Store) ListUsageClears(limit int) ([]model.UsageClear, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: list usage clears: store is not open")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, cleared_at, COALESCE(note, '') FROM usage_clears
		 ORDER BY cleared_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list usage clears: %w", err)
	}
	defer rows.Close()
	out := make([]model.UsageClear, 0, limit)
	for rows.Next() {
		var item model.UsageClear
		if err := rows.Scan(&item.ID, &item.ClearedAt, &item.Note); err != nil {
			return nil, fmt.Errorf("store: list usage clears: scan: %w", err)
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
