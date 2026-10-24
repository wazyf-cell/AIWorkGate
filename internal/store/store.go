// Package store owns the host SQLite database at <dataDir>\aiworkgate.db.
//
// The database keeps request logs, check-in history, quota snapshots, client
// key metadata, the proxy pool and per-account egress assignments, plugin
// settings, rule packs and a small key/value area. Secrets (client key hashes
// live here, but proxy passwords and plugin tokens) are stored elsewhere as
// DPAPI blobs; this package only ever receives the encrypted proxy password.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	dbFileName = "aiworkgate.db"

	// busyTimeoutMS is applied to every pooled connection through the DSN so
	// concurrent writers wait instead of failing with SQLITE_BUSY.
	busyTimeoutMS = 5000

	// opTimeout bounds a single statement issued through the helpers below.
	opTimeout = 15 * time.Second
)

var (
	// ErrNotFound is returned (wrapped) when a lookup matches no row.
	ErrNotFound = errors.New("store: not found")
	// ErrInvalidArgument is returned (wrapped) for arguments that cannot be
	// turned into a valid query.
	ErrInvalidArgument = errors.New("store: invalid argument")
)

// DBPath returns the host database path inside dataDir.
func DBPath(dataDir string) string { return filepath.Join(dataDir, dbFileName) }

// Store is a handle to the host database. It is safe for concurrent use.
type Store struct {
	db   *sql.DB
	dir  string
	path string
}

// Open creates dataDir when needed and opens the SQLite database. The schema
// migration is applied before returning, so an opened store is always usable;
// Migrate stays exported (and idempotent) for callers that prefer it explicit.
func Open(dataDir string) (*Store, error) {
	dataDir = strings.TrimSpace(dataDir)
	if dataDir == "" {
		return nil, fmt.Errorf("store: open: empty data dir")
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("store: open: create data dir %q: %w", dataDir, err)
	}
	p := filepath.Join(dataDir, dbFileName)
	if strings.ContainsRune(p, '?') {
		return nil, fmt.Errorf("store: open: unsupported '?' in path %q", p)
	}

	// The DSN carries the pragmas so every connection opened by database/sql
	// inherits them (WAL, busy timeout and synchronous=NORMAL are per
	// connection, not per database).
	dsn := fmt.Sprintf("%s?_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)",
		filepath.ToSlash(p), busyTimeoutMS)

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %q: %w", p, err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	db.SetConnMaxIdleTime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: open %q: ping: %w", p, err)
	}

	s := &Store{db: db, dir: dataDir, path: p}
	if err := s.Migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database handle.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("store: close: %w", err)
	}
	return nil
}

// Path returns the absolute database file path.
func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Dir returns the data directory the store lives in.
func (s *Store) Dir() string {
	if s == nil {
		return ""
	}
	return s.dir
}

// DB exposes the underlying handle for callers that need an ad-hoc query
// (the web UI statistics endpoints). Callers must not close it.
func (s *Store) DB() *sql.DB {
	if s == nil {
		return nil
	}
	return s.db
}

// ctx returns a bounded context for internal statements.
func (s *Store) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), opTimeout)
}

// migrationStep is one ordered schema step. The index+1 of a step is the
// user_version it produces. Every statement must be safe to re-run so a
// partially applied database can still be brought up to date; columns added to
// pre-existing tables are applied only when PRAGMA table_info says they are
// missing, which makes the step safe on a live v1 database.
type migrationStep struct {
	sql     []string
	columns []columnMigration
}

// columnMigration adds one column to a table created by an earlier step.
type columnMigration struct {
	table  string
	column string
	decl   string
}

