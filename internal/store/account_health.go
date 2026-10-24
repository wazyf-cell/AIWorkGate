package store

import (
	"database/sql"
	"fmt"
	"strings"

	"aiworkgate/internal/model"
)

// This file owns the check-in health state of an account: the four account_meta
// columns status_kind/cooldown_until/cooldown_reason/failure_count and the
// dedup_scope/device_key columns of checkin_results.
//
// Ported from A (MIT, 朱天伟): the five-state account model in
// api_server/pool.rs (state_str) and the classify_error/save_cooldown rules in
// tasks/trae_checkin.rs. The host stores the same facts, but keeps them in the
// account_meta row so they survive a restart without a second file.
//
// Writes here only ever touch the health columns. The overlay (alias, group,
// priority, weight, enabled_override, note, updated_at) is written by
// accounts.go and stays untouched, and vice versa.

// AccountHealth is the health slice of one account_meta row.
type AccountHealth struct {
	PluginID       string `json:"plugin_id"`
	AccountID      string `json:"account_id"`
	StatusKind     string `json:"status_kind,omitempty"`
	CooldownUntil  int64  `json:"cooldown_until,omitempty"`
	CooldownReason string `json:"cooldown_reason,omitempty"`
	FailureCount   int    `json:"failure_count,omitempty"`
}

// healthColumns is the SELECT list of the health tuple, in scan order.
const healthColumns = `COALESCE(status_kind, ''), COALESCE(cooldown_until, 0),
	COALESCE(cooldown_reason, ''), COALESCE(failure_count, 0)`

// GetAccountHealth reads the health tuple of one account. ok is false when the
// account has no account_meta row at all (meaning healthy, never failed).
func (s *Store) GetAccountHealth(pluginID, accountID string) (AccountHealth, bool, error) {
	health := AccountHealth{PluginID: pluginID, AccountID: accountID}
	if s == nil || s.db == nil {
		return health, false, fmt.Errorf("store: get account health: store is not open")
	}
	if pluginID == "" || accountID == "" {
		return health, false, fmt.Errorf("store: get account health: %w: plugin_id and account_id are required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	err := s.db.QueryRowContext(ctx, `SELECT plugin_id, account_id, `+healthColumns+`
		FROM account_meta WHERE plugin_id = ? AND account_id = ?`, pluginID, accountID).
		Scan(&health.PluginID, &health.AccountID, &health.StatusKind,
			&health.CooldownUntil, &health.CooldownReason, &health.FailureCount)
	if err == sql.ErrNoRows {
		return AccountHealth{}, false, nil
	}
	if err != nil {
		return AccountHealth{}, false, fmt.Errorf("store: get account health: %w", err)
	}
	return health, true, nil
}

// ListAccountHealth returns every account with a health signal (a kind, a
// cooldown or a failure count), ordered by plugin and account.
func (s *Store) ListAccountHealth() ([]AccountHealth, error) {
	return s.listAccountHealth("")
}

// ListAccountHealthForPlugin returns the health rows of one plugin.
func (s *Store) ListAccountHealthForPlugin(pluginID string) ([]AccountHealth, error) {
	if strings.TrimSpace(pluginID) == "" {
		return nil, fmt.Errorf("store: list account health: %w: plugin_id is required", ErrInvalidArgument)
	}
	return s.listAccountHealth(pluginID)
}

func (s *Store) listAccountHealth(pluginID string) ([]AccountHealth, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: list account health: store is not open")
	}
	q := `SELECT plugin_id, account_id, ` + healthColumns + `
		FROM account_meta
		WHERE status_kind IS NOT NULL OR cooldown_until IS NOT NULL OR COALESCE(failure_count, 0) > 0`
	var args []any
	if pluginID != "" {
		q += ` AND plugin_id = ?`
		args = append(args, pluginID)
	}
	q += ` ORDER BY plugin_id, account_id`
	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list account health: %w", err)
	}
	defer rows.Close()
	var out []AccountHealth
	for rows.Next() {
		var health AccountHealth
		if err := rows.Scan(&health.PluginID, &health.AccountID, &health.StatusKind,
			&health.CooldownUntil, &health.CooldownReason, &health.FailureCount); err != nil {
			return nil, fmt.Errorf("store: list account health: scan: %w", err)
		}
		out = append(out, health)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list account health: iterate: %w", err)
	}
	return out, nil
}

// SetAccountHealth writes the four health columns and nothing else. A missing
// account_meta row is created with only the primary key and the health columns,
// so the overlay columns keep their defaults (NULL/0).
func (s *Store) SetAccountHealth(health AccountHealth) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: set account health: store is not open")
	}
	if health.PluginID == "" || health.AccountID == "" {
		return fmt.Errorf("store: set account health: %w: plugin_id and account_id are required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	_, err := s.db.ExecContext(ctx, `INSERT INTO account_meta
		(plugin_id, account_id, status_kind, cooldown_until, cooldown_reason, failure_count)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(plugin_id, account_id) DO UPDATE SET
			status_kind = excluded.status_kind,
			cooldown_until = excluded.cooldown_until,
			cooldown_reason = excluded.cooldown_reason,
			failure_count = excluded.failure_count`,
		health.PluginID, health.AccountID, nullStr(health.StatusKind),
		nullI64(health.CooldownUntil), nullStr(health.CooldownReason), health.FailureCount)
	if err != nil {
		return fmt.Errorf("store: set account health: %w", err)
	}
	return nil
}

// ClearAccountHealth returns an account to the healthy state after a successful
// check-in. It never creates a row and never touches the overlay.
func (s *Store) ClearAccountHealth(pluginID, accountID string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: clear account health: store is not open")
	}
	if pluginID == "" || accountID == "" {
		return fmt.Errorf("store: clear account health: %w: plugin_id and account_id are required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `UPDATE account_meta SET
			status_kind = NULL,
			cooldown_until = NULL,
			cooldown_reason = NULL,
			failure_count = 0
		WHERE plugin_id = ? AND account_id = ?`, pluginID, accountID); err != nil {
		return fmt.Errorf("store: clear account health: %w", err)
	}
	return nil
}

// BumpAccountFailure increments failure_count and returns the new value. It is
// used for failures the host cannot attribute: they never cool the account
// down, they only accumulate evidence.
func (s *Store) BumpAccountFailure(pluginID, accountID string) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("store: bump account failure: store is not open")
	}
	if pluginID == "" || accountID == "" {
		return 0, fmt.Errorf("store: bump account failure: %w: plugin_id and account_id are required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	var count int
	err := s.db.QueryRowContext(ctx, `INSERT INTO account_meta (plugin_id, account_id, failure_count)
		VALUES (?, ?, 1)
		ON CONFLICT(plugin_id, account_id) DO UPDATE SET
			failure_count = COALESCE(failure_count, 0) + 1
		RETURNING COALESCE(failure_count, 0)`, pluginID, accountID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("store: bump account failure: %w", err)
	}
	return count, nil
}

// CheckinSucceededOn reports whether the account already has an ok/already row
// for date. The scheduler uses it to avoid overwriting a real success with a
// later "skipped" note.
func (s *Store) CheckinSucceededOn(pluginID, accountID, date string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("store: checkin succeeded on: store is not open")
	}
	if pluginID == "" || accountID == "" || date == "" {
		return false, fmt.Errorf("store: checkin succeeded on: %w: plugin_id, account_id and date are required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM checkin_results
		WHERE plugin_id = ? AND account_id = ? AND date = ? AND result IN (?, ?)`,
		pluginID, accountID, date, model.CheckinOK, model.CheckinAlready).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("store: checkin succeeded on: %w", err)
	}
	return n > 0, nil
}

