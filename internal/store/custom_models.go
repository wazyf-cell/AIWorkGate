package store

// User-defined OpenAI-compatible upstream models (Settings → 自定义模型).
//
// Ported from A (TraeWorkAssistant, MIT, 朱天伟) src-tauri/src/api_server/
// custom_models.rs:23-54 (the per-entry shape), 118-181 (upsert / remove /
// find_enabled / chat_url semantics). The rules carried over from A:
//
//   - `name` is the model id clients request and therefore the routing key; it
//     is compared with the canonical fold (trim + lower-case) of
//     internal/gateway.CanonicalModelID, so two entries that differ only in case
//     or padding are one route and must not both exist;
//   - a name may not contain "/" (A routes custom entries by bare name only;
//     "/" would collide with the "<plugin>/<model>" namespace of this host);
//   - the upstream key is never stored in this table. It lives in the DPAPI
//     secret store under CustomModelSecretKey(id), exactly like proxy
//     passwords, so a database copy never contains a credential.
//
// The table itself ships with migration v5; this file only reads and writes it.

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"unicode/utf8"
)

const (
	// MaxCustomModelNameRunes bounds the request model id (A trims but does not
	// cap it; the host keeps a finite id because it is also a /v1/models entry).
	MaxCustomModelNameRunes = 64
	// MaxCustomModelUpstreamRunes bounds the upstream model name.
	MaxCustomModelUpstreamRunes = 128
	// MaxCustomModelBaseURLRunes bounds the base URL.
	MaxCustomModelBaseURLRunes = 2048
	// MaxCustomModelTokens bounds the configurable context/output limits.
	MaxCustomModelTokens = 10_000_000
	// DefaultCustomInputTypes keeps pre-existing entries behaving as before.
	DefaultCustomInputTypes = `["text","image"]`
)

var allowedInputTypes = map[string]bool{"text": true, "image": true}

var allowedReasoningTiers = map[string]bool{
	"auto": true, "off": true, "minimal": true, "low": true,
	"medium": true, "high": true, "xhigh": true, "max": true,
}

// ReasoningTiers is the canonical order the UI presents.
var ReasoningTiers = []string{"off", "minimal", "low", "medium", "high", "xhigh", "max", "auto"}

