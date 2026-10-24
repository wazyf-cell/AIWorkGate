package store

import (
	"database/sql"
	"fmt"
	"strings"

	"aiworkgate/internal/model"
)

// UpsertAccountGroup creates or renames one group. The id and name must be
// non-empty; a duplicate name violates the unique index and rolls back.
func (s *Store) UpsertAccountGroup(group model.AccountGroup) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: upsert account group: store is not open")
	}
	id := strings.TrimSpace(group.ID)
	name := strings.TrimSpace(group.Name)
	if id == "" || name == "" {
		return fmt.Errorf("store: upsert account group: %w: id and name are required", ErrInvalidArgument)
	}
	createdAt := group.CreatedAt
	if createdAt == 0 {
		createdAt = nowUnix()
	}
	ctx, cancel := s.ctx()
	defer cancel()
	_, err := s.db.ExecContext(ctx, `INSERT INTO account_groups (id, name, color, sort, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			color = excluded.color,
			sort = excluded.sort`,
		id, name, nullStr(strings.TrimSpace(group.Color)), group.Sort, createdAt)
	if err != nil {
		return fmt.Errorf("store: upsert account group: %w", err)
	}
	return nil
}

// GetAccountGroup reads one group.
func (s *Store) GetAccountGroup(id string) (model.AccountGroup, bool, error) {
	if s == nil || s.db == nil {
		return model.AccountGroup{}, false, fmt.Errorf("store: get account group: store is not open")
	}
	if strings.TrimSpace(id) == "" {
		return model.AccountGroup{}, false, fmt.Errorf("store: get account group: %w: id is required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	var group model.AccountGroup
	err := s.db.QueryRowContext(ctx, `SELECT id, name, COALESCE(color, ''), COALESCE(sort, 0),
		COALESCE(created_at, 0),
		(SELECT COUNT(*) FROM account_meta WHERE group_id = account_groups.id)
		FROM account_groups WHERE id = ?`, id).Scan(
		&group.ID, &group.Name, &group.Color, &group.Sort, &group.CreatedAt, &group.AccountCount)
	if err == sql.ErrNoRows {
		return model.AccountGroup{}, false, nil
	}
	if err != nil {
		return model.AccountGroup{}, false, fmt.Errorf("store: get account group: %w", err)
	}
	return group, true, nil
}

// ListAccountGroups returns every group ordered by sort, then name, with its
// current member count.
func (s *Store) ListAccountGroups() ([]model.AccountGroup, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: list account groups: store is not open")
	}
	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT g.id, g.name, COALESCE(g.color, ''), COALESCE(g.sort, 0),
		COALESCE(g.created_at, 0),
		(SELECT COUNT(*) FROM account_meta m WHERE m.group_id = g.id)
		FROM account_groups g ORDER BY COALESCE(g.sort, 0), g.name, g.id`)
	if err != nil {
		return nil, fmt.Errorf("store: list account groups: %w", err)
	}
	defer rows.Close()

	var out []model.AccountGroup
	for rows.Next() {
		var group model.AccountGroup
		if err := rows.Scan(&group.ID, &group.Name, &group.Color, &group.Sort,
			&group.CreatedAt, &group.AccountCount); err != nil {
			return nil, fmt.Errorf("store: list account groups: scan: %w", err)
		}
		out = append(out, group)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list account groups: iterate: %w", err)
	}
	return out, nil
}

// DeleteAccountGroup removes one group and sets its members back to ungrouped
// in the same transaction. Deleting an unknown id is a no-op.
func (s *Store) DeleteAccountGroup(id string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: delete account group: store is not open")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("store: delete account group: %w: id is required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: delete account group: begin: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE account_meta SET group_id = NULL, updated_at = ?
		WHERE group_id = ?`, nowUnix(), id); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("store: delete account group: ungroup: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM account_groups WHERE id = ?`, id); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("store: delete account group: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: delete account group: commit: %w", err)
	}
	return nil
}

// ReplaceGroupMembers makes items the complete member list of groupID:
// members currently in another group move here, and accounts that are not in
// items stay untouched unless they already belonged to groupID.
func (s *Store) ReplaceGroupMembers(groupID string, items []model.AccountRef) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: replace group members: store is not open")
	}
	groupID = strings.TrimSpace(groupID)
	if groupID == "" {
		return fmt.Errorf("store: replace group members: %w: group id is required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: replace group members: begin: %w", err)
	}
	now := nowUnix()
	if _, err := tx.ExecContext(ctx, `UPDATE account_meta SET group_id = NULL, updated_at = ?
		WHERE group_id = ?`, now, groupID); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("store: replace group members: clear: %w", err)
	}
	for _, item := range items {
		pluginID := strings.TrimSpace(item.PluginID)
		accountID := strings.TrimSpace(item.AccountID)
		if pluginID == "" || accountID == "" {
			_ = tx.Rollback()
			return fmt.Errorf("store: replace group members: %w: plugin_id and account_id are required", ErrInvalidArgument)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO account_meta (plugin_id, account_id, group_id, updated_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(plugin_id, account_id) DO UPDATE SET
				group_id = excluded.group_id,
				updated_at = excluded.updated_at`,
			pluginID, accountID, groupID, now); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: replace group members: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: replace group members: commit: %w", err)
	}
	return nil
}
