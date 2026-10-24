package store

// User-defined upstream connections (Settings → 自定义模型 → 接入).
//
// A connection is one (endpoint, API key) pair: it owns one protocol, one base
// URL and many models. The key is never stored in SQLite; it lives in the DPAPI
// secret store under CustomConnectionSecretKey(id), exactly like proxy passwords.

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"aiworkgate/internal/model"
)

const (
	ProtocolOpenAI    = model.ProtocolOpenAI
	ProtocolAnthropic = model.ProtocolAnthropic
	ProtocolGemini    = model.ProtocolGemini
	// MaxCustomConnectionLabelRunes bounds the display label.
	MaxCustomConnectionLabelRunes = 64
)

// CustomConnection is one user-configured upstream endpoint and credential.
// KeySet is filled by the host; ModelCount is a list-only annotation.
type CustomConnection struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	Protocol   string `json:"protocol"`
	BaseURL    string `json:"base_url"`
	Enabled    bool   `json:"enabled"`
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
	KeySet     bool   `json:"key_set"`
	ModelCount int    `json:"model_count,omitempty"`
}

// CustomConnectionSecretKey names the DPAPI extra holding one connection key.
func CustomConnectionSecretKey(id string) string {
	return "custom_connection." + strings.TrimSpace(id) + ".api_key"
}

// NormalizeCustomProtocol folds a protocol name and reports whether it is
// supported. An empty value means the historical OpenAI-compatible protocol.
func NormalizeCustomProtocol(raw string) (string, bool) {
	return model.NormalizeProtocol(raw)
}

// normalize folds strings and validates one connection. IDs and timestamps are
// managed by the store.
func (c CustomConnection) normalize() (CustomConnection, error) {
	out := c
	out.ID = strings.TrimSpace(out.ID)
	out.Label = strings.TrimSpace(out.Label)
	out.BaseURL = strings.TrimRight(strings.TrimSpace(out.BaseURL), "/")
	protocol, ok := NormalizeCustomProtocol(out.Protocol)
	if !ok {
		return CustomConnection{}, fmt.Errorf("store: custom connection: %w: 不支持的协议 %q", ErrInvalidArgument, out.Protocol)
	}
	out.Protocol = protocol
	if utf8.RuneCountInString(out.Label) > MaxCustomConnectionLabelRunes {
		return CustomConnection{}, fmt.Errorf("store: custom connection: %w: 名称最多 %d 个字符",
			ErrInvalidArgument, MaxCustomConnectionLabelRunes)
	}
	if err := ValidateCustomModelBaseURL(out.BaseURL); err != nil {
		return CustomConnection{}, err
	}
	return out, nil
}