// migrations is an ordered list of schema steps.
var migrations = []migrationStep{
	{sql: []string{ // version 1: initial schema
		`CREATE TABLE IF NOT EXISTS api_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ts TEXT NOT NULL,
			req_id TEXT,
			source TEXT,
			model TEXT,
			key_id TEXT,
			key_name TEXT,
			account TEXT,
			stream INTEGER,
			status INTEGER,
			latency_ms INTEGER,
			ttfb_ms INTEGER,
			prompt_tokens INTEGER,
			completion_tokens INTEGER,
			error TEXT,
			body_req TEXT,
			body_resp TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS checkin_results (
			id INTEGER PRIMARY KEY,
			plugin_id TEXT NOT NULL,
			account_id TEXT NOT NULL,
			account_label TEXT,
			date TEXT NOT NULL,
			result TEXT,
			credits_before REAL NULL,
			credits_after REAL NULL,
			message TEXT,
			at INTEGER,
			UNIQUE(plugin_id, account_id, date)
		)`,
		`CREATE TABLE IF NOT EXISTS quota_snapshots (
			plugin_id TEXT NOT NULL,
			account_id TEXT NOT NULL,
			remaining REAL,
			total REAL,
			at INTEGER NOT NULL,
			PRIMARY KEY(plugin_id, account_id, at)
		)`,
		`CREATE TABLE IF NOT EXISTS client_keys (
			id TEXT PRIMARY KEY,
			name TEXT,
			prefix TEXT,
			hash TEXT,
			enabled INTEGER,
			created_at INTEGER,
			last_used_at INTEGER
		)`,
		`CREATE TABLE IF NOT EXISTS proxies (
			id TEXT PRIMARY KEY,
			name TEXT,
			type TEXT,
			host TEXT,
			port INTEGER,
			username TEXT,
			password_enc TEXT,
			enabled INTEGER,
			last_check_at INTEGER,
			last_exit_ip TEXT,
			last_latency_ms INTEGER,
			last_error TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS account_egress (
			plugin_id TEXT NOT NULL,
			account_id TEXT NOT NULL,
			mode TEXT,
			proxy_id TEXT,
			PRIMARY KEY(plugin_id, account_id)
		)`,
		`CREATE TABLE IF NOT EXISTS plugin_settings (
			plugin_id TEXT PRIMARY KEY,
			enabled INTEGER,
			mode TEXT,
			concurrency INTEGER,
			checkin_time TEXT,
			strict INTEGER,
			installed_at INTEGER,
			updated_at INTEGER
		)`,
		`CREATE TABLE IF NOT EXISTS rule_packs (
			name TEXT PRIMARY KEY,
			content TEXT,
			updated_at INTEGER
		)`,
		`CREATE TABLE IF NOT EXISTS kv (
			key TEXT PRIMARY KEY,
			content TEXT,
			updated_at INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS idx_api_logs_ts ON api_logs(ts)`,
		`CREATE INDEX IF NOT EXISTS idx_api_logs_source ON api_logs(source)`,
		`CREATE INDEX IF NOT EXISTS idx_checkin_results_date ON checkin_results(date)`,
		`CREATE INDEX IF NOT EXISTS idx_quota_snapshots_at ON quota_snapshots(at)`,
	}},
	{ // version 2: S1 account center (design §4)
		sql: []string{
			`CREATE TABLE IF NOT EXISTS account_meta (
				plugin_id TEXT NOT NULL,
				account_id TEXT NOT NULL,
				alias TEXT,
				group_id TEXT,
				enabled_override INTEGER,
				priority INTEGER,
				weight REAL,
				note TEXT,
				updated_at INTEGER,
				PRIMARY KEY (plugin_id, account_id)
			)`,
			`CREATE TABLE IF NOT EXISTS account_groups (
				id TEXT PRIMARY KEY,
				name TEXT NOT NULL UNIQUE,
				color TEXT,
				sort INTEGER,
				created_at INTEGER
			)`,
			`CREATE TABLE IF NOT EXISTS pool_members (
				plugin_id TEXT NOT NULL,
				account_id TEXT NOT NULL,
				enabled INTEGER NOT NULL DEFAULT 1,
				updated_at INTEGER,
				PRIMARY KEY (plugin_id, account_id)
			)`,
			`CREATE TABLE IF NOT EXISTS pool_settings (
				plugin_id TEXT PRIMARY KEY,
				upstream_enabled INTEGER,
				strategy TEXT,
				pool_priority INTEGER,
				account_concurrency INTEGER,
				pool_sticky_ttl INTEGER,
				conv_sticky_ttl INTEGER,
				hedge_ms INTEGER,
				default_reasoning TEXT,
				background_downgrade INTEGER,
				longctx_downgrade INTEGER,
				tool_exec INTEGER,
				model_sync_time TEXT,
				updated_at INTEGER
			)`,
			`CREATE TABLE IF NOT EXISTS model_bindings (
				logical_model TEXT PRIMARY KEY,
				display_name TEXT,
				updated_at INTEGER
			)`,
			`CREATE TABLE IF NOT EXISTS model_binding_items (
				logical_model TEXT NOT NULL,
				position INTEGER NOT NULL,
				source_plugin TEXT NOT NULL,
				source_model TEXT NOT NULL,
				PRIMARY KEY (logical_model, position)
			)`,
			`CREATE TABLE IF NOT EXISTS model_catalog (
				plugin_id TEXT NOT NULL,
				model_id TEXT NOT NULL,
				display_name TEXT,
				vendor TEXT,
				multiplier REAL DEFAULT 1,
				thinking TEXT,
				context_length INTEGER,
				vision INTEGER,
				free INTEGER,
				synced_at INTEGER,
				PRIMARY KEY (plugin_id, model_id)
			)`,
			`CREATE TABLE IF NOT EXISTS sticky_bindings (
				scope TEXT NOT NULL,
				key TEXT NOT NULL,
				plugin_id TEXT NOT NULL,
				account_id TEXT NOT NULL,
				expires_at INTEGER NOT NULL,
				PRIMARY KEY (scope, key)
			)`,
			`CREATE TABLE IF NOT EXISTS usage_daily (
				day TEXT NOT NULL,
				plugin_id TEXT NOT NULL,
				account_id TEXT NOT NULL,
				model TEXT NOT NULL,
				requests INTEGER,
				errors INTEGER,
				prompt_tokens INTEGER,
				completion_tokens INTEGER,
				credits REAL,
				latency_ms_sum INTEGER,
				ttfb_ms_sum INTEGER,
				ttfb_count INTEGER,
				PRIMARY KEY (day, plugin_id, account_id, model)
			)`,
			`CREATE TABLE IF NOT EXISTS audit_log (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				ts TEXT NOT NULL,
				category TEXT,
				action TEXT,
				target TEXT,
				detail TEXT,
				result TEXT
			)`,
			`CREATE TABLE IF NOT EXISTS account_cache (
				plugin_id TEXT NOT NULL,
				account_id TEXT NOT NULL,
				payload TEXT NOT NULL,
				updated_at INTEGER,
				PRIMARY KEY (plugin_id, account_id)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_api_logs_ts_source ON api_logs(ts, source)`,
			`CREATE INDEX IF NOT EXISTS idx_api_logs_account_ts ON api_logs(account, ts)`,
			`CREATE INDEX IF NOT EXISTS idx_audit_log_ts ON audit_log(ts)`,
			`CREATE INDEX IF NOT EXISTS idx_account_meta_group ON account_meta(group_id)`,
			`CREATE INDEX IF NOT EXISTS idx_usage_daily_day ON usage_daily(day)`,
			`CREATE INDEX IF NOT EXISTS idx_sticky_bindings_expires ON sticky_bindings(expires_at)`,
		},
		columns: []columnMigration{
			{"api_logs", "conversation_id", "TEXT"},
			{"api_logs", "logical_model", "TEXT"},
			{"api_logs", "credits", "REAL"},
			{"api_logs", "first_token_at", "TEXT"},
			{"quota_snapshots", "expires_at", "INTEGER"},
			{"quota_snapshots", "plan", "TEXT"},
		},
	},
	{ // version 3: per-model exposure switch (resource page "接入")
		columns: []columnMigration{
			{"model_catalog", "enabled", "INTEGER NOT NULL DEFAULT 1"},
		},
	},
	{ // version 4: key scopes/quotas, catalog overrides, account health, cache usage
		sql: []string{
			// Per-key daily counters (item 1 + 5). One row per key per day.
			`CREATE TABLE IF NOT EXISTS client_key_usage (
				day TEXT NOT NULL,
				key_id TEXT NOT NULL,
				requests INTEGER NOT NULL DEFAULT 0,
				prompt_tokens INTEGER NOT NULL DEFAULT 0,
				completion_tokens INTEGER NOT NULL DEFAULT 0,
				cached_tokens INTEGER NOT NULL DEFAULT 0,
				credits REAL,
				PRIMARY KEY (day, key_id)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_client_key_usage_key ON client_key_usage(key_id, day)`,
			// Human overrides for catalog metadata. NULL means "no override",
			// so a sync may refresh the underlying value without touching it.
			`CREATE TABLE IF NOT EXISTS model_overrides (
				plugin_id TEXT NOT NULL,
				model TEXT NOT NULL,
				display_name TEXT,
				vendor TEXT,
				context_length INTEGER,
				vision INTEGER,
				thinking TEXT,
				multiplier REAL,
				free INTEGER,
				updated_at INTEGER,
				PRIMARY KEY (plugin_id, model)
			)`,
		},
		columns: []columnMigration{
			{"client_keys", "model_scope", "TEXT"},
			{"client_keys", "bind_pool", "TEXT"},
			{"client_keys", "daily_quota", "INTEGER NOT NULL DEFAULT 0"},
			{"account_meta", "status_kind", "TEXT"},
			{"account_meta", "cooldown_until", "INTEGER"},
			{"account_meta", "cooldown_reason", "TEXT"},
			{"account_meta", "failure_count", "INTEGER NOT NULL DEFAULT 0"},
			{"checkin_results", "dedup_scope", "TEXT"},
			{"checkin_results", "device_key", "TEXT"},
			{"usage_daily", "cached_tokens", "INTEGER"},
			{"api_logs", "cached_tokens", "INTEGER"},
		},
	},
	{ // version 5: user-defined custom models (OpenAI-compatible upstreams)
		sql: []string{
			// Ported from A (TraeWorkAssistant, MIT, 朱天伟)
			// custom_models.rs:23-54: one row per user-defined model. The API
			// key is deliberately absent here; it lives in the DPAPI store under
			// "custom_model.<id>.api_key" (see CustomModelSecretKey).
			`CREATE TABLE IF NOT EXISTS custom_models (
				id TEXT PRIMARY KEY,
				name TEXT NOT NULL,
				base_url TEXT NOT NULL,
				upstream_model TEXT NOT NULL,
				enabled INTEGER NOT NULL DEFAULT 1,
				created_at INTEGER NOT NULL,
				updated_at INTEGER NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_custom_models_name ON custom_models(name)`,
		},
	},
	{ // version 6: request-path scheduler cooldowns (durable across restarts)
		sql: []string{
			// Ported from A (TraeWorkAssistant, MIT, 朱天伟) store/docs.rs:1266-1290
			// (account_cooldowns_load/save) and models.rs:712-731 (CooldownEntry):
			// A persists the cooldown deadline together with the consecutive-error
			// count, so a restart resumes the backoff instead of probing upstream
			// again from the base duration. model_id = '' marks an account-wide
			// cooldown (5xx/408/409); a non-empty model_id marks a 429 cooldown of
			// one account+model pair (A pool.rs:331-370 restores the same split).
			`CREATE TABLE IF NOT EXISTS scheduler_cooldowns (
				plugin_id TEXT NOT NULL,
				account_id TEXT NOT NULL,
				model_id TEXT NOT NULL DEFAULT '',
				until INTEGER NOT NULL,
				reason TEXT NOT NULL DEFAULT '',
				strikes INTEGER NOT NULL DEFAULT 0,
				updated_at INTEGER NOT NULL,
				PRIMARY KEY(plugin_id, account_id, model_id)
			)`,
			// Startup/load prunes by deadline; keep that scan off the table scan.
			`CREATE INDEX IF NOT EXISTS idx_scheduler_cooldowns_until ON scheduler_cooldowns(until)`,
		},
	},
	{ // version 7: connectivity probe history (Settings → 连通性自检)
		sql: []string{
			// One row per explicit probe, written whether the probe succeeded or
			// failed so the history shows outages as well as recoveries. Rows
			// older than 90 days are pruned on insert (InsertConnectivityProbe),
			// which keeps the table bounded without a startup hook.
			`CREATE TABLE IF NOT EXISTS connectivity_probes (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				kind TEXT NOT NULL,
				target TEXT NOT NULL DEFAULT '',
				target_id TEXT NOT NULL DEFAULT '',
				ok INTEGER NOT NULL DEFAULT 0,
				status INTEGER NOT NULL DEFAULT 0,
				latency_ms INTEGER NOT NULL DEFAULT 0,
				detail TEXT NOT NULL DEFAULT '',
				error_code TEXT NOT NULL DEFAULT '',
				created_at INTEGER NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_connectivity_probes_created ON connectivity_probes(created_at)`,
		},
	},
	{ // version 8: custom model connections + per-model capabilities
		sql: []string{
			// One connection is one (endpoint, API key) pair. The key stays in
			// the DPAPI store under "custom_connection.<id>.api_key"; this table
			// never contains it. A connection owns many models.
			`CREATE TABLE IF NOT EXISTS custom_connections (
				id TEXT PRIMARY KEY,
				label TEXT NOT NULL DEFAULT '',
				protocol TEXT NOT NULL DEFAULT 'openai',
				base_url TEXT NOT NULL,
				enabled INTEGER NOT NULL DEFAULT 1,
				created_at INTEGER NOT NULL,
				updated_at INTEGER NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_custom_connections_base ON custom_connections(base_url)`,
		},
		columns: []columnMigration{
			{"custom_models", "connection_id", "TEXT"},
			{"custom_models", "context_window", "INTEGER NOT NULL DEFAULT 0"},
			{"custom_models", "max_output_tokens", "INTEGER NOT NULL DEFAULT 0"},
			// Existing entries keep their current behaviour: images were passed
			// through and no reasoning tier was imposed.
			{"custom_models", "input_types", `TEXT NOT NULL DEFAULT '["text","image"]'`},
			{"custom_models", "reasoning_tiers", `TEXT NOT NULL DEFAULT '[]'`},
			{"custom_models", "default_reasoning", "TEXT NOT NULL DEFAULT ''"},
		},
	},
	{ // version 9: proxy (egress) traffic log
		sql: []string{
			// One row per tunnelled CONNECT through a local forwarder. The
			// payload is never stored: a tunnel is opaque TLS, and the row only
			// carries identity, target, outcome, duration and byte counters.
			`CREATE TABLE IF NOT EXISTS proxy_logs (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				ts TEXT NOT NULL,
				at INTEGER NOT NULL,
				plugin_id TEXT NOT NULL DEFAULT '',
				account_id TEXT NOT NULL DEFAULT '',
				proxy_id TEXT NOT NULL DEFAULT '',
				proxy_label TEXT NOT NULL DEFAULT '',
				target_host TEXT NOT NULL DEFAULT '',
				target_port INTEGER NOT NULL DEFAULT 0,
				result TEXT NOT NULL DEFAULT '',
				status INTEGER NOT NULL DEFAULT 0,
				duration_ms INTEGER NOT NULL DEFAULT 0,
				bytes_up INTEGER NOT NULL DEFAULT 0,
				bytes_down INTEGER NOT NULL DEFAULT 0,
				error TEXT NOT NULL DEFAULT ''
			)`,
			`CREATE INDEX IF NOT EXISTS idx_proxy_logs_at ON proxy_logs(at)`,
			`CREATE INDEX IF NOT EXISTS idx_proxy_logs_plugin ON proxy_logs(plugin_id, account_id, at)`,
		},
	},
	{ // version 10: timeline keyset paging
		sql: []string{
			// The merged timeline pages by (timestamp, id) instead of loading a
			// capped window. These expression indexes match the ORDER BY and
			// keyset predicates in timelineEpochExpr exactly, so page N costs
			// one index seek instead of a growing OFFSET scan.
			`CREATE INDEX IF NOT EXISTS idx_api_logs_timeline
				ON api_logs(CAST(unixepoch(ts, 'subsec') * 1000 AS INTEGER), id)`,
			`CREATE INDEX IF NOT EXISTS idx_audit_log_timeline
				ON audit_log(CAST(unixepoch(ts, 'subsec') * 1000 AS INTEGER), id)`,
			// proxy_logs already stores at; last_error/connectivity use their own
			// integer timestamps and existing indexes.
			`CREATE INDEX IF NOT EXISTS idx_proxy_logs_timeline ON proxy_logs(at, id)`,
		},
	},
}