// DeviceCheckedIn reports whether a device-scoped check-in already succeeded on
// date for the plugin (plan 2026-10-21 §4). The lookup matches A's natural-day
// rule: one successful claim per (plugin, day, device), whatever account made it.
func (s *Store) DeviceCheckedIn(pluginID, date, deviceKey string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("store: device checked in: store is not open")
	}
	if pluginID == "" || date == "" || strings.TrimSpace(deviceKey) == "" {
		return false, fmt.Errorf("store: device checked in: %w: plugin_id, date and device_key are required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM checkin_results
		WHERE plugin_id = ? AND date = ? AND device_key = ? AND dedup_scope = ? AND result IN (?, ?)`,
		pluginID, date, deviceKey, model.DedupScopeDevice, model.CheckinOK, model.CheckinAlready).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("store: device checked in: %w", err)
	}
	return n > 0, nil
}

// LastDeviceKey returns the most recent device key reported for an account, so
// a later pass can tell whether that account sits on an already-signed device
// without calling the plugin first.
func (s *Store) LastDeviceKey(pluginID, accountID string) (string, bool, error) {
	if s == nil || s.db == nil {
		return "", false, fmt.Errorf("store: last device key: store is not open")
	}
	if pluginID == "" || accountID == "" {
		return "", false, fmt.Errorf("store: last device key: %w: plugin_id and account_id are required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	var key string
	err := s.db.QueryRowContext(ctx, `SELECT device_key FROM checkin_results
		WHERE plugin_id = ? AND account_id = ? AND dedup_scope = ? AND device_key IS NOT NULL AND device_key <> ''
		ORDER BY date DESC, COALESCE(at, 0) DESC, id DESC LIMIT 1`,
		pluginID, accountID, model.DedupScopeDevice).Scan(&key)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: last device key: %w", err)
	}
	return key, true, nil
}

// nullI64 stores zero as NULL so "no cooldown" stays distinguishable from an
// epoch timestamp.
func nullI64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}
