package store

import (
	"fmt"
	"strings"
)

// SchedulerCooldown is one durable request-path cooldown. An empty ModelID
// marks an account-wide cooldown; a non-empty ModelID marks one account+model
// pair. Strikes carries the 429 backoff counter so the doubling survives a
// restart.
//
// Ported from A (TraeWorkAssistant, MIT, 朱天伟) models.rs:712-731 and
// store/docs.rs:1266-1290: A stores `until` together with `error_count` in the
// `account_cooldowns` table and restores both into the pool on startup
// (api_server/pool.rs:331-370).
type SchedulerCooldown struct {
	PluginID  string
	AccountID string
	ModelID   string
	Until     int64
	Reason    string
	Strikes   int
	UpdatedAt int64
}

// LoadSchedulerCooldowns prunes rows whose deadline is not after now (when now
// is positive) and returns the surviving rows ordered by key. Pruning on load
// is deliberate: a restart must not resurrect a cooldown that already expired.
func (s *Store) LoadSchedulerCooldowns(now int64) ([]SchedulerCooldown, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: load scheduler cooldowns: store is not open")
	}
	ctx, cancel := s.ctx()
	defer cancel()

	if now > 0 {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM scheduler_cooldowns WHERE until <= ?`, now); err != nil {
			return nil, fmt.Errorf("store: load scheduler cooldowns: prune: %w", err)
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT plugin_id, account_id, model_id, until,
		COALESCE(reason, ''), COALESCE(strikes, 0), COALESCE(updated_at, 0)
		FROM scheduler_cooldowns
		ORDER BY plugin_id, account_id, model_id`)
	if err != nil {
		return nil, fmt.Errorf("store: load scheduler cooldowns: %w", err)
	}
	defer rows.Close()

	var out []SchedulerCooldown
	for rows.Next() {
		var row SchedulerCooldown
		if err := rows.Scan(&row.PluginID, &row.AccountID, &row.ModelID, &row.Until,
			&row.Reason, &row.Strikes, &row.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: load scheduler cooldowns: scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: load scheduler cooldowns: iterate: %w", err)
	}
	return out, nil
}

// UpsertSchedulerCooldown writes one cooldown row, replacing the previous row
// for the same (plugin, account, model) key.
func (s *Store) UpsertSchedulerCooldown(row SchedulerCooldown) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: upsert scheduler cooldown: store is not open")
	}
	pluginID := strings.TrimSpace(row.PluginID)
	accountID := strings.TrimSpace(row.AccountID)
	if pluginID == "" || accountID == "" {
		return fmt.Errorf("store: upsert scheduler cooldown: %w: plugin_id and account_id are required", ErrInvalidArgument)
	}
	if row.Until <= 0 {
		return fmt.Errorf("store: upsert scheduler cooldown: %w: until is required", ErrInvalidArgument)
	}
	updatedAt := row.UpdatedAt
	if updatedAt <= 0 {
		updatedAt = nowUnix()
	}
	ctx, cancel := s.ctx()
	defer cancel()
	_, err := s.db.ExecContext(ctx, `INSERT INTO scheduler_cooldowns
		(plugin_id, account_id, model_id, until, reason, strikes, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(plugin_id, account_id, model_id) DO UPDATE SET
			until = excluded.until,
			reason = excluded.reason,
			strikes = excluded.strikes,
			updated_at = excluded.updated_at`,
		pluginID, accountID, strings.TrimSpace(row.ModelID), row.Until,
		row.Reason, row.Strikes, updatedAt)
	if err != nil {
		return fmt.Errorf("store: upsert scheduler cooldown: %w", err)
	}
	return nil
}

// DeleteSchedulerCooldown removes one cooldown row (modelID may be empty for
// the account-wide row).
func (s *Store) DeleteSchedulerCooldown(pluginID, accountID, modelID string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: delete scheduler cooldown: store is not open")
	}
	pluginID = strings.TrimSpace(pluginID)
	accountID = strings.TrimSpace(accountID)
	if pluginID == "" || accountID == "" {
		return fmt.Errorf("store: delete scheduler cooldown: %w: plugin_id and account_id are required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM scheduler_cooldowns WHERE plugin_id = ? AND account_id = ? AND model_id = ?`,
		pluginID, accountID, strings.TrimSpace(modelID)); err != nil {
		return fmt.Errorf("store: delete scheduler cooldown: %w", err)
	}
	return nil
}

// PruneSchedulerCooldowns removes rows whose deadline is not after now and
// reports how many were removed. now <= 0 means the current wall clock.
func (s *Store) PruneSchedulerCooldowns(now int64) (int64, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("store: prune scheduler cooldowns: store is not open")
	}
	if now <= 0 {
		now = nowUnix()
	}
	ctx, cancel := s.ctx()
	defer cancel()
	res, err := s.db.ExecContext(ctx, `DELETE FROM scheduler_cooldowns WHERE until <= ?`, now)
	if err != nil {
		return 0, fmt.Errorf("store: prune scheduler cooldowns: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: prune scheduler cooldowns: rows affected: %w", err)
	}
	return n, nil
}
