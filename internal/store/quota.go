package store

import (
	"fmt"

	"aiworkgate/internal/model"
)

// SaveQuotaSnapshot records one quota observation. The primary key includes
// the timestamp, so repeated polls within the same second collapse.
func (s *Store) SaveQuotaSnapshot(snap model.QuotaSnapshot) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: save quota snapshot: store is not open")
	}
	if snap.PluginID == "" || snap.AccountID == "" {
		return fmt.Errorf("store: save quota snapshot: %w: plugin_id and account_id are required", ErrInvalidArgument)
	}
	at := snap.At
	if at == 0 {
		at = nowUnix()
	}
	ctx, cancel := s.ctx()
	defer cancel()
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO quota_snapshots
		(plugin_id, account_id, remaining, total, expires_at, plan, at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		snap.PluginID, snap.AccountID, snap.Remaining, snap.Total, snap.ExpiresAt, nullStr(snap.Plan), at)
	if err != nil {
		return fmt.Errorf("store: save quota snapshot: %w", err)
	}
	return nil
}

// QueryQuotaSnapshots returns snapshots at or after since (unix seconds,
// <=0 means all) for one account, or for every account of the plugin when
// accountID is empty. Newest first.
func (s *Store) QueryQuotaSnapshots(pluginID, accountID string, since int64) ([]model.QuotaSnapshot, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: query quota snapshots: store is not open")
	}
	if pluginID == "" {
		return nil, fmt.Errorf("store: query quota snapshots: %w: plugin_id is required", ErrInvalidArgument)
	}
	q := `SELECT plugin_id, account_id, COALESCE(remaining, 0), COALESCE(total, 0),
		COALESCE(expires_at, 0), COALESCE(plan, ''), at
		FROM quota_snapshots WHERE plugin_id = ?`
	args := []any{pluginID}
	if accountID != "" {
		q += " AND account_id = ?"
		args = append(args, accountID)
	}
	if since > 0 {
		q += " AND at >= ?"
		args = append(args, since)
	}
	q += " ORDER BY at DESC"

	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query quota snapshots: %w", err)
	}
	defer rows.Close()

	var out []model.QuotaSnapshot
	for rows.Next() {
		var snap model.QuotaSnapshot
		if err := rows.Scan(&snap.PluginID, &snap.AccountID, &snap.Remaining, &snap.Total,
			&snap.ExpiresAt, &snap.Plan, &snap.At); err != nil {
			return nil, fmt.Errorf("store: query quota snapshots: scan: %w", err)
		}
		out = append(out, snap)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: query quota snapshots: iterate: %w", err)
	}
	return out, nil
}

// CleanupQuotaSnapshots deletes snapshots older than the given unix timestamp.
func (s *Store) CleanupQuotaSnapshots(before int64) (int64, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("store: cleanup quota snapshots: store is not open")
	}
	if before <= 0 {
		return 0, fmt.Errorf("store: cleanup quota snapshots: %w: before must be positive", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		res, err := s.db.ExecContext(ctx, `DELETE FROM quota_snapshots WHERE rowid IN (
			SELECT rowid FROM quota_snapshots WHERE at < ? LIMIT ?)`, before, cleanupBatch)
		if err != nil {
			return total, fmt.Errorf("store: cleanup quota snapshots: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("store: cleanup quota snapshots: rows affected: %w", err)
		}
		total += n
		if n < cleanupBatch {
			return total, nil
		}
	}
}
