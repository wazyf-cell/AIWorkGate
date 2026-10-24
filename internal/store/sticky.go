package store

import (
	"fmt"
	"strings"

	"aiworkgate/internal/model"
)

// UpsertStickyBinding pins one (scope, key) to an account until expiresAt.
func (s *Store) UpsertStickyBinding(binding model.StickyBinding) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: upsert sticky binding: store is not open")
	}
	scope := strings.TrimSpace(binding.Scope)
	key := strings.TrimSpace(binding.Key)
	if scope == "" || key == "" {
		return fmt.Errorf("store: upsert sticky binding: %w: scope and key are required", ErrInvalidArgument)
	}
	if binding.PluginID == "" || binding.AccountID == "" {
		return fmt.Errorf("store: upsert sticky binding: %w: plugin_id and account_id are required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	_, err := s.db.ExecContext(ctx, `INSERT INTO sticky_bindings (scope, key, plugin_id, account_id, expires_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(scope, key) DO UPDATE SET
			plugin_id = excluded.plugin_id,
			account_id = excluded.account_id,
			expires_at = excluded.expires_at`,
		scope, key, binding.PluginID, binding.AccountID, binding.ExpiresAt)
	if err != nil {
		return fmt.Errorf("store: upsert sticky binding: %w", err)
	}
	return nil
}

// ListStickyBindings returns the bindings of one scope, or of every scope when
// scope is empty, newest expiry first.
func (s *Store) ListStickyBindings(scope string) ([]model.StickyBinding, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: list sticky bindings: store is not open")
	}
	q := `SELECT scope, key, plugin_id, account_id, expires_at FROM sticky_bindings`
	var args []any
	if scope = strings.TrimSpace(scope); scope != "" {
		q += ` WHERE scope = ?`
		args = append(args, scope)
	}
	q += ` ORDER BY expires_at DESC, scope, key`

	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list sticky bindings: %w", err)
	}
	defer rows.Close()

	var out []model.StickyBinding
	for rows.Next() {
		var binding model.StickyBinding
		if err := rows.Scan(&binding.Scope, &binding.Key, &binding.PluginID,
			&binding.AccountID, &binding.ExpiresAt); err != nil {
			return nil, fmt.Errorf("store: list sticky bindings: scan: %w", err)
		}
		out = append(out, binding)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list sticky bindings: iterate: %w", err)
	}
	return out, nil
}

// DeleteStickyBinding removes one (scope, key) pin.
func (s *Store) DeleteStickyBinding(scope, key string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: delete sticky binding: store is not open")
	}
	ctx, cancel := s.ctx()
	defer cancel()
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM sticky_bindings WHERE scope = ? AND key = ?`, scope, key); err != nil {
		return fmt.Errorf("store: delete sticky binding: %w", err)
	}
	return nil
}

