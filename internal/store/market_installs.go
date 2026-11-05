package store

import (
	"fmt"
)

// MarketInstall is one row of market_installs: the audit record of the last
// market install of a plugin (P2). It is deliberately separate from
// plugin_settings — that table holds the runtime choices, this one holds the
// provenance (source, exact version, tarball sha256) the update check needs.
type MarketInstall struct {
	PluginID    string `json:"plugin_id"`
	Package     string `json:"package"`
	Source      string `json:"source"`
	Version     string `json:"version"`
	SHA256      string `json:"sha256,omitempty"`
	InstalledAt int64  `json:"installed_at"`
}

// UpsertMarketInstall writes or updates the market record of one plugin. The
// row is keyed by plugin id: a reinstall replaces the provenance with the new
// values so only the last state is reported. installed_at defaults to now when
// zero.
func (s *Store) UpsertMarketInstall(rec MarketInstall) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: market install: store is not open")
	}
	if rec.PluginID == "" {
		return fmt.Errorf("store: market install: %w: plugin_id is required", ErrInvalidArgument)
	}
	if rec.InstalledAt == 0 {
		rec.InstalledAt = nowUnix()
	}
	ctx, cancel := s.ctx()
	defer cancel()
	_, err := s.db.ExecContext(ctx, `INSERT INTO market_installs
		(plugin_id, package, source, version, sha256, installed_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(plugin_id) DO UPDATE SET
			package = excluded.package,
			source = excluded.source,
			version = excluded.version,
			sha256 = excluded.sha256,
			installed_at = excluded.installed_at`,
		rec.PluginID, rec.Package, rec.Source, rec.Version, rec.SHA256, rec.InstalledAt)
	if err != nil {
		return fmt.Errorf("store: market install: %w", err)
	}
	return nil
}

// ListMarketInstalls returns every market install record ordered by plugin id.
func (s *Store) ListMarketInstalls() ([]MarketInstall, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: market installs: store is not open")
	}
	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT plugin_id, package, source, version, sha256, installed_at
		FROM market_installs ORDER BY plugin_id`)
	if err != nil {
		return nil, fmt.Errorf("store: market installs: %w", err)
	}
	defer rows.Close()
	var out []MarketInstall
	for rows.Next() {
		var rec MarketInstall
		if err := rows.Scan(&rec.PluginID, &rec.Package, &rec.Source, &rec.Version, &rec.SHA256, &rec.InstalledAt); err != nil {
			return nil, fmt.Errorf("store: market installs: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: market installs: %w", err)
	}
	return out, nil
}