// Migrate applies pending schema steps using PRAGMA user_version. It is
// idempotent and safe to call on every start; a pending upgrade first copies the
// database aside so the new schema can be rolled back.
func (s *Store) Migrate() error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: migrate: store is not open")
	}
	ctx, cancel := s.ctx()
	defer cancel()

	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("store: migrate: read user_version: %w", err)
	}
	if version < 0 {
		return fmt.Errorf("store: migrate: invalid user_version %d", version)
	}
	if version > len(migrations) {
		return fmt.Errorf("store: migrate: database schema v%d is newer than this build (v%d)", version, len(migrations))
	}
	if version < len(migrations) {
		if err := s.backupBeforeMigration(version, len(migrations)); err != nil {
			return err
		}
	}
	for i := version; i < len(migrations); i++ {
		next := i + 1
		step := migrations[i]
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("store: migrate: begin v%d: %w", next, err)
		}
		for _, stmt := range step.sql {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("store: migrate: v%d: %w", next, err)
			}
		}
		for _, col := range step.columns {
			if err := ensureColumn(ctx, tx, col); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("store: migrate: v%d: %w", next, err)
			}
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", next)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: migrate: set user_version %d: %w", next, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: migrate: commit v%d: %w", next, err)
		}
	}
	return nil
}

// backupName renders the pre-migration backup path for a from-to upgrade.
func (s *Store) backupName(from, to int) string {
	return fmt.Sprintf("%s.bak-v%d-v%d", s.path, from, to)
}

