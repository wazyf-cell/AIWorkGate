package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"aiworkgate/internal/model"
)

// UpsertClientKey inserts or updates a client key record. Only the SHA-256
// hash is stored; last_used_at is preserved across updates.
func (s *Store) UpsertClientKey(id, name, prefix, hash string, enabled bool, createdAt int64) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: upsert client key: store is not open")
	}
	if id == "" || hash == "" {
		return fmt.Errorf("store: upsert client key: %w: id and hash are required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	_, err := s.db.ExecContext(ctx, `INSERT INTO client_keys
		(id, name, prefix, hash, enabled, created_at, last_used_at)
		VALUES (?, ?, ?, ?, ?, COALESCE(NULLIF(?, 0), CAST(strftime('%s','now') AS INTEGER)), 0)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			prefix = excluded.prefix,
			hash = excluded.hash,
			enabled = excluded.enabled,
			created_at = CASE WHEN ? <> 0 THEN ? ELSE client_keys.created_at END`,
		id, name, prefix, hash, boolInt(enabled), createdAt, createdAt, createdAt)
	if err != nil {
		return fmt.Errorf("store: upsert client key: %w", err)
	}
	return nil
}

// ListClientKeys returns every key ordered by creation time (newest first).
// Each row carries its scope/quota and today's request counter (plan
// 2026-10-21 §1), so the settings table needs no second round trip.
func (s *Store) ListClientKeys() ([]model.ClientKey, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: list client keys: store is not open")
	}
	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT k.id, COALESCE(k.name, ''), COALESCE(k.prefix, ''),
		COALESCE(k.enabled, 0), COALESCE(k.created_at, 0), COALESCE(k.last_used_at, 0),
		COALESCE(k.model_scope, ''), COALESCE(k.bind_pool, ''), COALESCE(k.daily_quota, 0),
		COALESCE(u.requests, 0)
		FROM client_keys k
		LEFT JOIN client_key_usage u ON u.key_id = k.id AND u.day = ?
		ORDER BY k.created_at DESC, k.id DESC`, ClientKeyUsageDay(time.Now()))
	if err != nil {
		return nil, fmt.Errorf("store: list client keys: %w", err)
	}
	defer rows.Close()

	var out []model.ClientKey
	for rows.Next() {
		k, err := scanClientKeyWithUsage(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list client keys: scan: %w", err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list client keys: iterate: %w", err)
	}
	return out, nil
}

// ClientKeyHashByID returns the stored hash for a key id.
func (s *Store) ClientKeyHashByID(id string) (string, bool, error) {
	return s.queryString(`SELECT COALESCE(hash, '') FROM client_keys WHERE id = ?`, "client key hash", id)
}

// ClientKeyIDByHash resolves a presented hash to its key id (auth path).
func (s *Store) ClientKeyIDByHash(hash string) (string, bool, error) {
	return s.queryString(`SELECT id FROM client_keys WHERE hash = ? LIMIT 1`, "client key id", hash)
}

// TouchClientKey records the most recent successful use of a key.
// TouchClientKey records the most recent successful use of a key. Writes are
// throttled to one per 30s per key: the gateway touches on every request and
// the exact second of last_used_at is a display detail, not a billing signal.
func (s *Store) TouchClientKey(id string, at int64) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: touch client key: store is not open")
	}
	if at == 0 {
		at = nowUnix()
	}
	ctx, cancel := s.ctx()
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `UPDATE client_keys SET last_used_at = ?
		WHERE id = ? AND COALESCE(last_used_at, 0) < ?`, at, id, at-30); err != nil {
		return fmt.Errorf("store: touch client key: %w", err)
	}
	return nil
}

// DeleteClientKey removes a key. Its client_key_usage rows are intentionally
// left in place so the ranking keeps the historical usage of deleted keys;
// they expire through CleanupClientKeyUsage instead.
func (s *Store) DeleteClientKey(id string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: delete client key: store is not open")
	}
	ctx, cancel := s.ctx()
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM client_keys WHERE id = ?`, id); err != nil {
		return fmt.Errorf("store: delete client key: %w", err)
	}
	return nil
}

// CleanupClientKeyUsage deletes per-day key counters older than beforeDay
// (YYYY-MM-DD) and reports the number removed. Deleted keys keep their rows
// until this time-based window passes, so the history survives but does not
// grow forever.
func (s *Store) CleanupClientKeyUsage(beforeDay string) (int64, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("store: cleanup client key usage: store is not open")
	}
	if strings.TrimSpace(beforeDay) == "" {
		return 0, fmt.Errorf("store: cleanup client key usage: %w: day is required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	res, err := s.db.ExecContext(ctx, `DELETE FROM client_key_usage WHERE day < ?`, beforeDay)
	if err != nil {
		return 0, fmt.Errorf("store: cleanup client key usage: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: cleanup client key usage: rows affected: %w", err)
	}
	return n, nil
}

// queryString runs a single-column, single-row query and reports whether a
// row existed.
func (s *Store) queryString(q, what string, args ...any) (string, bool, error) {
	if s == nil || s.db == nil {
		return "", false, fmt.Errorf("store: get %s: store is not open", what)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	var v string
	err := s.db.QueryRowContext(ctx, q, args...).Scan(&v)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("store: get %s: %w", what, err)
	}
	return v, true, nil
}
