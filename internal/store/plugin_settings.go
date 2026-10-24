package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// PluginSetting is the host-side, per-plugin configuration row.
//
// Precedence: only concurrency/strict carry a "跟随全局" marker. A row written
// by the settings UI stores NULL for an unset override, so the host/global
// value wins. A row created by an install carries the then-current concrete
// value (concurrency >= 1, strict 0/1, never NULL), which StrictSet reports as
// an explicit override - documented in the UI so an existing install keeps
// behaving until the user deliberately chooses 跟随全局.
type PluginSetting struct {
	PluginID string `json:"plugin_id"`
	Enabled  bool   `json:"enabled"`
	Mode     string `json:"mode"` // always | on_demand
	// Concurrency is the per-plugin override; 0 means "跟随全局".
	Concurrency int    `json:"concurrency"`
	CheckinTime string `json:"checkin_time"` // HH:MM local
	// Strict is the stored override; meaningful only when StrictSet is true.
	Strict bool `json:"strict"`
	// StrictSet reports whether the row carries an explicit strict value
	// (non-NULL). false means the column is NULL and the global value wins.
	StrictSet   bool  `json:"strict_set"`
	InstalledAt int64 `json:"installed_at"`
	UpdatedAt   int64 `json:"updated_at"`
}

// PluginSettingsView is the nullable read model used by the settings API and
// the egress writer, so they can tell an explicit override from 跟随全局.
type PluginSettingsView struct {
	Enabled     bool
	Mode        string
	Concurrency int // 0 = 跟随全局
	CheckinTime string
	Strict      bool // meaningful only when StrictSet is true
	StrictSet   bool // false = column is NULL = 跟随全局
	InstalledAt int64
}

// UpsertPluginSettings stores the editable plugin settings. installed_at is
// kept from the first write when the caller passes 0 (or a value already
// stored), because the check-in calendar is anchored to the install date.
func (s *Store) UpsertPluginSettings(pluginID string, enabled bool, mode string, concurrency int, checkinTime string, strict bool, installedAt int64) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: upsert plugin settings: store is not open")
	}
	if pluginID == "" {
		return fmt.Errorf("store: upsert plugin settings: %w: plugin_id is required", ErrInvalidArgument)
	}
	now := nowUnix()
	ctx, cancel := s.ctx()
	defer cancel()
	_, err := s.db.ExecContext(ctx, `INSERT INTO plugin_settings
		(plugin_id, enabled, mode, concurrency, checkin_time, strict, installed_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, COALESCE(NULLIF(?, 0), CAST(strftime('%s','now') AS INTEGER)), ?)
		ON CONFLICT(plugin_id) DO UPDATE SET
			enabled = excluded.enabled,
			mode = excluded.mode,
			concurrency = excluded.concurrency,
			checkin_time = excluded.checkin_time,
			strict = excluded.strict,
			installed_at = CASE WHEN ? <> 0 THEN ? ELSE plugin_settings.installed_at END,
			updated_at = excluded.updated_at`,
		pluginID, boolInt(enabled), mode, concurrency, checkinTime, boolInt(strict), installedAt, now,
		installedAt, installedAt)
	if err != nil {
		return fmt.Errorf("store: upsert plugin settings: %w", err)
	}
	return nil
}

// SetPluginEnabled flips only the enabled flag. A plugin toggle must not
// rewrite the other columns: the caller only knows the *resolved* settings, so
// writing them back would turn an unset 跟随全局 override into a concrete one.
// seed is used only when no row exists yet, and its zero concurrency/strict
// keep the new row on the global defaults.
func (s *Store) SetPluginEnabled(pluginID string, enabled bool, seed PluginSettingsView) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: set plugin enabled: store is not open")
	}
	if pluginID == "" {
		return fmt.Errorf("store: set plugin enabled: %w: plugin_id is required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	res, err := s.db.ExecContext(ctx, `UPDATE plugin_settings SET enabled = ?, updated_at = ?
		WHERE plugin_id = ?`, boolInt(enabled), nowUnix(), pluginID)
	if err != nil {
		return fmt.Errorf("store: set plugin enabled: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		return nil
	}
	var strict *bool
	if seed.StrictSet {
		value := seed.Strict
		strict = &value
	}
	return s.UpdatePluginSettings(pluginID, enabled, seed.Mode, seed.Concurrency, seed.CheckinTime,
		strict, seed.InstalledAt)
}

// GetPluginSettings reads one plugin's settings. ok is false when the plugin
// has never been configured.
func (s *Store) GetPluginSettings(pluginID string) (enabled bool, mode string, concurrency int, checkinTime string, strict bool, ok bool, err error) {
	if s == nil || s.db == nil {
		return false, "", 0, "", false, false, fmt.Errorf("store: get plugin settings: store is not open")
	}
	ctx, cancel := s.ctx()
	defer cancel()

	var (
		en, st int64
	)
	row := s.db.QueryRowContext(ctx, `SELECT COALESCE(enabled, 0), COALESCE(mode, ''),
		COALESCE(concurrency, 0), COALESCE(checkin_time, ''), COALESCE(strict, 0)
		FROM plugin_settings WHERE plugin_id = ?`, pluginID)
	scanErr := row.Scan(&en, &mode, &concurrency, &checkinTime, &st)
	switch {
	case errors.Is(scanErr, sql.ErrNoRows):
		return false, "", 0, "", false, false, nil
	case scanErr != nil:
		return false, "", 0, "", false, false, fmt.Errorf("store: get plugin settings %q: %w", pluginID, scanErr)
	}
	return en != 0, mode, concurrency, checkinTime, st != 0, true, nil
}

// ListPluginSettings returns every configured plugin ordered by id.
func (s *Store) ListPluginSettings() ([]PluginSetting, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: list plugin settings: store is not open")
	}
	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT plugin_id, COALESCE(enabled, 0), COALESCE(mode, ''),
		COALESCE(concurrency, 0), COALESCE(checkin_time, ''), strict,
		COALESCE(installed_at, 0), COALESCE(updated_at, 0)
		FROM plugin_settings ORDER BY plugin_id`)
	if err != nil {
		return nil, fmt.Errorf("store: list plugin settings: %w", err)
	}
	defer rows.Close()

	var out []PluginSetting
	for rows.Next() {
		var (
			item      PluginSetting
			en        int64
			strictVal sql.NullInt64
		)
		if err := rows.Scan(&item.PluginID, &en, &item.Mode, &item.Concurrency,
			&item.CheckinTime, &strictVal, &item.InstalledAt, &item.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: list plugin settings: scan: %w", err)
		}
		item.Enabled = en != 0
		item.StrictSet = strictVal.Valid
		item.Strict = strictVal.Valid && strictVal.Int64 != 0
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list plugin settings: iterate: %w", err)
	}
	return out, nil
}

