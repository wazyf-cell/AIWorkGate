package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"aiworkgate/internal/model"
)

// ---------------------------------------------------------------- pool_members

// UpsertPoolMember marks one account as an explicit pool member.
func (s *Store) UpsertPoolMember(member model.PoolMember) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: upsert pool member: store is not open")
	}
	if member.PluginID == "" || member.AccountID == "" {
		return fmt.Errorf("store: upsert pool member: %w: plugin_id and account_id are required", ErrInvalidArgument)
	}
	updatedAt := member.UpdatedAt
	if updatedAt == 0 {
		updatedAt = nowUnix()
	}
	ctx, cancel := s.ctx()
	defer cancel()
	_, err := s.db.ExecContext(ctx, `INSERT INTO pool_members (plugin_id, account_id, enabled, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(plugin_id, account_id) DO UPDATE SET
			enabled = excluded.enabled,
			updated_at = excluded.updated_at`,
		member.PluginID, member.AccountID, boolInt(member.Enabled), updatedAt)
	if err != nil {
		return fmt.Errorf("store: upsert pool member: %w", err)
	}
	return nil
}

// ListPoolMembers returns the explicit members of one plugin ordered by
// account id. An empty slice means "no explicit membership rows".
func (s *Store) ListPoolMembers(pluginID string) ([]model.PoolMember, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: list pool members: store is not open")
	}
	if strings.TrimSpace(pluginID) == "" {
		return nil, fmt.Errorf("store: list pool members: %w: plugin_id is required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT plugin_id, account_id, COALESCE(enabled, 1), COALESCE(updated_at, 0)
		FROM pool_members WHERE plugin_id = ? ORDER BY account_id`, pluginID)
	if err != nil {
		return nil, fmt.Errorf("store: list pool members: %w", err)
	}
	defer rows.Close()

	var out []model.PoolMember
	for rows.Next() {
		var (
			member  model.PoolMember
			enabled int64
		)
		if err := rows.Scan(&member.PluginID, &member.AccountID, &enabled, &member.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: list pool members: scan: %w", err)
		}
		member.Enabled = enabled != 0
		out = append(out, member)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list pool members: iterate: %w", err)
	}
	return out, nil
}

// DeletePoolMember removes one explicit membership row.
func (s *Store) DeletePoolMember(pluginID, accountID string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: delete pool member: store is not open")
	}
	ctx, cancel := s.ctx()
	defer cancel()
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM pool_members WHERE plugin_id = ? AND account_id = ?`, pluginID, accountID); err != nil {
		return fmt.Errorf("store: delete pool member: %w", err)
	}
	return nil
}