// backupBeforeMigration copies the database, plus its WAL when one exists, to
// <db>.bak-v<from>-v<to> before the first step that raises user_version. An
// existing backup is never overwritten, and a database without tables has no
// state worth protecting yet.
func (s *Store) backupBeforeMigration(from, to int) error {
	ctx, cancel := s.ctx()
	defer cancel()
	if from == 0 {
		var tables int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table'`).Scan(&tables); err != nil {
			return fmt.Errorf("store: migrate: inspect schema before backup: %w", err)
		}
		if tables == 0 {
			return nil
		}
	}
	dst := s.backupName(from, to)
	if _, err := os.Stat(dst); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("store: migrate: stat backup %s: %w", dst, err)
	}
	if err := copyFile(s.path, dst); err != nil {
		return err
	}
	// The WAL copy belongs to that backup: both files are renamed back together.
	if wal := s.path + "-wal"; fileExists(wal) {
		if err := copyFile(wal, dst+"-wal"); err != nil {
			_ = os.Remove(dst) // a half-written set must not look like a valid backup
			return err
		}
	}
	return nil
}

// copyFile makes a durable copy of src at dst.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("store: migrate: read %s: %w", src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("store: migrate: create %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("store: migrate: copy to %s: %w", dst, err)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return fmt.Errorf("store: migrate: sync %s: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("store: migrate: close %s: %w", dst, err)
	}
	return nil
}

