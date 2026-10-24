package store

import (
	"database/sql"
	"errors"
	"fmt"

	"aiworkgate/internal/model"
)

const proxyColumns = `id, COALESCE(name, ''), COALESCE(type, ''), COALESCE(host, ''), COALESCE(port, 0),
	COALESCE(username, ''), COALESCE(enabled, 0), COALESCE(last_check_at, 0),
	COALESCE(last_exit_ip, ''), COALESCE(last_latency_ms, 0), COALESCE(last_error, '')`

// UpsertProxy inserts or updates a proxy. passwordEnc carries the DPAPI blob;
// an empty value keeps the previously stored password so edits that only touch
// the display fields do not wipe the credential.
func (s *Store) UpsertProxy(p model.Proxy, passwordEnc string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: upsert proxy: store is not open")
	}
	if p.ID == "" {
		return fmt.Errorf("store: upsert proxy: %w: id is required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	_, err := s.db.ExecContext(ctx, `INSERT INTO proxies
		(id, name, type, host, port, username, password_enc, enabled,
		 last_check_at, last_exit_ip, last_latency_ms, last_error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			type = excluded.type,
			host = excluded.host,
			port = excluded.port,
			username = excluded.username,
			password_enc = CASE WHEN excluded.password_enc IS NOT NULL AND excluded.password_enc <> ''
				THEN excluded.password_enc ELSE proxies.password_enc END,
			enabled = excluded.enabled,
			last_check_at = excluded.last_check_at,
			last_exit_ip = excluded.last_exit_ip,
			last_latency_ms = excluded.last_latency_ms,
			last_error = excluded.last_error`,
		p.ID, p.Name, p.Type, p.Host, p.Port, p.Username, nullStr(passwordEnc),
		boolInt(p.Enabled), p.LastCheckAt, nullStr(p.LastExitIP), p.LastLatencyMS, nullStr(p.LastError))
	if err != nil {
		return fmt.Errorf("store: upsert proxy: %w", err)
	}
	return nil
}

// ListProxies returns all proxies by name. Passwords are never included.
func (s *Store) ListProxies() ([]model.Proxy, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: list proxies: store is not open")
	}
	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT `+proxyColumns+` FROM proxies ORDER BY name, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list proxies: %w", err)
	}
	defer rows.Close()

	var out []model.Proxy
	for rows.Next() {
		p, err := scanProxy(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list proxies: scan: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list proxies: iterate: %w", err)
	}
	return out, nil
}

// GetProxy returns one proxy together with its encrypted password blob. A
// missing row yields ErrNotFound.
func (s *Store) GetProxy(id string) (model.Proxy, string, error) {
	if s == nil || s.db == nil {
		return model.Proxy{}, "", fmt.Errorf("store: get proxy: store is not open")
	}
	ctx, cancel := s.ctx()
	defer cancel()
	row := s.db.QueryRowContext(ctx, `SELECT `+proxyColumns+`, COALESCE(password_enc, '')
		FROM proxies WHERE id = ?`, id)
	p, enc, err := scanProxyWithPassword(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Proxy{}, "", fmt.Errorf("store: get proxy %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return model.Proxy{}, "", fmt.Errorf("store: get proxy %q: %w", id, err)
	}
	return p, enc, nil
}

// DeleteProxy removes a proxy row.
func (s *Store) DeleteProxy(id string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: delete proxy: store is not open")
	}
	ctx, cancel := s.ctx()
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM proxies WHERE id = ?`, id); err != nil {
		return fmt.Errorf("store: delete proxy: %w", err)
	}
	return nil
}

// UpdateProxyHealth records the outcome of a latency / exit IP probe.
func (s *Store) UpdateProxyHealth(id string, checkedAt int64, exitIP string, latencyMS int64, errText string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: update proxy health: store is not open")
	}
	if checkedAt == 0 {
		checkedAt = nowUnix()
	}
	ctx, cancel := s.ctx()
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `UPDATE proxies SET
		last_check_at = ?, last_exit_ip = ?, last_latency_ms = ?, last_error = ?
		WHERE id = ?`, checkedAt, nullStr(exitIP), latencyMS, nullStr(errText), id); err != nil {
		return fmt.Errorf("store: update proxy health: %w", err)
	}
	return nil
}

// rowScanner abstracts *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanProxy(sc rowScanner) (model.Proxy, error) {
	var (
		p       model.Proxy
		enabled int64
	)
	if err := sc.Scan(&p.ID, &p.Name, &p.Type, &p.Host, &p.Port, &p.Username,
		&enabled, &p.LastCheckAt, &p.LastExitIP, &p.LastLatencyMS, &p.LastError); err != nil {
		return model.Proxy{}, err
	}
	p.Enabled = enabled != 0
	return p, nil
}

func scanProxyWithPassword(sc rowScanner) (model.Proxy, string, error) {
	var (
		p       model.Proxy
		enabled int64
		enc     string
	)
	if err := sc.Scan(&p.ID, &p.Name, &p.Type, &p.Host, &p.Port, &p.Username,
		&enabled, &p.LastCheckAt, &p.LastExitIP, &p.LastLatencyMS, &p.LastError, &enc); err != nil {
		return model.Proxy{}, "", err
	}
	p.Enabled = enabled != 0
	return p, enc, nil
}