// ReplacePoolMembers makes items the complete explicit membership list of one
// plugin. An empty list clears it (every credential account participates).
func (s *Store) ReplacePoolMembers(pluginID string, items []model.AccountRef) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: replace pool members: store is not open")
	}
	if strings.TrimSpace(pluginID) == "" {
		return fmt.Errorf("store: replace pool members: %w: plugin_id is required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: replace pool members: begin: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM pool_members WHERE plugin_id = ?`, pluginID); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("store: replace pool members: clear: %w", err)
	}
	now := nowUnix()
	for _, item := range items {
		accountID := strings.TrimSpace(item.AccountID)
		if accountID == "" {
			_ = tx.Rollback()
			return fmt.Errorf("store: replace pool members: %w: account_id is required", ErrInvalidArgument)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO pool_members (plugin_id, account_id, enabled, updated_at)
			VALUES (?, ?, 1, ?)`, pluginID, accountID, now); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: replace pool members: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: replace pool members: commit: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------- pool_settings

const poolSettingsColumns = `COALESCE(upstream_enabled, 0), COALESCE(strategy, ''),
	COALESCE(pool_priority, 0), COALESCE(account_concurrency, 0),
	COALESCE(pool_sticky_ttl, 0), COALESCE(conv_sticky_ttl, 0), COALESCE(hedge_ms, 0),
	COALESCE(default_reasoning, ''), COALESCE(background_downgrade, 0),
	COALESCE(longctx_downgrade, 0), COALESCE(tool_exec, 0),
	COALESCE(model_sync_time, ''), COALESCE(updated_at, 0)`

func scanPoolSettings(scan func(dest ...any) error, settings *model.PoolSettings) error {
	var (
		upstream, background, longctx, tool int64
	)
	if err := scan(&settings.PluginID, &upstream, &settings.Strategy,
		&settings.PoolPriority, &settings.AccountConcurrency,
		&settings.PoolStickyTTL, &settings.ConvStickyTTL, &settings.HedgeMS,
		&settings.DefaultReasoning, &background, &longctx, &tool,
		&settings.ModelSyncTime, &settings.UpdatedAt); err != nil {
		return err
	}
	settings.UpstreamEnabled = upstream != 0
	settings.BackgroundDowngrade = background != 0
	settings.LongctxDowngrade = longctx != 0
	settings.ToolExec = tool != 0
	return nil
}

// UpsertPoolSettings writes one plugin's scheduling parameters.
func (s *Store) UpsertPoolSettings(settings model.PoolSettings) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: upsert pool settings: store is not open")
	}
	pluginID := strings.TrimSpace(settings.PluginID)
	if pluginID == "" {
		return fmt.Errorf("store: upsert pool settings: %w: plugin_id is required", ErrInvalidArgument)
	}
	updatedAt := settings.UpdatedAt
	if updatedAt == 0 {
		updatedAt = nowUnix()
	}
	ctx, cancel := s.ctx()
	defer cancel()
	_, err := s.db.ExecContext(ctx, `INSERT INTO pool_settings
		(plugin_id, upstream_enabled, strategy, pool_priority, account_concurrency,
		 pool_sticky_ttl, conv_sticky_ttl, hedge_ms, default_reasoning,
		 background_downgrade, longctx_downgrade, tool_exec, model_sync_time, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(plugin_id) DO UPDATE SET
			upstream_enabled = excluded.upstream_enabled,
			strategy = excluded.strategy,
			pool_priority = excluded.pool_priority,
			account_concurrency = excluded.account_concurrency,
			pool_sticky_ttl = excluded.pool_sticky_ttl,
			conv_sticky_ttl = excluded.conv_sticky_ttl,
			hedge_ms = excluded.hedge_ms,
			default_reasoning = excluded.default_reasoning,
			background_downgrade = excluded.background_downgrade,
			longctx_downgrade = excluded.longctx_downgrade,
			tool_exec = excluded.tool_exec,
			model_sync_time = excluded.model_sync_time,
			updated_at = excluded.updated_at`,
		pluginID, boolInt(settings.UpstreamEnabled), nullStr(strings.TrimSpace(settings.Strategy)),
		settings.PoolPriority, settings.AccountConcurrency, settings.PoolStickyTTL,
		settings.ConvStickyTTL, settings.HedgeMS, nullStr(strings.TrimSpace(settings.DefaultReasoning)),
		boolInt(settings.BackgroundDowngrade), boolInt(settings.LongctxDowngrade),
		boolInt(settings.ToolExec), nullStr(strings.TrimSpace(settings.ModelSyncTime)), updatedAt)
	if err != nil {
		return fmt.Errorf("store: upsert pool settings: %w", err)
	}
	return nil
}

// UpsertPoolSettingsBatch writes several pool rows (including the "__global__"
// defaults row) in one transaction. The scheduler presets use it so readers
// never observe a half-applied preset.
func (s *Store) UpsertPoolSettingsBatch(rows []model.PoolSettings) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: upsert pool settings batch: store is not open")
	}
	ctx, cancel := s.ctx()
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: upsert pool settings batch: begin: %w", err)
	}
	for _, settings := range rows {
		pluginID := strings.TrimSpace(settings.PluginID)
		if pluginID == "" {
			_ = tx.Rollback()
			return fmt.Errorf("store: upsert pool settings batch: %w: plugin_id is required", ErrInvalidArgument)
		}
		updatedAt := settings.UpdatedAt
		if updatedAt == 0 {
			updatedAt = nowUnix()
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO pool_settings
			(plugin_id, upstream_enabled, strategy, pool_priority, account_concurrency,
			 pool_sticky_ttl, conv_sticky_ttl, hedge_ms, default_reasoning,
			 background_downgrade, longctx_downgrade, tool_exec, model_sync_time, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(plugin_id) DO UPDATE SET
				upstream_enabled = excluded.upstream_enabled,
				strategy = excluded.strategy,
				pool_priority = excluded.pool_priority,
				account_concurrency = excluded.account_concurrency,
				pool_sticky_ttl = excluded.pool_sticky_ttl,
				conv_sticky_ttl = excluded.conv_sticky_ttl,
				hedge_ms = excluded.hedge_ms,
				default_reasoning = excluded.default_reasoning,
				background_downgrade = excluded.background_downgrade,
				longctx_downgrade = excluded.longctx_downgrade,
				tool_exec = excluded.tool_exec,
				model_sync_time = excluded.model_sync_time,
				updated_at = excluded.updated_at`,
			pluginID, boolInt(settings.UpstreamEnabled), nullStr(strings.TrimSpace(settings.Strategy)),
			settings.PoolPriority, settings.AccountConcurrency, settings.PoolStickyTTL,
			settings.ConvStickyTTL, settings.HedgeMS, nullStr(strings.TrimSpace(settings.DefaultReasoning)),
			boolInt(settings.BackgroundDowngrade), boolInt(settings.LongctxDowngrade),
			boolInt(settings.ToolExec), nullStr(strings.TrimSpace(settings.ModelSyncTime)), updatedAt); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: upsert pool settings batch %q: %w", pluginID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: upsert pool settings batch: commit: %w", err)
	}
	return nil
}