// normalizeInputTypes dedupes and validates input modalities. An empty list
// means "not configured" and defaults to text+image for backward compatibility.
func normalizeInputTypes(in []string) ([]string, error) {
	if len(in) == 0 {
		return []string{"text", "image"}, nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, raw := range in {
		value := strings.ToLower(strings.TrimSpace(raw))
		if value == "" || seen[value] {
			continue
		}
		if !allowedInputTypes[value] {
			return nil, fmt.Errorf("store: custom model: %w: 输入类型只能是 text 或 image，收到 %q",
				ErrInvalidArgument, raw)
		}
		seen[value] = true
		out = append(out, value)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("store: custom model: %w: 至少保留一种输入类型", ErrInvalidArgument)
	}
	sortInputTypes(out)
	return out, nil
}

func sortInputTypes(types []string) {
	for i := 1; i < len(types); i++ {
		for j := i; j > 0 && types[j] == "text" && types[j-1] != "text"; j-- {
			types[j], types[j-1] = types[j-1], types[j]
		}
	}
}

// normalizeReasoningTiers dedupes and validates the tiers a model supports.
// The canonical order is preserved so the API and UI agree.
func normalizeReasoningTiers(in []string) ([]string, error) {
	if len(in) == 0 {
		return []string{}, nil
	}
	seen := map[string]bool{}
	for _, raw := range in {
		value := strings.ToLower(strings.TrimSpace(raw))
		if value == "" {
			continue
		}
		if !allowedReasoningTiers[value] {
			return nil, fmt.Errorf("store: custom model: %w: 不支持的推理档位 %q", ErrInvalidArgument, raw)
		}
		seen[value] = true
	}
	out := make([]string, 0, len(seen))
	for _, tier := range ReasoningTiers {
		if seen[tier] {
			out = append(out, tier)
		}
	}
	return out, nil
}

// ParseCustomInputTypes reads the stored JSON column. A malformed value falls
// back to the backward-compatible default instead of failing every listing.
func ParseCustomInputTypes(raw string) []string {
	var out []string
	if strings.TrimSpace(raw) != "" && json.Unmarshal([]byte(raw), &out) == nil {
		if normalized, err := normalizeInputTypes(out); err == nil {
			return normalized
		}
	}
	return []string{"text", "image"}
}

// ParseCustomReasoningTiers reads the stored JSON column.
func ParseCustomReasoningTiers(raw string) []string {
	var out []string
	if strings.TrimSpace(raw) != "" && json.Unmarshal([]byte(raw), &out) == nil {
		if normalized, err := normalizeReasoningTiers(out); err == nil {
			return normalized
		}
	}
	return []string{}
}

// ErrCustomModelConflict reports a name that canonicalises to an existing
// entry. The admin API maps it to HTTP 409.
var ErrCustomModelConflict = errors.New("store: custom model name conflict")

// CustomModel is one user-defined model. KeySet is filled by the host (the
// secret store is not reachable from here) and is the only key state exposed
// to clients; Conflict is likewise an admin-only annotation.
type CustomModel struct {
	ID           string `json:"id"`
	ConnectionID string `json:"connection_id"`
	Name         string `json:"name"`
	BaseURL      string `json:"base_url"`
	Protocol     string `json:"protocol,omitempty"`
	// ConnectionEnabled is the owning connection's switch. A model whose
	// connection is disabled is not routed even if the model itself is enabled.
	ConnectionEnabled bool   `json:"connection_enabled"`
	UpstreamModel     string `json:"upstream_model"`
	Enabled           bool   `json:"enabled"`
	CreatedAt         int64  `json:"created_at"`
	UpdatedAt         int64  `json:"updated_at"`
	// ContextWindow and MaxOutputTokens are 0 when the user did not configure
	// a limit. InputTypes defaults to text+image to preserve the behaviour of
	// entries created before these fields existed.
	ContextWindow    int      `json:"context_window"`
	MaxOutputTokens  int      `json:"max_output_tokens"`
	InputTypes       []string `json:"input_types"`
	ReasoningTiers   []string `json:"reasoning_tiers"`
	DefaultReasoning string   `json:"default_reasoning"`
	// KeySet reports whether an upstream API key is stored. The key itself is
	// never read into this struct, logged or returned.
	KeySet bool `json:"key_set"`
	// Conflict is set by the admin list when a plugin model or logical binding
	// already serves the same id; such a custom model is unpublished and
	// unroutable (the existing source wins).
	Conflict bool `json:"conflict,omitempty"`
}

// CustomModelSecretKey names the DPAPI extra that holds one upstream key.
// An empty value clears it (secrets.SetExtra semantics).
func CustomModelSecretKey(id string) string {
	return "custom_model." + strings.TrimSpace(id) + ".api_key"
}

// CanonicalCustomModelName is the uniqueness key of a custom model name. It
// mirrors gateway.CanonicalModelID (trim + lower-case) without importing the
// gateway package, which itself imports this one.
func CanonicalCustomModelName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// ValidateCustomModelName applies the request-id rules: non-empty, at most 64
// runes and no "/" (the plugin namespace).
func ValidateCustomModelName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("store: custom model: %w: 模型名称不能为空", ErrInvalidArgument)
	}
	if utf8.RuneCountInString(name) > MaxCustomModelNameRunes {
		return fmt.Errorf("store: custom model: %w: 模型名称最多 %d 个字符", ErrInvalidArgument, MaxCustomModelNameRunes)
	}
	if strings.Contains(name, "/") {
		return fmt.Errorf("store: custom model: %w: 模型名称不能包含 “/”（该前缀保留给插件）", ErrInvalidArgument)
	}
	return nil
}

// ValidateCustomModelBaseURL requires an absolute http(s) URL without query or
// fragment. Plain http is only accepted for a loopback host so a key is never
// sent in clear text across the network.
func ValidateCustomModelBaseURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("store: custom model: %w: API 地址不能为空", ErrInvalidArgument)
	}
	if utf8.RuneCountInString(raw) > MaxCustomModelBaseURLRunes {
		return fmt.Errorf("store: custom model: %w: API 地址过长", ErrInvalidArgument)
	}
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" {
		return fmt.Errorf("store: custom model: %w: API 地址必须是完整的 http(s) 地址", ErrInvalidArgument)
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return fmt.Errorf("store: custom model: %w: API 地址必须以 http:// 或 https:// 开头", ErrInvalidArgument)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("store: custom model: %w: API 地址不能包含查询参数或片段", ErrInvalidArgument)
	}
	if parsed.Scheme == "http" && !isLoopbackHost(parsed.Hostname()) {
		return fmt.Errorf("store: custom model: %w: 远程 API 地址必须使用 https（本机回环地址可用 http）", ErrInvalidArgument)
	}
	return nil
}

