package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"aiworkgate/internal/model"
)

// getAccountMetaTx reads one overlay row inside a transaction.
func getAccountMetaTx(ctx context.Context, tx *sql.Tx, pluginID, accountID string) (model.AccountMeta, bool, error) {
	meta := model.AccountMeta{PluginID: pluginID, AccountID: accountID}
	err := scanAccountMeta(tx.QueryRowContext(ctx, `SELECT plugin_id, account_id, `+accountMetaColumns+`
		FROM account_meta WHERE plugin_id = ? AND account_id = ?`, pluginID, accountID).Scan, &meta)
	if err == sql.ErrNoRows {
		return model.AccountMeta{}, false, nil
	}
	if err != nil {
		return model.AccountMeta{}, false, err
	}
	return meta, true, nil
}

// ---------------------------------------------------------------- account_meta

const accountMetaColumns = `COALESCE(alias, ''), COALESCE(group_id, ''),
	enabled_override, priority, weight, COALESCE(note, ''), COALESCE(updated_at, 0),
	COALESCE(status_kind, ''), COALESCE(cooldown_until, 0), COALESCE(cooldown_reason, ''),
	COALESCE(failure_count, 0)`

// scanAccountMeta reads one overlay row in accountMetaColumns order. The last
// four columns are the check-in health tuple; the overlay writers never touch
// them and store/account_health.go owns their updates.
func scanAccountMeta(scan func(dest ...any) error, meta *model.AccountMeta) error {
	var (
		enabled  sql.NullInt64
		prio     sql.NullInt64
		weight   sql.NullFloat64
		failures sql.NullInt64
	)
	if err := scan(&meta.PluginID, &meta.AccountID, &meta.Alias, &meta.GroupID,
		&enabled, &prio, &weight, &meta.Note, &meta.UpdatedAt,
		&meta.StatusKind, &meta.CooldownUntil, &meta.CooldownReason, &failures); err != nil {
		return err
	}
	if failures.Valid {
		meta.FailureCount = int(failures.Int64)
	}
	if enabled.Valid {
		v := enabled.Int64 != 0
		meta.EnabledOverride = &v
	}
	if prio.Valid {
		v := int(prio.Int64)
		meta.Priority = &v
	}
	if weight.Valid {
		v := weight.Float64
		meta.Weight = &v
	}
	return nil
}

// UpsertAccountMeta writes the whole overlay, including explicit NULLs, so a
// caller can clear enabled_override/priority/weight by passing nil pointers.
func (s *Store) UpsertAccountMeta(meta model.AccountMeta) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: upsert account meta: store is not open")
	}
	if meta.PluginID == "" || meta.AccountID == "" {
		return fmt.Errorf("store: upsert account meta: %w: plugin_id and account_id are required", ErrInvalidArgument)
	}
	updatedAt := meta.UpdatedAt
	if updatedAt == 0 {
		updatedAt = nowUnix()
	}
	ctx, cancel := s.ctx()
	defer cancel()
	_, err := s.db.ExecContext(ctx, `INSERT INTO account_meta
		(plugin_id, account_id, alias, group_id, enabled_override, priority, weight, note, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(plugin_id, account_id) DO UPDATE SET
			alias = excluded.alias,
			group_id = excluded.group_id,
			enabled_override = excluded.enabled_override,
			priority = excluded.priority,
			weight = excluded.weight,
			note = excluded.note,
			updated_at = excluded.updated_at`,
		meta.PluginID, meta.AccountID, nullStr(meta.Alias), nullStr(meta.GroupID),
		optBool(meta.EnabledOverride), optInt(meta.Priority), optFloat(meta.Weight),
		nullStr(meta.Note), updatedAt)
	if err != nil {
		return fmt.Errorf("store: upsert account meta: %w", err)
	}
	return nil
}