// DeleteStickyForAccount drops every pin pointing at one account (account
// delete or disable).
func (s *Store) DeleteStickyForAccount(pluginID, accountID string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: delete sticky for account: store is not open")
	}
	ctx, cancel := s.ctx()
	defer cancel()
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM sticky_bindings WHERE plugin_id = ? AND account_id = ?`, pluginID, accountID); err != nil {
		return fmt.Errorf("store: delete sticky for account: %w", err)
	}
	return nil
}

// DeleteExpiredStickyBindings removes pins whose expires_at is not after now
// and reports how many were removed.
func (s *Store) DeleteExpiredStickyBindings(now int64) (int64, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("store: delete expired sticky: store is not open")
	}
	if now <= 0 {
		now = nowUnix()
	}
	ctx, cancel := s.ctx()
	defer cancel()
	res, err := s.db.ExecContext(ctx, `DELETE FROM sticky_bindings WHERE expires_at <= ?`, now)
	if err != nil {
		return 0, fmt.Errorf("store: delete expired sticky: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: delete expired sticky: rows affected: %w", err)
	}
	return n, nil
}

// ---------------------------------------------------------------- usage_daily

// UpsertUsageDaily writes one daily aggregate, replacing the previous row for
// the same (day, plugin, account, model) key.
func (s *Store) UpsertUsageDaily(row model.UsageDailyRow) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: upsert usage daily: store is not open")
	}
	if strings.TrimSpace(row.Day) == "" || row.PluginID == "" || row.Model == "" {
		return fmt.Errorf("store: upsert usage daily: %w: day, plugin_id and model are required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	_, err := s.db.ExecContext(ctx, `INSERT INTO usage_daily
		(day, plugin_id, account_id, model, requests, errors, prompt_tokens, completion_tokens,
		 credits, latency_ms_sum, ttfb_ms_sum, ttfb_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(day, plugin_id, account_id, model) DO UPDATE SET
			requests = excluded.requests,
			errors = excluded.errors,
			prompt_tokens = excluded.prompt_tokens,
			completion_tokens = excluded.completion_tokens,
			credits = excluded.credits,
			latency_ms_sum = excluded.latency_ms_sum,
			ttfb_ms_sum = excluded.ttfb_ms_sum,
			ttfb_count = excluded.ttfb_count`,
		row.Day, row.PluginID, row.AccountID, row.Model, row.Requests, row.Errors,
		row.PromptTokens, row.CompletionTokens, row.Credits,
		row.LatencyMSSum, row.TTFBMSSum, row.TTFBCount)
	if err != nil {
		return fmt.Errorf("store: upsert usage daily: %w", err)
	}
	return nil
}

// ListUsageDaily returns the aggregates between fromDay and toDay inclusive
// (YYYY-MM-DD); empty bounds are open. pluginID filters one plugin when set.
func (s *Store) ListUsageDaily(fromDay, toDay, pluginID string) ([]model.UsageDailyRow, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: list usage daily: store is not open")
	}
	q := `SELECT day, plugin_id, account_id, model, COALESCE(requests, 0), COALESCE(errors, 0),
		COALESCE(prompt_tokens, 0), COALESCE(completion_tokens, 0), COALESCE(credits, 0),
		COALESCE(latency_ms_sum, 0), COALESCE(ttfb_ms_sum, 0), COALESCE(ttfb_count, 0)
		FROM usage_daily`
	var (
		conds []string
		args  []any
	)
	if fromDay = strings.TrimSpace(fromDay); fromDay != "" {
		conds = append(conds, "day >= ?")
		args = append(args, fromDay)
	}
	if toDay = strings.TrimSpace(toDay); toDay != "" {
		conds = append(conds, "day <= ?")
		args = append(args, toDay)
	}
	if pluginID = strings.TrimSpace(pluginID); pluginID != "" {
		conds = append(conds, "plugin_id = ?")
		args = append(args, pluginID)
	}
	for i, cond := range conds {
		if i == 0 {
			q += " WHERE " + cond
		} else {
			q += " AND " + cond
		}
	}
	q += " ORDER BY day, plugin_id, account_id, model"

	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list usage daily: %w", err)
	}
	defer rows.Close()

	var out []model.UsageDailyRow
	for rows.Next() {
		var row model.UsageDailyRow
		if err := rows.Scan(&row.Day, &row.PluginID, &row.AccountID, &row.Model,
			&row.Requests, &row.Errors, &row.PromptTokens, &row.CompletionTokens,
			&row.Credits, &row.LatencyMSSum, &row.TTFBMSSum, &row.TTFBCount); err != nil {
			return nil, fmt.Errorf("store: list usage daily: scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list usage daily: iterate: %w", err)
	}
	return out, nil
}

// DeleteUsageDailyBefore removes aggregates older than day (exclusive).
func (s *Store) DeleteUsageDailyBefore(day string) (int64, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("store: delete usage daily: store is not open")
	}
	day = strings.TrimSpace(day)
	if day == "" {
		return 0, fmt.Errorf("store: delete usage daily: %w: day is required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	res, err := s.db.ExecContext(ctx, `DELETE FROM usage_daily WHERE day < ?`, day)
	if err != nil {
		return 0, fmt.Errorf("store: delete usage daily: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: delete usage daily: rows affected: %w", err)
	}
	return n, nil
}