// UpdatePluginSettings writes the editable settings through the settings API.
// Unlike UpsertPluginSettings it stores NULL for a concurrency of 0 and for a
// nil strict so the host/global default wins (see the type comment above).
// installed_at is kept from the existing row when the caller passes 0.
func (s *Store) UpdatePluginSettings(pluginID string, enabled bool, mode string, concurrency int, checkinTime string, strict *bool, installedAt int64) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: update plugin settings: store is not open")
	}
	if pluginID == "" {
		return fmt.Errorf("store: update plugin settings: %w: plugin_id is required", ErrInvalidArgument)
	}
	if concurrency < 0 {
		return fmt.Errorf("store: update plugin settings: %w: concurrency must not be negative", ErrInvalidArgument)
	}
	// 0 and a negative-after-clamp value both collapse to NULL = 跟随全局.
	var concurrencyArg any
	if concurrency > 0 {
		concurrencyArg = concurrency
	}
	now := nowUnix()
	ctx, cancel := s.ctx()
	defer cancel()
	_, err := s.db.ExecContext(ctx, `INSERT INTO plugin_settings
		(plugin_id, enabled, mode, concurrency, checkin_time, strict, installed_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, COALESCE(NULLIF(?, 0), CAST(strftime('%s','now') AS INTEGER)), ?)
		ON CONFLICT(plugin_id) DO UPDATE SET
			enabled = excluded.enabled,
			mode = excluded.mode,
			concurrency = excluded.concurrency,
			checkin_time = excluded.checkin_time,
			strict = excluded.strict,
			installed_at = CASE WHEN ? <> 0 THEN ? ELSE plugin_settings.installed_at END,
			updated_at = excluded.updated_at`,
		pluginID, boolInt(enabled), nullStr(mode), concurrencyArg, nullStr(checkinTime), optBool(strict),
		installedAt, now, installedAt, installedAt)
	if err != nil {
		return fmt.Errorf("store: update plugin settings: %w", err)
	}
	return nil
}

// GetPluginSettingsView reads one plugin's settings, exposing the strict
// override state. ok is false when the plugin has never been configured.
func (s *Store) GetPluginSettingsView(pluginID string) (PluginSettingsView, bool, error) {
	if s == nil || s.db == nil {
		return PluginSettingsView{}, false, fmt.Errorf("store: get plugin settings: store is not open")
	}
	ctx, cancel := s.ctx()
	defer cancel()

	var (
		view      PluginSettingsView
		en        int64
		conc      sql.NullInt64
		strictVal sql.NullInt64
		inst      sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(enabled, 0), COALESCE(mode, ''),
		concurrency, COALESCE(checkin_time, ''), strict, COALESCE(installed_at, 0)
		FROM plugin_settings WHERE plugin_id = ?`, pluginID).
		Scan(&en, &view.Mode, &conc, &view.CheckinTime, &strictVal, &inst)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return PluginSettingsView{}, false, nil
	case err != nil:
		return PluginSettingsView{}, false, fmt.Errorf("store: get plugin settings %q: %w", pluginID, err)
	}
	view.Enabled = en != 0
	if conc.Valid && conc.Int64 > 0 {
		view.Concurrency = int(conc.Int64)
	}
	view.StrictSet = strictVal.Valid
	view.Strict = strictVal.Valid && strictVal.Int64 != 0
	if inst.Valid {
		view.InstalledAt = inst.Int64
	}
	return view, true, nil
}
