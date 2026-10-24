package store

import (
	"fmt"

	"aiworkgate/internal/model"
)

// SetAccountEgress assigns (or replaces) the egress mode of one account.
func (s *Store) SetAccountEgress(e model.AccountEgress) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: set account egress: store is not open")
	}
	if e.PluginID == "" || e.AccountID == "" {
		return fmt.Errorf("store: set account egress: %w: plugin_id and account_id are required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	_, err := s.db.ExecContext(ctx, `INSERT INTO account_egress (plugin_id, account_id, mode, proxy_id)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(plugin_id, account_id) DO UPDATE SET
			mode = excluded.mode,
			proxy_id = excluded.proxy_id`,
		e.PluginID, e.AccountID, e.Mode, nullStr(e.ProxyID))
	if err != nil {
		return fmt.Errorf("store: set account egress: %w", err)
	}
	return nil
}

// ListAccountEgress returns the assignments of one plugin ordered by account.
func (s *Store) ListAccountEgress(pluginID string) ([]model.AccountEgress, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: list account egress: store is not open")
	}
	if pluginID == "" {
		return nil, fmt.Errorf("store: list account egress: %w: plugin_id is required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT plugin_id, account_id, COALESCE(mode, ''), COALESCE(proxy_id, '')
		FROM account_egress WHERE plugin_id = ? ORDER BY account_id`, pluginID)
	if err != nil {
		return nil, fmt.Errorf("store: list account egress: %w", err)
	}
	defer rows.Close()

	var out []model.AccountEgress
	for rows.Next() {
		var e model.AccountEgress
		if err := rows.Scan(&e.PluginID, &e.AccountID, &e.Mode, &e.ProxyID); err != nil {
			return nil, fmt.Errorf("store: list account egress: scan: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list account egress: iterate: %w", err)
	}
	return out, nil
}

// DeleteAccountEgress removes one account assignment (back to plugin default).
func (s *Store) DeleteAccountEgress(pluginID, accountID string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: delete account egress: store is not open")
	}
	ctx, cancel := s.ctx()
	defer cancel()
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM account_egress WHERE plugin_id = ? AND account_id = ?`, pluginID, accountID); err != nil {
		return fmt.Errorf("store: delete account egress: %w", err)
	}
	return nil
}