// ListCustomConnections returns every connection ordered by label. The API key
// is never loaded; ModelCount is computed in the same query.
func (s *Store) ListCustomConnections() ([]CustomConnection, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: list custom connections: store is not open")
	}
	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT c.id, c.label, c.protocol, c.base_url, c.enabled,
		c.created_at, c.updated_at,
		(SELECT COUNT(*) FROM custom_models m WHERE m.connection_id = c.id)
		FROM custom_connections c ORDER BY c.label COLLATE NOCASE, c.id`)
	if err != nil {
		return nil, fmt.Errorf("store: list custom connections: %w", err)
	}
	defer rows.Close()
	out := make([]CustomConnection, 0, 4)
	for rows.Next() {
		var (
			c       CustomConnection
			enabled int64
		)
		if err := rows.Scan(&c.ID, &c.Label, &c.Protocol, &c.BaseURL, &enabled,
			&c.CreatedAt, &c.UpdatedAt, &c.ModelCount); err != nil {
			return nil, fmt.Errorf("store: list custom connections: %w", err)
		}
		c.Enabled = enabled != 0
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list custom connections: %w", err)
	}
	return out, nil
}

// GetCustomConnection returns one connection by id.
func (s *Store) GetCustomConnection(id string) (CustomConnection, bool, error) {
	if s == nil || s.db == nil {
		return CustomConnection{}, false, fmt.Errorf("store: get custom connection: store is not open")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return CustomConnection{}, false, fmt.Errorf("store: get custom connection: %w: id is required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	var (
		c       CustomConnection
		enabled int64
	)
	err := s.db.QueryRowContext(ctx, `SELECT id, label, protocol, base_url, enabled, created_at, updated_at
		FROM custom_connections WHERE id = ?`, id).
		Scan(&c.ID, &c.Label, &c.Protocol, &c.BaseURL, &enabled, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return CustomConnection{}, false, nil
	}
	if err != nil {
		return CustomConnection{}, false, fmt.Errorf("store: get custom connection: %w", err)
	}
	c.Enabled = enabled != 0
	return c, true, nil
}

// UpsertCustomConnection validates and writes one connection. An empty ID
// creates "cc-<12hex>"; an existing row keeps created_at.
func (s *Store) UpsertCustomConnection(c CustomConnection) (CustomConnection, error) {
	if s == nil || s.db == nil {
		return CustomConnection{}, fmt.Errorf("store: upsert custom connection: store is not open")
	}
	entry, err := c.normalize()
	if err != nil {
		return CustomConnection{}, err
	}
	ctx, cancel := s.ctx()
	defer cancel()
	now := nowUnix()
	createdAt := now
	if entry.ID == "" {
		id, err := newCustomConnectionID()
		if err != nil {
			return CustomConnection{}, err
		}
		entry.ID = id
	} else {
		var existing int64
		err := s.db.QueryRowContext(ctx, `SELECT created_at FROM custom_connections WHERE id = ?`, entry.ID).Scan(&existing)
		switch {
		case err == nil:
			createdAt = existing
		case errors.Is(err, sql.ErrNoRows):
			// Explicit unknown id inserts; callers only send ids they just read.
		default:
			return CustomConnection{}, fmt.Errorf("store: upsert custom connection: %w", err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO custom_connections
		(id, label, protocol, base_url, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			label = excluded.label,
			protocol = excluded.protocol,
			base_url = excluded.base_url,
			enabled = excluded.enabled,
			updated_at = excluded.updated_at`,
		entry.ID, entry.Label, entry.Protocol, entry.BaseURL,
		boolInt(entry.Enabled), createdAt, now); err != nil {
		return CustomConnection{}, fmt.Errorf("store: upsert custom connection: %w", err)
	}
	entry.CreatedAt = createdAt
	entry.UpdatedAt = now
	entry.KeySet = false
	return entry, nil
}

// DeleteCustomConnection removes the connection and all of its models in one
// transaction, returning the removed model ids so the caller can clear their
// secrets. found=false means the connection did not exist.
func (s *Store) DeleteCustomConnection(id string) (modelIDs []string, found bool, err error) {
	if s == nil || s.db == nil {
		return nil, false, fmt.Errorf("store: delete custom connection: store is not open")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, false, fmt.Errorf("store: delete custom connection: %w: id is required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("store: delete custom connection: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `SELECT id FROM custom_models WHERE connection_id = ?`, id)
	if err != nil {
		return nil, false, fmt.Errorf("store: delete custom connection: %w", err)
	}
	for rows.Next() {
		var modelID string
		if err := rows.Scan(&modelID); err != nil {
			rows.Close()
			return nil, false, fmt.Errorf("store: delete custom connection: %w", err)
		}
		modelIDs = append(modelIDs, modelID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("store: delete custom connection: %w", err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM custom_connections WHERE id = ?`, id)
	if err != nil {
		return nil, false, fmt.Errorf("store: delete custom connection: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, false, fmt.Errorf("store: delete custom connection: %w", err)
	}
	if affected == 0 {
		return nil, false, nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM custom_models WHERE connection_id = ?`, id); err != nil {
		return nil, false, fmt.Errorf("store: delete custom connection: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("store: delete custom connection: %w", err)
	}
	return modelIDs, true, nil
}

// newCustomConnectionID returns "cc-" plus 12 random hex characters.
func newCustomConnectionID() (string, error) {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("store: custom connection id: %w", err)
	}
	return "cc-" + hex.EncodeToString(buf[:]), nil
}