// GetAccountMeta reads one overlay row.
func (s *Store) GetAccountMeta(pluginID, accountID string) (model.AccountMeta, bool, error) {
	if s == nil || s.db == nil {
		return model.AccountMeta{}, false, fmt.Errorf("store: get account meta: store is not open")
	}
	if pluginID == "" || accountID == "" {
		return model.AccountMeta{}, false, fmt.Errorf("store: get account meta: %w: plugin_id and account_id are required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	meta := model.AccountMeta{PluginID: pluginID, AccountID: accountID}
	err := scanAccountMeta(s.db.QueryRowContext(ctx, `SELECT plugin_id, account_id, `+accountMetaColumns+`
		FROM account_meta WHERE plugin_id = ? AND account_id = ?`, pluginID, accountID).Scan, &meta)
	if err == sql.ErrNoRows {
		return model.AccountMeta{}, false, nil
	}
	if err != nil {
		return model.AccountMeta{}, false, fmt.Errorf("store: get account meta: %w", err)
	}
	return meta, true, nil
}

// ListAccountMeta returns every overlay row ordered by plugin and account.
func (s *Store) ListAccountMeta() ([]model.AccountMeta, error) {
	return s.listAccountMeta("", "")
}

// ListAccountMetaForPlugin returns the overlay rows of one plugin.
func (s *Store) ListAccountMetaForPlugin(pluginID string) ([]model.AccountMeta, error) {
	if strings.TrimSpace(pluginID) == "" {
		return nil, fmt.Errorf("store: list account meta: %w: plugin_id is required", ErrInvalidArgument)
	}
	return s.listAccountMeta(" WHERE plugin_id = ?", pluginID)
}

func (s *Store) listAccountMeta(where string, args ...any) ([]model.AccountMeta, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: list account meta: store is not open")
	}
	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT plugin_id, account_id, `+accountMetaColumns+`
		FROM account_meta`+where+` ORDER BY plugin_id, account_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list account meta: %w", err)
	}
	defer rows.Close()

	var out []model.AccountMeta
	for rows.Next() {
		var meta model.AccountMeta
		if err := scanAccountMeta(rows.Scan, &meta); err != nil {
			return nil, fmt.Errorf("store: list account meta: scan: %w", err)
		}
		out = append(out, meta)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list account meta: iterate: %w", err)
	}
	return out, nil
}

// SetAccountAlias writes (or clears, with an empty alias) one account alias
// while leaving every other overlay field alone.
func (s *Store) SetAccountAlias(pluginID, accountID, alias string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: set account alias: store is not open")
	}
	if pluginID == "" || accountID == "" {
		return fmt.Errorf("store: set account alias: %w: plugin_id and account_id are required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	_, err := s.db.ExecContext(ctx, `INSERT INTO account_meta (plugin_id, account_id, alias, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(plugin_id, account_id) DO UPDATE SET
			alias = excluded.alias,
			updated_at = excluded.updated_at`,
		pluginID, accountID, nullStr(strings.TrimSpace(alias)), nowUnix())
	if err != nil {
		return fmt.Errorf("store: set account alias: %w", err)
	}
	return nil
}

// SetAccountGroup moves one account into groupID; an empty groupID ungroups it.
func (s *Store) SetAccountGroup(pluginID, accountID, groupID string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: set account group: store is not open")
	}
	if pluginID == "" || accountID == "" {
		return fmt.Errorf("store: set account group: %w: plugin_id and account_id are required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	_, err := s.db.ExecContext(ctx, `INSERT INTO account_meta (plugin_id, account_id, group_id, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(plugin_id, account_id) DO UPDATE SET
			group_id = excluded.group_id,
			updated_at = excluded.updated_at`,
		pluginID, accountID, nullStr(strings.TrimSpace(groupID)), nowUnix())
	if err != nil {
		return fmt.Errorf("store: set account group: %w", err)
	}
	return nil
}

// SetAccountsGroup moves several accounts into one group in a single
// transaction. An empty groupID ungroups them.
func (s *Store) SetAccountsGroup(items []model.AccountRef, groupID string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: set accounts group: store is not open")
	}
	ctx, cancel := s.ctx()
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: set accounts group: begin: %w", err)
	}
	now := nowUnix()
	for _, item := range items {
		if strings.TrimSpace(item.PluginID) == "" || strings.TrimSpace(item.AccountID) == "" {
			_ = tx.Rollback()
			return fmt.Errorf("store: set accounts group: %w: plugin_id and account_id are required", ErrInvalidArgument)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO account_meta (plugin_id, account_id, group_id, updated_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(plugin_id, account_id) DO UPDATE SET
				group_id = excluded.group_id,
				updated_at = excluded.updated_at`,
			item.PluginID, item.AccountID, nullStr(strings.TrimSpace(groupID)), now); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: set accounts group: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: set accounts group: commit: %w", err)
	}
	return nil
}