// fileExists reports a regular file at path.
func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

// ensureColumn adds a column when PRAGMA table_info does not list it yet, so a
// database that was partially migrated (or hand-patched) still converges.
func ensureColumn(ctx context.Context, tx *sql.Tx, col columnMigration) error {
	rows, err := tx.QueryContext(ctx, "PRAGMA table_info("+col.table+")")
	if err != nil {
		return fmt.Errorf("inspect %s: %w", col.table, err)
	}
	found := false
	for rows.Next() {
		var (
			cid       int
			name      string
			ctype     string
			notNull   int
			dfltValue sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dfltValue, &pk); err != nil {
			rows.Close()
			return fmt.Errorf("inspect %s: %w", col.table, err)
		}
		if name == col.column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("inspect %s: %w", col.table, err)
	}
	rows.Close()
	if found {
		return nil
	}
	stmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", col.table, col.column, col.decl)
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("add %s.%s: %w", col.table, col.column, err)
	}
	return nil
}

// SchemaVersion reports PRAGMA user_version.
func (s *Store) SchemaVersion() (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("store: schema version: store is not open")
	}
	ctx, cancel := s.ctx()
	defer cancel()
	var v int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("store: schema version: %w", err)
	}
	return v, nil
}

// resetForTests drops every user table; used by package tests only.
func (s *Store) resetForTests() error {
	tables := []string{
		"api_logs", "checkin_results", "quota_snapshots", "client_keys",
		"proxies", "account_egress", "plugin_settings", "rule_packs", "kv",
		"account_meta", "account_groups", "pool_members", "pool_settings",
		"model_bindings", "model_binding_items", "model_catalog",
		"sticky_bindings", "usage_daily", "audit_log", "account_cache",
	}
	ctx, cancel := s.ctx()
	defer cancel()
	for _, t := range tables {
		if _, err := s.db.ExecContext(ctx, "DELETE FROM "+t); err != nil {
			return err
		}
	}
	return nil
}

// nowUnix is the default timestamp source.
func nowUnix() int64 { return time.Now().Unix() }

// nullStr stores an empty string as NULL so optional columns stay absent.
func nullStr(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// boolInt converts a bool to the 0/1 form stored in SQLite.
func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// optBool maps an optional bool to a NULL-able driver argument.
func optBool(v *bool) any {
	if v == nil {
		return nil
	}
	return boolInt(*v)
}

// optInt maps an optional int to a NULL-able driver argument.
func optInt(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

// optFloat maps an optional float to a NULL-able driver argument.
func optFloat(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}