// isLoopbackHost reports localhost and any loopback IP literal.
func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// normalize trims every string, drops a trailing "/" from the base URL and
// validates the entry. IDs and timestamps are managed by the store.
func (m CustomModel) normalize() (CustomModel, error) {
	out := m
	out.ID = strings.TrimSpace(out.ID)
	out.ConnectionID = strings.TrimSpace(out.ConnectionID)
	out.Name = strings.TrimSpace(out.Name)
	out.BaseURL = strings.TrimRight(strings.TrimSpace(out.BaseURL), "/")
	out.UpstreamModel = strings.TrimSpace(out.UpstreamModel)
	out.Protocol, _ = NormalizeCustomProtocol(out.Protocol)
	if err := ValidateCustomModelName(out.Name); err != nil {
		return CustomModel{}, err
	}
	if err := ValidateCustomModelBaseURL(out.BaseURL); err != nil {
		return CustomModel{}, err
	}
	if out.UpstreamModel == "" {
		return CustomModel{}, fmt.Errorf("store: custom model: %w: 上游模型名不能为空", ErrInvalidArgument)
	}
	if utf8.RuneCountInString(out.UpstreamModel) > MaxCustomModelUpstreamRunes {
		return CustomModel{}, fmt.Errorf("store: custom model: %w: 上游模型名最多 %d 个字符",
			ErrInvalidArgument, MaxCustomModelUpstreamRunes)
	}
	if out.ContextWindow < 0 || out.ContextWindow > MaxCustomModelTokens {
		return CustomModel{}, fmt.Errorf("store: custom model: %w: 上下文长度必须在 0 到 %d 之间",
			ErrInvalidArgument, MaxCustomModelTokens)
	}
	if out.MaxOutputTokens < 0 || out.MaxOutputTokens > MaxCustomModelTokens {
		return CustomModel{}, fmt.Errorf("store: custom model: %w: 最大输出 token 必须在 0 到 %d 之间",
			ErrInvalidArgument, MaxCustomModelTokens)
	}
	types, err := normalizeInputTypes(out.InputTypes)
	if err != nil {
		return CustomModel{}, err
	}
	out.InputTypes = types
	tiers, err := normalizeReasoningTiers(out.ReasoningTiers)
	if err != nil {
		return CustomModel{}, err
	}
	out.ReasoningTiers = tiers
	out.DefaultReasoning = strings.ToLower(strings.TrimSpace(out.DefaultReasoning))
	if out.DefaultReasoning != "" {
		if len(tiers) == 0 {
			return CustomModel{}, fmt.Errorf("store: custom model: %w: 没有勾选推理档位时不能设置默认档",
				ErrInvalidArgument)
		}
		found := false
		for _, tier := range tiers {
			if tier == out.DefaultReasoning {
				found = true
				break
			}
		}
		if !found {
			return CustomModel{}, fmt.Errorf("store: custom model: %w: 默认推理档 %q 不在已勾选的档位里",
				ErrInvalidArgument, out.DefaultReasoning)
		}
	}
	return out, nil
}

// customModelSelect joins the owning connection so the caller sees its live
// protocol and base URL. Legacy rows without a connection keep their own
// base_url and default to the OpenAI-compatible protocol.
const customModelSelect = `SELECT m.id, m.name, COALESCE(c.base_url, m.base_url), m.upstream_model,
	m.enabled, m.created_at, m.updated_at, COALESCE(m.connection_id, ''), COALESCE(c.protocol, 'openai'),
	COALESCE(c.enabled, 1),
	m.context_window, m.max_output_tokens, m.input_types, m.reasoning_tiers, m.default_reasoning
	FROM custom_models m LEFT JOIN custom_connections c ON c.id = m.connection_id`

// ListCustomModels returns every entry ordered by name. The API key is never
// loaded (KeySet stays false for the caller to fill from the secret store).
func (s *Store) ListCustomModels() ([]CustomModel, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: list custom models: store is not open")
	}
	return s.listCustomModelsWhere(` ORDER BY m.name COLLATE NOCASE, m.id`)
}