// DeleteAccountMeta removes one overlay row.
func (s *Store) DeleteAccountMeta(pluginID, accountID string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: delete account meta: store is not open")
	}
	ctx, cancel := s.ctx()
	defer cancel()
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM account_meta WHERE plugin_id = ? AND account_id = ?`, pluginID, accountID); err != nil {
		return fmt.Errorf("store: delete account meta: %w", err)
	}
	return nil
}

// DeleteAccountData removes every host-side trace of one account: overlay,
// cached plugin snapshot, pool membership, egress assignment and sticky
// bindings. It runs in one transaction so a plugin account deletion never
// leaves a dangling row behind.
func (s *Store) DeleteAccountData(pluginID, accountID string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: delete account data: store is not open")
	}
	if pluginID == "" || accountID == "" {
		return fmt.Errorf("store: delete account data: %w: plugin_id and account_id are required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: delete account data: begin: %w", err)
	}
	stmts := []string{
		`DELETE FROM account_meta WHERE plugin_id = ? AND account_id = ?`,
		`DELETE FROM account_cache WHERE plugin_id = ? AND account_id = ?`,
		`DELETE FROM pool_members WHERE plugin_id = ? AND account_id = ?`,
		`DELETE FROM account_egress WHERE plugin_id = ? AND account_id = ?`,
		`DELETE FROM sticky_bindings WHERE plugin_id = ? AND account_id = ?`,
	}
	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt, pluginID, accountID); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: delete account data: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: delete account data: commit: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------- account_cache

// ReplaceAccountCache stores the plugin-reported snapshot of one plugin. The
// write replaces the whole plugin slice so accounts that disappeared from the
// plugin answer are removed from the cache too.
func (s *Store) ReplaceAccountCache(pluginID string, accounts []model.Account) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: replace account cache: store is not open")
	}
	if strings.TrimSpace(pluginID) == "" {
		return fmt.Errorf("store: replace account cache: %w: plugin_id is required", ErrInvalidArgument)
	}
	now := nowUnix()
	ctx, cancel := s.ctx()
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: replace account cache: begin: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM account_cache WHERE plugin_id = ?`, pluginID); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("store: replace account cache: clear: %w", err)
	}
	for _, acc := range accounts {
		if strings.TrimSpace(acc.ID) == "" {
			continue
		}
		payload, err := json.Marshal(acc)
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: replace account cache: encode %q: %w", acc.ID, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO account_cache (plugin_id, account_id, payload, updated_at)
			VALUES (?, ?, ?, ?)`, pluginID, acc.ID, string(payload), now); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: replace account cache: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: replace account cache: commit: %w", err)
	}
	return nil
}

// UpsertAccountCache stores one account snapshot as raw JSON.
func (s *Store) UpsertAccountCache(pluginID, accountID, payload string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: upsert account cache: store is not open")
	}
	if pluginID == "" || accountID == "" {
		return fmt.Errorf("store: upsert account cache: %w: plugin_id and account_id are required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	_, err := s.db.ExecContext(ctx, `INSERT INTO account_cache (plugin_id, account_id, payload, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(plugin_id, account_id) DO UPDATE SET
			payload = excluded.payload,
			updated_at = excluded.updated_at`,
		pluginID, accountID, payload, nowUnix())
	if err != nil {
		return fmt.Errorf("store: upsert account cache: %w", err)
	}
	return nil
}

// ListAccountCache returns the cached accounts of one plugin plus the newest
// updated_at. Rows whose payload is not decodable are skipped.
func (s *Store) ListAccountCache(pluginID string) ([]model.Account, int64, error) {
	if s == nil || s.db == nil {
		return nil, 0, fmt.Errorf("store: list account cache: store is not open")
	}
	if strings.TrimSpace(pluginID) == "" {
		return nil, 0, fmt.Errorf("store: list account cache: %w: plugin_id is required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT payload, COALESCE(updated_at, 0)
		FROM account_cache WHERE plugin_id = ? ORDER BY account_id`, pluginID)
	if err != nil {
		return nil, 0, fmt.Errorf("store: list account cache: %w", err)
	}
	defer rows.Close()

	var (
		out       []model.Account
		updatedAt int64
	)
	for rows.Next() {
		var (
			payload string
			at      int64
		)
		if err := rows.Scan(&payload, &at); err != nil {
			return nil, 0, fmt.Errorf("store: list account cache: scan: %w", err)
		}
		var acc model.Account
		if err := json.Unmarshal([]byte(payload), &acc); err != nil {
			continue
		}
		if at > updatedAt {
			updatedAt = at
		}
		out = append(out, acc)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: list account cache: iterate: %w", err)
	}
	return out, updatedAt, nil
}

