package store

import (
	"fmt"
)

// RulePackRecord is one stored rule pack. Content is the literal JSON document
// the host merges into runtime\<plugin>\rules.json.
type RulePackRecord struct {
	Name      string `json:"name"`
	Content   string `json:"content"`
	UpdatedAt int64  `json:"updated_at"`
}

// UpsertRulePack inserts or replaces a rule pack's JSON content.
func (s *Store) UpsertRulePack(name, content string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: upsert rule pack: store is not open")
	}
	if name == "" {
		return fmt.Errorf("store: upsert rule pack: %w: name is required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO rule_packs (name, content, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			content = excluded.content,
			updated_at = excluded.updated_at`,
		name, content, nowUnix()); err != nil {
		return fmt.Errorf("store: upsert rule pack %q: %w", name, err)
	}
	return nil
}

// ListRulePacks returns all packs ordered by name, content included.
func (s *Store) ListRulePacks() ([]RulePackRecord, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: list rule packs: store is not open")
	}
	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT name, COALESCE(content, ''), COALESCE(updated_at, 0)
		FROM rule_packs ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: list rule packs: %w", err)
	}
	defer rows.Close()

	var out []RulePackRecord
	for rows.Next() {
		var r RulePackRecord
		if err := rows.Scan(&r.Name, &r.Content, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: list rule packs: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list rule packs: iterate: %w", err)
	}
	return out, nil
}

// GetRulePack returns one pack's content.
func (s *Store) GetRulePack(name string) (string, bool, error) {
	return s.queryString(`SELECT COALESCE(content, '') FROM rule_packs WHERE name = ?`, "rule pack "+name, name)
}

// DeleteRulePack removes a pack.
func (s *Store) DeleteRulePack(name string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: delete rule pack: store is not open")
	}
	ctx, cancel := s.ctx()
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM rule_packs WHERE name = ?`, name); err != nil {
		return fmt.Errorf("store: delete rule pack %q: %w", name, err)
	}
	return nil
}