// listCustomModelsWhere runs the joined select with an extra clause.
func (s *Store) listCustomModelsWhere(clause string, args ...any) ([]CustomModel, error) {
	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, customModelSelect+clause, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list custom models: %w", err)
	}
	defer rows.Close()

	out := make([]CustomModel, 0, 4)
	for rows.Next() {
		entry, err := scanCustomModel(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list custom models: %w", err)
		}
		out = append(out, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list custom models: %w", err)
	}
	return out, nil
}

// ListCustomModelsByConnection returns the models of one connection.
func (s *Store) ListCustomModelsByConnection(connectionID string) ([]CustomModel, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: list custom models: store is not open")
	}
	return s.listCustomModelsWhere(` WHERE m.connection_id = ? ORDER BY m.name COLLATE NOCASE, m.id`,
		strings.TrimSpace(connectionID))
}

// ListLegacyCustomModels returns rows created before connections existed. The
// host merges them into connections once on startup.
func (s *Store) ListLegacyCustomModels() ([]CustomModel, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: list custom models: store is not open")
	}
	return s.listCustomModelsWhere(` WHERE COALESCE(m.connection_id, '') = '' ORDER BY m.name COLLATE NOCASE, m.id`)
}

// AttachCustomModelConnection links a legacy row to its merged connection.
func (s *Store) AttachCustomModelConnection(modelID, connectionID string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: attach custom model: store is not open")
	}
	ctx, cancel := s.ctx()
	defer cancel()
	if _, err := s.db.ExecContext(ctx,
		`UPDATE custom_models SET connection_id = ?, updated_at = ? WHERE id = ?`,
		strings.TrimSpace(connectionID), nowUnix(), strings.TrimSpace(modelID)); err != nil {
		return fmt.Errorf("store: attach custom model: %w", err)
	}
	return nil
}

