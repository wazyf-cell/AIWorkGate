package store

import (
	"fmt"
)

// GetKV reads a small host-side document (e.g. last-seen version, UI state).
func (s *Store) GetKV(key string) (string, bool, error) {
	if key == "" {
		return "", false, fmt.Errorf("store: get kv: %w: key is required", ErrInvalidArgument)
	}
	return s.queryString(`SELECT COALESCE(content, '') FROM kv WHERE key = ?`, "kv "+key, key)
}

// SetKV writes a small host-side document.
func (s *Store) SetKV(key, content string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: set kv: store is not open")
	}
	if key == "" {
		return fmt.Errorf("store: set kv: %w: key is required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO kv (key, content, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET
			content = excluded.content,
			updated_at = excluded.updated_at`,
		key, content, nowUnix()); err != nil {
		return fmt.Errorf("store: set kv %q: %w", key, err)
	}
	return nil
}

// DeleteKV removes a key.
func (s *Store) DeleteKV(key string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: delete kv: store is not open")
	}
	ctx, cancel := s.ctx()
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM kv WHERE key = ?`, key); err != nil {
		return fmt.Errorf("store: delete kv %q: %w", key, err)
	}
	return nil
}