// GetPoolSettings reads one plugin's scheduling parameters.
func (s *Store) GetPoolSettings(pluginID string) (model.PoolSettings, bool, error) {
	if s == nil || s.db == nil {
		return model.PoolSettings{}, false, fmt.Errorf("store: get pool settings: store is not open")
	}
	if strings.TrimSpace(pluginID) == "" {
		return model.PoolSettings{}, false, fmt.Errorf("store: get pool settings: %w: plugin_id is required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	settings := model.PoolSettings{PluginID: pluginID}
	err := scanPoolSettings(s.db.QueryRowContext(ctx, `SELECT plugin_id, `+poolSettingsColumns+`
		FROM pool_settings WHERE plugin_id = ?`, pluginID).Scan, &settings)
	if err == sql.ErrNoRows {
		return model.PoolSettings{}, false, nil
	}
	if err != nil {
		return model.PoolSettings{}, false, fmt.Errorf("store: get pool settings: %w", err)
	}
	return settings, true, nil
}

// ListPoolSettings returns every stored pool row ordered by plugin id.
func (s *Store) ListPoolSettings() ([]model.PoolSettings, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: list pool settings: store is not open")
	}
	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT plugin_id, `+poolSettingsColumns+`
		FROM pool_settings ORDER BY plugin_id`)
	if err != nil {
		return nil, fmt.Errorf("store: list pool settings: %w", err)
	}
	defer rows.Close()
	var out []model.PoolSettings
	for rows.Next() {
		var settings model.PoolSettings
		if err := scanPoolSettings(rows.Scan, &settings); err != nil {
			return nil, fmt.Errorf("store: list pool settings: scan: %w", err)
		}
		out = append(out, settings)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list pool settings: iterate: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------- model bindings

// ReplaceModelBinding writes a logical model and its ordered sources in one
// transaction. An empty item list leaves the binding without candidates.
func (s *Store) ReplaceModelBinding(binding model.ModelBinding) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: replace model binding: store is not open")
	}
	logical := strings.TrimSpace(binding.LogicalModel)
	if logical == "" {
		return fmt.Errorf("store: replace model binding: %w: logical_model is required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: replace model binding: begin: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO model_bindings (logical_model, display_name, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(logical_model) DO UPDATE SET
			display_name = excluded.display_name,
			updated_at = excluded.updated_at`,
		logical, nullStr(strings.TrimSpace(binding.DisplayName)), nowUnix()); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("store: replace model binding: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM model_binding_items WHERE logical_model = ?`, logical); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("store: replace model binding: clear items: %w", err)
	}
	for i, item := range binding.Items {
		pluginID := strings.TrimSpace(item.SourcePlugin)
		sourceModel := strings.TrimSpace(item.SourceModel)
		if pluginID == "" || sourceModel == "" {
			_ = tx.Rollback()
			return fmt.Errorf("store: replace model binding: %w: source_plugin and source_model are required", ErrInvalidArgument)
		}
		position := item.Position
		if position < 0 {
			position = i
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO model_binding_items
			(logical_model, position, source_plugin, source_model) VALUES (?, ?, ?, ?)`,
			logical, position, pluginID, sourceModel); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: replace model binding: item %d: %w", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: replace model binding: commit: %w", err)
	}
	return nil
}

// GetModelBinding reads one logical model and its candidates.
func (s *Store) GetModelBinding(logical string) (model.ModelBinding, bool, error) {
	if s == nil || s.db == nil {
		return model.ModelBinding{}, false, fmt.Errorf("store: get model binding: store is not open")
	}
	logical = strings.TrimSpace(logical)
	if logical == "" {
		return model.ModelBinding{}, false, fmt.Errorf("store: get model binding: %w: logical_model is required", ErrInvalidArgument)
	}
	bindings, err := s.listModelBindings(" WHERE logical_model = ?", logical)
	if err != nil {
		return model.ModelBinding{}, false, err
	}
	if len(bindings) == 0 {
		return model.ModelBinding{}, false, nil
	}
	return bindings[0], true, nil
}

// ListModelBindings returns every logical model with its candidates.
func (s *Store) ListModelBindings() ([]model.ModelBinding, error) {
	return s.listModelBindings("")
}

func (s *Store) listModelBindings(where string, args ...any) ([]model.ModelBinding, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: list model bindings: store is not open")
	}
	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT logical_model, COALESCE(display_name, ''), COALESCE(updated_at, 0)
		FROM model_bindings`+where+` ORDER BY logical_model`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list model bindings: %w", err)
	}
	defer rows.Close()

	var out []model.ModelBinding
	index := make(map[string]int)
	for rows.Next() {
		var binding model.ModelBinding
		if err := rows.Scan(&binding.LogicalModel, &binding.DisplayName, &binding.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: list model bindings: scan: %w", err)
		}
		index[binding.LogicalModel] = len(out)
		out = append(out, binding)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list model bindings: iterate: %w", err)
	}
	if len(out) == 0 {
		return out, nil
	}

	itemRows, err := s.db.QueryContext(ctx, `SELECT logical_model, position, source_plugin, source_model
		FROM model_binding_items ORDER BY logical_model, position`)
	if err != nil {
		return nil, fmt.Errorf("store: list model bindings: items: %w", err)
	}
	defer itemRows.Close()
	for itemRows.Next() {
		var item model.ModelBindingItem
		if err := itemRows.Scan(&item.LogicalModel, &item.Position, &item.SourcePlugin, &item.SourceModel); err != nil {
			return nil, fmt.Errorf("store: list model bindings: item scan: %w", err)
		}
		if i, ok := index[item.LogicalModel]; ok {
			out[i].Items = append(out[i].Items, item)
		}
	}
	if err := itemRows.Err(); err != nil {
		return nil, fmt.Errorf("store: list model bindings: items iterate: %w", err)
	}
	return out, nil
}

// DeleteModelBinding removes a logical model and its candidates.
func (s *Store) DeleteModelBinding(logical string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: delete model binding: store is not open")
	}
	logical = strings.TrimSpace(logical)
	if logical == "" {
		return fmt.Errorf("store: delete model binding: %w: logical_model is required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: delete model binding: begin: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM model_binding_items WHERE logical_model = ?`, logical); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("store: delete model binding: items: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM model_bindings WHERE logical_model = ?`, logical); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("store: delete model binding: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: delete model binding: commit: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------- model catalog

// ReplaceModelCatalog stores the full catalog of one plugin; entries missing
// from the new slice disappear.
func (s *Store) ReplaceModelCatalog(pluginID string, entries []model.ModelCatalogEntry) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: replace model catalog: store is not open")
	}
	pluginID = strings.TrimSpace(pluginID)
	if pluginID == "" {
		return fmt.Errorf("store: replace model catalog: %w: plugin_id is required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: replace model catalog: begin: %w", err)
	}
	// A sync must not reset the user's "接入" choices: remember the old
	// enabled flags and reapply them to models that still exist.
	enabled := make(map[string]bool)
	rows, err := tx.QueryContext(ctx,
		`SELECT model_id, COALESCE(enabled, 1) FROM model_catalog WHERE plugin_id = ?`, pluginID)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("store: replace model catalog: read enabled flags: %w", err)
	}
	for rows.Next() {
		var id string
		var on int64
		if err := rows.Scan(&id, &on); err != nil {
			_ = rows.Close()
			_ = tx.Rollback()
			return fmt.Errorf("store: replace model catalog: scan enabled flag: %w", err)
		}
		enabled[id] = on != 0
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		_ = tx.Rollback()
		return fmt.Errorf("store: replace model catalog: iterate enabled flags: %w", err)
	}
	_ = rows.Close()
	if _, err := tx.ExecContext(ctx, `DELETE FROM model_catalog WHERE plugin_id = ?`, pluginID); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("store: replace model catalog: clear: %w", err)
	}
	for _, entry := range entries {
		modelID := strings.TrimSpace(entry.ModelID)
		if modelID == "" {
			_ = tx.Rollback()
			return fmt.Errorf("store: replace model catalog: %w: model_id is required", ErrInvalidArgument)
		}
		thinking := ""
		if len(entry.Thinking) > 0 {
			data, err := json.Marshal(entry.Thinking)
			if err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("store: replace model catalog: encode thinking: %w", err)
			}
			thinking = string(data)
		}
		syncedAt := entry.SyncedAt
		if syncedAt == 0 {
			syncedAt = nowUnix()
		}
		exposed := true
		if previous, ok := enabled[modelID]; ok {
			exposed = previous
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO model_catalog
			(plugin_id, model_id, display_name, vendor, multiplier, thinking,
			 context_length, vision, free, enabled, synced_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			pluginID, modelID, nullStr(strings.TrimSpace(entry.DisplayName)),
			nullStr(strings.TrimSpace(entry.Vendor)), entry.Multiplier, nullStr(thinking),
			entry.ContextLength, boolInt(entry.Vision), boolInt(entry.Free), boolInt(exposed), syncedAt); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: replace model catalog: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: replace model catalog: commit: %w", err)
	}
	return nil
}

// ListModelCatalog returns one plugin's catalog ordered by model id.
func (s *Store) ListModelCatalog(pluginID string) ([]model.ModelCatalogEntry, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: list model catalog: store is not open")
	}
	if strings.TrimSpace(pluginID) == "" {
		return nil, fmt.Errorf("store: list model catalog: %w: plugin_id is required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT plugin_id, model_id, COALESCE(display_name, ''),
		COALESCE(vendor, ''), COALESCE(multiplier, 0), COALESCE(thinking, ''),
		COALESCE(context_length, 0), COALESCE(vision, 0), COALESCE(free, 0),
		COALESCE(enabled, 1), COALESCE(synced_at, 0)
		FROM model_catalog WHERE plugin_id = ? ORDER BY model_id`, pluginID)
	if err != nil {
		return nil, fmt.Errorf("store: list model catalog: %w", err)
	}
	defer rows.Close()

	var out []model.ModelCatalogEntry
	for rows.Next() {
		var (
			entry                 model.ModelCatalogEntry
			thinking              string
			vision, free, exposed int64
		)
		if err := rows.Scan(&entry.PluginID, &entry.ModelID, &entry.DisplayName, &entry.Vendor,
			&entry.Multiplier, &thinking, &entry.ContextLength, &vision, &free, &exposed, &entry.SyncedAt); err != nil {
			return nil, fmt.Errorf("store: list model catalog: scan: %w", err)
		}
		entry.Vision = vision != 0
		entry.Free = free != 0
		entry.Enabled = exposed != 0
		if thinking != "" {
			_ = json.Unmarshal([]byte(thinking), &entry.Thinking)
		}
		out = append(out, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list model catalog: iterate: %w", err)
	}
	return out, nil
}

// SetModelCatalogEnabled flips one model's "接入" switch. It reports
// ErrNotFound when the catalog has no such row.
func (s *Store) SetModelCatalogEnabled(pluginID, modelID string, enabled bool) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: set model enabled: store is not open")
	}
	pluginID = strings.TrimSpace(pluginID)
	modelID = strings.TrimSpace(modelID)
	if pluginID == "" || modelID == "" {
		return fmt.Errorf("store: set model enabled: %w: plugin_id and model_id are required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	res, err := s.db.ExecContext(ctx,
		`UPDATE model_catalog SET enabled = ? WHERE plugin_id = ? AND model_id = ?`,
		boolInt(enabled), pluginID, modelID)
	if err != nil {
		return fmt.Errorf("store: set model enabled: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: set model enabled: rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("store: set model enabled: %w", ErrNotFound)
	}
	return nil
}

// DeleteModelCatalog removes one plugin's catalog (uninstall).
func (s *Store) DeleteModelCatalog(pluginID string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: delete model catalog: store is not open")
	}
	ctx, cancel := s.ctx()
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM model_catalog WHERE plugin_id = ?`, pluginID); err != nil {
		return fmt.Errorf("store: delete model catalog: %w", err)
	}
	return nil
}