// GetCustomModel returns one entry by id. found=false means the row is absent.
func (s *Store) GetCustomModel(id string) (CustomModel, bool, error) {
	if s == nil || s.db == nil {
		return CustomModel{}, false, fmt.Errorf("store: get custom model: store is not open")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return CustomModel{}, false, fmt.Errorf("store: get custom model: %w: id is required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	entry, err := scanCustomModel(s.db.QueryRowContext(ctx, customModelSelect+` WHERE m.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return CustomModel{}, false, nil
	}
	if err != nil {
		return CustomModel{}, false, fmt.Errorf("store: get custom model: %w", err)
	}
	return entry, true, nil
}

// UpsertCustomModel validates and writes one entry. An empty ID creates a new
// row with a generated "cm-<12hex>" id (A custom_models.rs:131-149); otherwise
// the existing row keeps its created_at. The canonical name must be unique
// across the table, excluding the row being written.
func (s *Store) UpsertCustomModel(m CustomModel) (CustomModel, error) {
	if s == nil || s.db == nil {
		return CustomModel{}, fmt.Errorf("store: upsert custom model: store is not open")
	}
	entry, err := m.normalize()
	if err != nil {
		return CustomModel{}, err
	}

	ctx, cancel := s.ctx()
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CustomModel{}, fmt.Errorf("store: upsert custom model: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Uniqueness is compared in Go because SQLite lower() is ASCII-only; the
	// table is tiny and the write path is user-triggered.
	rows, err := tx.QueryContext(ctx, `SELECT id, name, connection_id, upstream_model
		FROM custom_models WHERE id <> ?`, entry.ID)
	if err != nil {
		return CustomModel{}, fmt.Errorf("store: upsert custom model: %w", err)
	}
	canonical := CanonicalCustomModelName(entry.Name)
	for rows.Next() {
		var (
			otherID       string
			otherName     string
			otherConn     sql.NullString
			otherUpstream string
		)
		if err := rows.Scan(&otherID, &otherName, &otherConn, &otherUpstream); err != nil {
			rows.Close()
			return CustomModel{}, fmt.Errorf("store: upsert custom model: %w", err)
		}
		if CanonicalCustomModelName(otherName) == canonical {
			rows.Close()
			return CustomModel{}, fmt.Errorf("%w: 模型名称 %q 与既有条目 %q 重复（模型名不区分大小写）",
				ErrCustomModelConflict, entry.Name, otherName)
		}
		if entry.ConnectionID != "" && otherConn.String == entry.ConnectionID &&
			strings.TrimSpace(otherUpstream) == entry.UpstreamModel {
			rows.Close()
			return CustomModel{}, fmt.Errorf("%w: 上游模型 %q 已经在这个接入里了",
				ErrCustomModelConflict, entry.UpstreamModel)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return CustomModel{}, fmt.Errorf("store: upsert custom model: %w", err)
	}
	rows.Close()

	now := nowUnix()
	createdAt := now
	if entry.ID != "" {
		var existing int64
		err := tx.QueryRowContext(ctx, `SELECT created_at FROM custom_models WHERE id = ?`, entry.ID).Scan(&existing)
		switch {
		case err == nil:
			createdAt = existing
		case errors.Is(err, sql.ErrNoRows):
			// A put with an explicit unknown id inserts it; the admin API only
			// sends ids it just read, so this is the "row vanished" case.
		default:
			return CustomModel{}, fmt.Errorf("store: upsert custom model: %w", err)
		}
	} else {
		id, err := newCustomModelID()
		if err != nil {
			return CustomModel{}, err
		}
		entry.ID = id
	}

	inputJSON, err := json.Marshal(entry.InputTypes)
	if err != nil {
		return CustomModel{}, fmt.Errorf("store: upsert custom model: %w", err)
	}
	tiersJSON, err := json.Marshal(entry.ReasoningTiers)
	if err != nil {
		return CustomModel{}, fmt.Errorf("store: upsert custom model: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO custom_models
		(id, connection_id, name, base_url, upstream_model, enabled,
		 context_window, max_output_tokens, input_types, reasoning_tiers,
		 default_reasoning, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			connection_id = excluded.connection_id,
			name = excluded.name,
			base_url = excluded.base_url,
			upstream_model = excluded.upstream_model,
			enabled = excluded.enabled,
			context_window = excluded.context_window,
			max_output_tokens = excluded.max_output_tokens,
			input_types = excluded.input_types,
			reasoning_tiers = excluded.reasoning_tiers,
			default_reasoning = excluded.default_reasoning,
			updated_at = excluded.updated_at`,
		entry.ID, entry.ConnectionID, entry.Name, entry.BaseURL, entry.UpstreamModel,
		boolInt(entry.Enabled), entry.ContextWindow, entry.MaxOutputTokens,
		string(inputJSON), string(tiersJSON), entry.DefaultReasoning, createdAt, now); err != nil {
		return CustomModel{}, fmt.Errorf("store: upsert custom model: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return CustomModel{}, fmt.Errorf("store: upsert custom model: %w", err)
	}

	entry.CreatedAt = createdAt
	entry.UpdatedAt = now
	entry.KeySet = false
	return entry, nil
}

// DeleteCustomModel removes one row and reports whether it existed. The caller
// (the host) also clears the DPAPI secret.
func (s *Store) DeleteCustomModel(id string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("store: delete custom model: store is not open")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return false, fmt.Errorf("store: delete custom model: %w: id is required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	result, err := s.db.ExecContext(ctx, `DELETE FROM custom_models WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("store: delete custom model: %w", err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: delete custom model: %w", err)
	}
	return removed > 0, nil
}

// scanCustomModel reads one row; rowScanner is declared in proxies.go and is
// satisfied by both *sql.Row and *sql.Rows.
func scanCustomModel(row rowScanner) (CustomModel, error) {
	var (
		entry       CustomModel
		enabled     int64
		connEnabled int64
		inputJSON   string
		tiersJSON   string
		protocol    string
	)
	if err := row.Scan(&entry.ID, &entry.Name, &entry.BaseURL, &entry.UpstreamModel,
		&enabled, &entry.CreatedAt, &entry.UpdatedAt, &entry.ConnectionID, &protocol,
		&connEnabled, &entry.ContextWindow, &entry.MaxOutputTokens, &inputJSON, &tiersJSON,
		&entry.DefaultReasoning); err != nil {
		return CustomModel{}, err
	}
	entry.Enabled = enabled != 0
	entry.ConnectionEnabled = connEnabled != 0
	entry.Protocol = protocol
	entry.InputTypes = ParseCustomInputTypes(inputJSON)
	entry.ReasoningTiers = ParseCustomReasoningTiers(tiersJSON)
	return entry, nil
}

// newCustomModelID returns "cm-" plus 12 random hex characters, the id shape
// of A custom_models.rs:141-148.
func newCustomModelID() (string, error) {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("store: custom model id: %w", err)
	}
	return "cm-" + hex.EncodeToString(buf[:]), nil
}