// DeleteAccountCache removes one cached account snapshot.
func (s *Store) DeleteAccountCache(pluginID, accountID string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: delete account cache: store is not open")
	}
	ctx, cancel := s.ctx()
	defer cancel()
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM account_cache WHERE plugin_id = ? AND account_id = ?`, pluginID, accountID); err != nil {
		return fmt.Errorf("store: delete account cache: %w", err)
	}
	return nil
}

// DeletePluginAccountCache drops the cached snapshots of one plugin (uninstall).
func (s *Store) DeletePluginAccountCache(pluginID string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: delete plugin account cache: store is not open")
	}
	ctx, cancel := s.ctx()
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM account_cache WHERE plugin_id = ?`, pluginID); err != nil {
		return fmt.Errorf("store: delete plugin account cache: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------- save draft

// SaveAccountDraft applies POST /api/accounts/save in one transaction: deleted
// groups are ungrouped and removed, groups are created or updated, account
// overlay fields and egress assignments are written, and pool memberships are
// toggled. Any error rolls the whole draft back; group references are checked
// against the union of stored groups and the draft itself.
func (s *Store) SaveAccountDraft(draft model.AccountDraft) (model.SaveApplied, error) {
	var applied model.SaveApplied
	if s == nil || s.db == nil {
		return applied, fmt.Errorf("store: save account draft: store is not open")
	}
	ctx, cancel := s.ctx()
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return applied, fmt.Errorf("store: save account draft: begin: %w", err)
	}
	fail := func(format string, args ...any) (model.SaveApplied, error) {
		_ = tx.Rollback()
		return model.SaveApplied{}, fmt.Errorf("store: save account draft: "+format, args...)
	}

	// 1. Deleted groups: members become ungrouped, then the row goes away.
	for _, id := range draft.DeletedGroups {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE account_meta SET group_id = NULL, updated_at = ?
			WHERE group_id = ?`, nowUnix(), id); err != nil {
			return fail("ungroup %q: %w", id, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM account_groups WHERE id = ?`, id); err != nil {
			return fail("delete group %q: %w", id, err)
		}
	}

	// 2. Existing group ids (after the deletions).
	existing := make(map[string]bool)
	rows, err := tx.QueryContext(ctx, `SELECT id FROM account_groups`)
	if err != nil {
		return fail("list groups: %w", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fail("list groups: %w", err)
		}
		existing[id] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fail("list groups: %w", err)
	}
	rows.Close()

	now := nowUnix()
	for _, group := range draft.Groups {
		id := strings.TrimSpace(group.ID)
		name := strings.TrimSpace(group.Name)
		if id == "" {
			return fail("group name %q has no id", name)
		}
		if name == "" {
			return fail("group %q has an empty name", id)
		}
		sortValue := 0
		if group.Sort != nil {
			sortValue = *group.Sort
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO account_groups (id, name, color, sort, created_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				name = excluded.name,
				color = excluded.color,
				sort = excluded.sort`,
			id, name, nullStr(strings.TrimSpace(group.Color)), sortValue, now); err != nil {
			return fail("save group %q: %w", id, err)
		}
		existing[id] = true
		applied.Groups++
	}

	// 3. Account overlay and egress. Absent draft fields keep the stored value;
	//    explicit nulls clear it.
	for _, item := range draft.Accounts {
		pluginID := strings.TrimSpace(item.PluginID)
		accountID := strings.TrimSpace(item.AccountID)
		if pluginID == "" || accountID == "" {
			return fail("account rows need plugin_id and account_id")
		}
		meta, exists, err := getAccountMetaTx(ctx, tx, pluginID, accountID)
		if err != nil {
			return fail("read %s/%s: %w", pluginID, accountID, err)
		}
		if !exists {
			meta = model.AccountMeta{PluginID: pluginID, AccountID: accountID}
		}
		if item.AliasSet() {
			if item.Alias != nil {
				meta.Alias = strings.TrimSpace(*item.Alias)
			} else {
				meta.Alias = ""
			}
		}
		if item.GroupIDSet() {
			groupID := ""
			if item.GroupID != nil {
				groupID = strings.TrimSpace(*item.GroupID)
			}
			if groupID != "" && !existing[groupID] {
				return fail("unknown group %q for %s/%s", groupID, pluginID, accountID)
			}
			meta.GroupID = groupID
		}
		if item.PrioritySet() {
			meta.Priority = item.Priority
		}
		if item.WeightSet() {
			meta.Weight = item.Weight
		}
		if item.EnabledOverrideSet() {
			meta.EnabledOverride = item.EnabledOverride
		}
		meta.PluginID, meta.AccountID = pluginID, accountID
		if _, err := tx.ExecContext(ctx, `INSERT INTO account_meta
			(plugin_id, account_id, alias, group_id, enabled_override, priority, weight, note, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(plugin_id, account_id) DO UPDATE SET
				alias = excluded.alias,
				group_id = excluded.group_id,
				enabled_override = excluded.enabled_override,
				priority = excluded.priority,
				weight = excluded.weight,
				note = excluded.note,
				updated_at = excluded.updated_at`,
			pluginID, accountID, nullStr(meta.Alias), nullStr(meta.GroupID),
			optBool(meta.EnabledOverride), optInt(meta.Priority), optFloat(meta.Weight),
			nullStr(meta.Note), now); err != nil {
			return fail("save %s/%s: %w", pluginID, accountID, err)
		}
		if item.EgressSet() {
			if item.Egress == nil {
				if _, err := tx.ExecContext(ctx,
					`DELETE FROM account_egress WHERE plugin_id = ? AND account_id = ?`, pluginID, accountID); err != nil {
					return fail("clear egress %s/%s: %w", pluginID, accountID, err)
				}
			} else {
				mode := strings.ToLower(strings.TrimSpace(item.Egress.Mode))
				switch mode {
				case "", "default":
					if _, err := tx.ExecContext(ctx,
						`DELETE FROM account_egress WHERE plugin_id = ? AND account_id = ?`, pluginID, accountID); err != nil {
						return fail("clear egress %s/%s: %w", pluginID, accountID, err)
					}
				case model.EgressDirect:
					if _, err := tx.ExecContext(ctx, `INSERT INTO account_egress (plugin_id, account_id, mode, proxy_id)
						VALUES (?, ?, ?, NULL)
						ON CONFLICT(plugin_id, account_id) DO UPDATE SET mode = excluded.mode, proxy_id = NULL`,
						pluginID, accountID, model.EgressDirect); err != nil {
						return fail("save egress %s/%s: %w", pluginID, accountID, err)
					}
				case model.EgressProxy:
					proxyID := strings.TrimSpace(item.Egress.ProxyID)
					if proxyID == "" {
						return fail("proxy_id is required for egress mode proxy (%s/%s)", pluginID, accountID)
					}
					if _, err := tx.ExecContext(ctx, `INSERT INTO account_egress (plugin_id, account_id, mode, proxy_id)
						VALUES (?, ?, ?, ?)
						ON CONFLICT(plugin_id, account_id) DO UPDATE SET
							mode = excluded.mode, proxy_id = excluded.proxy_id`,
						pluginID, accountID, model.EgressProxy, proxyID); err != nil {
						return fail("save egress %s/%s: %w", pluginID, accountID, err)
					}
				default:
					return fail("unsupported egress mode %q (%s/%s)", mode, pluginID, accountID)
				}
			}
		}
		applied.Accounts++
	}

	// 4. Pool memberships.
	for _, membership := range draft.Memberships {
		pluginID := strings.TrimSpace(membership.PluginID)
		accountID := strings.TrimSpace(membership.AccountID)
		if pluginID == "" || accountID == "" {
			return fail("memberships need plugin_id and account_id")
		}
		if membership.Member {
			if _, err := tx.ExecContext(ctx, `INSERT INTO pool_members (plugin_id, account_id, enabled, updated_at)
				VALUES (?, ?, 1, ?)
				ON CONFLICT(plugin_id, account_id) DO UPDATE SET enabled = 1, updated_at = excluded.updated_at`,
				pluginID, accountID, now); err != nil {
				return fail("add member %s/%s: %w", pluginID, accountID, err)
			}
		} else {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM pool_members WHERE plugin_id = ? AND account_id = ?`, pluginID, accountID); err != nil {
				return fail("remove member %s/%s: %w", pluginID, accountID, err)
			}
		}
		applied.Memberships++
	}

	if err := tx.Commit(); err != nil {
		return model.SaveApplied{}, fmt.Errorf("store: save account draft: commit: %w", err)
	}
	return applied, nil
}

// countRows is a test helper that counts rows of a table.
func (s *Store) countRows(table string) (int, error) {
	ctx, cancel := s.ctx()
	defer cancel()
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}
