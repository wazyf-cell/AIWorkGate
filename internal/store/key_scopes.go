package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"aiworkgate/internal/model"
)

// Client key scope and quota storage (plan 2026-10-21 §1).
//
// Ported from A's ApiKeyEntry fields (TraeWorkAssistant, MIT, 朱天伟;
// src-tauri/src/api_server/api_keys.rs:55-78,254-293): a key may be limited to a
// model list, bound to one resource pool and capped by a per-day request
// counter. A keeps the counters inside its JSON key file; here the scope lives
// on client_keys and the per-day counters in client_key_usage.
//
// Wiring shape:
//
//	client_keys.model_scope  TEXT     JSON array; NULL/'' = every model
//	client_keys.bind_pool    TEXT     plugin id; '' = every pool
//	client_keys.daily_quota  INTEGER  0 = unlimited
//	client_key_usage(day,key_id,requests,prompt_tokens,completion_tokens,
//	                 cached_tokens,credits)  PK(day,key_id)

const (
	// MaxClientKeyModelScope bounds the model_scope list of one key.
	MaxClientKeyModelScope = 64
	// MaxClientKeyScopeRunes bounds one model_scope entry.
	MaxClientKeyScopeRunes = 128
	// MaxClientKeyDailyQuota bounds daily_quota (0 keeps meaning unlimited).
	MaxClientKeyDailyQuota = 1_000_000
	// MaxClientKeyBindPoolRunes bounds bind_pool; real plugin ids are far
	// shorter, the cap only stops a runaway request from being stored.
	MaxClientKeyBindPoolRunes = 64
)

// ClientKeyScope is the scope/quota triple of one client key.
type ClientKeyScope struct {
	ModelScope []string
	BindPool   string
	DailyQuota int
}

// Empty reports whether the scope restricts nothing.
func (s ClientKeyScope) Empty() bool {
	return len(normalizeClientKeyScope(s.ModelScope)) == 0 &&
		strings.TrimSpace(s.BindPool) == "" && s.DailyQuota == 0
}

// Validate rejects a scope the schema/UI cannot represent. The webui validates
// the same bounds so a rejected request never reaches the database.
func (s ClientKeyScope) Validate() error {
	scope := normalizeClientKeyScope(s.ModelScope)
	if len(scope) > MaxClientKeyModelScope {
		return fmt.Errorf("store: client key scope: %w: model_scope accepts at most %d entries",
			ErrInvalidArgument, MaxClientKeyModelScope)
	}
	for _, entry := range scope {
		if utf8.RuneCountInString(entry) > MaxClientKeyScopeRunes {
			return fmt.Errorf("store: client key scope: %w: model_scope entries must be at most %d characters",
				ErrInvalidArgument, MaxClientKeyScopeRunes)
		}
	}
	if len(strings.TrimSpace(s.BindPool)) > MaxClientKeyBindPoolRunes {
		return fmt.Errorf("store: client key scope: %w: bind_pool must be at most %d characters",
			ErrInvalidArgument, MaxClientKeyBindPoolRunes)
	}
	if s.DailyQuota < 0 || s.DailyQuota > MaxClientKeyDailyQuota {
		return fmt.Errorf("store: client key scope: %w: daily_quota must be between 0 and %d",
			ErrInvalidArgument, MaxClientKeyDailyQuota)
	}
	return nil
}

// ClientKeyUsage is one day bucket of one key.
type ClientKeyUsage struct {
	Day              string
	KeyID            string
	Requests         int
	PromptTokens     int
	CompletionTokens int
	CachedTokens     int
	Credits          *float64
}

// ClientKeyUsageDay is the day bucket of a local calendar date. Every caller
// (gateway quota gate, usage recorder, list join) must use this helper so the
// counter and the Retry-After reset moment agree.
func ClientKeyUsageDay(t time.Time) string {
	return t.In(time.Local).Format("2006-01-02")
}

// ClientKeyQuotaLeft reports what the UI shows next to a key: -1 when the key
// is unlimited, otherwise the remaining requests of the day (never negative).
func ClientKeyQuotaLeft(quota, used int) int {
	if quota <= 0 {
		return -1
	}
	left := quota - used
	if left < 0 {
		left = 0
	}
	return left
}

// GetClientKey returns one key including its scope and today's usage.
func (s *Store) GetClientKey(id string) (model.ClientKey, bool, error) {
	if s == nil || s.db == nil {
		return model.ClientKey{}, false, fmt.Errorf("store: get client key: store is not open")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return model.ClientKey{}, false, fmt.Errorf("store: get client key: %w: id is required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	row := s.db.QueryRowContext(ctx, `SELECT k.id, COALESCE(k.name, ''), COALESCE(k.prefix, ''),
		COALESCE(k.enabled, 0), COALESCE(k.created_at, 0), COALESCE(k.last_used_at, 0),
		COALESCE(k.model_scope, ''), COALESCE(k.bind_pool, ''), COALESCE(k.daily_quota, 0),
		COALESCE(u.requests, 0)
		FROM client_keys k
		LEFT JOIN client_key_usage u ON u.key_id = k.id AND u.day = ?
		WHERE k.id = ?`, ClientKeyUsageDay(time.Now()), id)
	key, err := scanClientKeyWithUsage(row)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return model.ClientKey{}, false, nil
	case err != nil:
		return model.ClientKey{}, false, fmt.Errorf("store: get client key: %w", err)
	}
	return key, true, nil
}

// scanClientKeyWithUsage reads the column order shared by ListClientKeys and
// GetClientKey.
func scanClientKeyWithUsage(row interface{ Scan(...any) error }) (model.ClientKey, error) {
	var (
		key       model.ClientKey
		enabled   int64
		scopeRaw  string
		dailyQ    int64
		usedToday int64
	)
	if err := row.Scan(&key.ID, &key.Name, &key.Prefix, &enabled, &key.CreatedAt, &key.LastUsedAt,
		&scopeRaw, &key.BindPool, &dailyQ, &usedToday); err != nil {
		return model.ClientKey{}, err
	}
	key.Enabled = enabled != 0
	key.ModelScope = decodeClientKeyScope(scopeRaw)
	key.DailyQuota = int(dailyQ)
	key.UsedToday = int(usedToday)
	key.QuotaLeft = ClientKeyQuotaLeft(key.DailyQuota, key.UsedToday)
	return key, nil
}

// UpsertClientKeyScoped is UpsertClientKey plus the scope/quota triple, in one
// statement so a create never leaves a scoped id without its limits.
func (s *Store) UpsertClientKeyScoped(id, name, prefix, hash string, enabled bool, createdAt int64, scope ClientKeyScope) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: upsert client key: store is not open")
	}
	if id == "" || hash == "" {
		return fmt.Errorf("store: upsert client key: %w: id and hash are required", ErrInvalidArgument)
	}
	if err := scope.Validate(); err != nil {
		return err
	}
	scopeJSON, err := encodeClientKeyScope(scope.ModelScope)
	if err != nil {
		return fmt.Errorf("store: upsert client key: %w", err)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	_, err = s.db.ExecContext(ctx, `INSERT INTO client_keys
		(id, name, prefix, hash, enabled, created_at, last_used_at, model_scope, bind_pool, daily_quota)
		VALUES (?, ?, ?, ?, ?, COALESCE(NULLIF(?, 0), CAST(strftime('%s','now') AS INTEGER)), 0, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			prefix = excluded.prefix,
			hash = excluded.hash,
			enabled = excluded.enabled,
			model_scope = excluded.model_scope,
			bind_pool = excluded.bind_pool,
			daily_quota = excluded.daily_quota,
			created_at = CASE WHEN ? <> 0 THEN ? ELSE client_keys.created_at END`,
		id, name, prefix, hash, boolInt(enabled), createdAt, scopeJSON,
		strings.TrimSpace(scope.BindPool), scope.DailyQuota, createdAt, createdAt)
	if err != nil {
		return fmt.Errorf("store: upsert client key: %w", err)
	}
	return nil
}

// SetClientKeyScope rewrites only the scope/quota columns.
func (s *Store) SetClientKeyScope(id string, scope ClientKeyScope) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: set client key scope: store is not open")
	}
	if err := scope.Validate(); err != nil {
		return err
	}
	scopeJSON, err := encodeClientKeyScope(scope.ModelScope)
	if err != nil {
		return fmt.Errorf("store: set client key scope: %w", err)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	res, err := s.db.ExecContext(ctx, `UPDATE client_keys
		SET model_scope = ?, bind_pool = ?, daily_quota = ? WHERE id = ?`,
		scopeJSON, strings.TrimSpace(scope.BindPool), scope.DailyQuota, id)
	if err != nil {
		return fmt.Errorf("store: set client key scope: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("store: set client key scope: %w: %s", ErrNotFound, id)
	}
	return nil
}

// UpdateClientKey edits the editable fields of a key (PUT /api/keys/{id}).
func (s *Store) UpdateClientKey(id, name string, enabled bool, scope ClientKeyScope) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: update client key: store is not open")
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("store: update client key: %w: id is required", ErrInvalidArgument)
	}
	if err := scope.Validate(); err != nil {
		return err
	}
	scopeJSON, err := encodeClientKeyScope(scope.ModelScope)
	if err != nil {
		return fmt.Errorf("store: update client key: %w", err)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	res, err := s.db.ExecContext(ctx, `UPDATE client_keys
		SET name = ?, enabled = ?, model_scope = ?, bind_pool = ?, daily_quota = ?
		WHERE id = ?`,
		strings.TrimSpace(name), boolInt(enabled), scopeJSON,
		strings.TrimSpace(scope.BindPool), scope.DailyQuota, id)
	if err != nil {
		return fmt.Errorf("store: update client key: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("store: update client key: %w: %s", ErrNotFound, id)
	}
	return nil
}

// IncrementKeyUsage adds one finished request to a key's day bucket. requests
// is always added (A counts successes and failures alike,
// api_keys.rs:327-358); tokens are only meaningful when the upstream reported
// usage. credits stay NULL until a positive amount is reported, so a key that
// never returned a cost does not look free by accident.
func (s *Store) IncrementKeyUsage(keyID, day string, requests, prompt, completion, cached int, credits *float64) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: increment key usage: store is not open")
	}
	keyID = strings.TrimSpace(keyID)
	if keyID == "" || strings.TrimSpace(day) == "" {
		return fmt.Errorf("store: increment key usage: %w: key id and day are required", ErrInvalidArgument)
	}
	if _, err := time.Parse("2006-01-02", day); err != nil {
		return fmt.Errorf("store: increment key usage: %w: day must be YYYY-MM-DD", ErrInvalidArgument)
	}
	if requests < 0 || prompt < 0 || completion < 0 || cached < 0 {
		return fmt.Errorf("store: increment key usage: %w: counters must not be negative", ErrInvalidArgument)
	}
	var credit any
	if credits != nil {
		if *credits < 0 {
			return fmt.Errorf("store: increment key usage: %w: credits must not be negative", ErrInvalidArgument)
		}
		credit = *credits
	}
	ctx, cancel := s.ctx()
	defer cancel()
	_, err := s.db.ExecContext(ctx, `INSERT INTO client_key_usage
		(day, key_id, requests, prompt_tokens, completion_tokens, cached_tokens, credits)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(day, key_id) DO UPDATE SET
			requests = client_key_usage.requests + excluded.requests,
			prompt_tokens = client_key_usage.prompt_tokens + excluded.prompt_tokens,
			completion_tokens = client_key_usage.completion_tokens + excluded.completion_tokens,
			cached_tokens = client_key_usage.cached_tokens + excluded.cached_tokens,
			credits = CASE WHEN excluded.credits IS NULL THEN client_key_usage.credits
				ELSE COALESCE(client_key_usage.credits, 0) + excluded.credits END`,
		day, keyID, requests, prompt, completion, cached, credit)
	if err != nil {
		return fmt.Errorf("store: increment key usage: %w", err)
	}
	return nil
}

// ReserveKeyUsage atomically consumes one request slot of a key's day bucket
// and reports whether the reservation was granted.
//
// Ported from A (TraeWorkAssistant, MIT, 朱天伟): api_server/auth.rs:74-91
// verifies the key and bumps used_today under one lock inside authentication
// (api_keys.rs:327-390), so N concurrent requests can never overshoot
// daily_limit. This host keeps the counter in SQLite, so the single conditional
// upsert below is that lock: the WHERE clause on DO UPDATE makes the increment
// and the quota check one statement.
//
// used is only meaningful when reserved is false (the current counter, which is
// the quota at that point). quota <= 0 means unlimited and always reserves.
func (s *Store) ReserveKeyUsage(keyID, day string, quota int) (reserved bool, used int, err error) {
	if s == nil || s.db == nil {
		return false, 0, fmt.Errorf("store: reserve key usage: store is not open")
	}
	keyID = strings.TrimSpace(keyID)
	if keyID == "" || strings.TrimSpace(day) == "" {
		return false, 0, fmt.Errorf("store: reserve key usage: %w: key id and day are required", ErrInvalidArgument)
	}
	if _, err := time.Parse("2006-01-02", day); err != nil {
		return false, 0, fmt.Errorf("store: reserve key usage: %w: day must be YYYY-MM-DD", ErrInvalidArgument)
	}
	if quota < 0 || quota > MaxClientKeyDailyQuota {
		return false, 0, fmt.Errorf("store: reserve key usage: %w: quota must be between 0 and %d",
			ErrInvalidArgument, MaxClientKeyDailyQuota)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	if quota == 0 {
		// Unlimited: keep counting so the day bucket stays accurate.
		if _, err := s.db.ExecContext(ctx, `INSERT INTO client_key_usage
			(day, key_id, requests, prompt_tokens, completion_tokens, cached_tokens, credits)
			VALUES (?, ?, 1, 0, 0, 0, NULL)
			ON CONFLICT(day, key_id) DO UPDATE SET requests = client_key_usage.requests + 1`,
			day, keyID); err != nil {
			return false, 0, fmt.Errorf("store: reserve key usage: %w", err)
		}
		return true, 0, nil
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO client_key_usage
		(day, key_id, requests, prompt_tokens, completion_tokens, cached_tokens, credits)
		VALUES (?, ?, 1, 0, 0, 0, NULL)
		ON CONFLICT(day, key_id) DO UPDATE SET requests = client_key_usage.requests + 1
		WHERE client_key_usage.requests < ?`,
		day, keyID, quota)
	if err != nil {
		return false, 0, fmt.Errorf("store: reserve key usage: %w", err)
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return false, 0, fmt.Errorf("store: reserve key usage: %w", err)
	}
	if changed == 0 {
		// Over quota: the update was suppressed. Report the counter so the 429
		// message can name it; a read error still means "not reserved".
		current, rerr := s.KeyUsageRequests(keyID, day)
		if rerr != nil {
			current = quota
		}
		return false, current, nil
	}
	return true, 0, nil
}

// ReleaseKeyUsageReservation gives one reserved slot back. A reservation is
// taken at admission (before the model scope is known); a request the key's
// scope rejects never reaches a model, so it must not burn daily quota.
func (s *Store) ReleaseKeyUsageReservation(keyID, day string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: release key usage: store is not open")
	}
	keyID = strings.TrimSpace(keyID)
	if keyID == "" || strings.TrimSpace(day) == "" {
		return fmt.Errorf("store: release key usage: %w: key id and day are required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `UPDATE client_key_usage
		SET requests = requests - 1
		WHERE day = ? AND key_id = ? AND requests > 0`, day, keyID); err != nil {
		return fmt.Errorf("store: release key usage: %w", err)
	}
	return nil
}

// KeyUsageRequests returns the request counter of one key and day (0 when the
// key was never used that day).
func (s *Store) KeyUsageRequests(keyID, day string) (int, error) {
	usage, ok, err := s.KeyUsage(keyID, day)
	if err != nil || !ok {
		return 0, err
	}
	return usage.Requests, nil
}

// KeyUsageToday returns today's request counter of one key.
func (s *Store) KeyUsageToday(keyID string) (int, error) {
	return s.KeyUsageRequests(keyID, ClientKeyUsageDay(time.Now()))
}

// KeyUsage returns the whole day bucket.
func (s *Store) KeyUsage(keyID, day string) (ClientKeyUsage, bool, error) {
	if s == nil || s.db == nil {
		return ClientKeyUsage{}, false, fmt.Errorf("store: key usage: store is not open")
	}
	keyID = strings.TrimSpace(keyID)
	if keyID == "" || strings.TrimSpace(day) == "" {
		return ClientKeyUsage{}, false, fmt.Errorf("store: key usage: %w: key id and day are required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	var (
		usage   ClientKeyUsage
		credits sql.NullFloat64
	)
	err := s.db.QueryRowContext(ctx, `SELECT day, key_id, COALESCE(requests, 0),
		COALESCE(prompt_tokens, 0), COALESCE(completion_tokens, 0), COALESCE(cached_tokens, 0), credits
		FROM client_key_usage WHERE day = ? AND key_id = ?`, day, keyID).
		Scan(&usage.Day, &usage.KeyID, &usage.Requests, &usage.PromptTokens,
			&usage.CompletionTokens, &usage.CachedTokens, &credits)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ClientKeyUsage{}, false, nil
	case err != nil:
		return ClientKeyUsage{}, false, fmt.Errorf("store: key usage: %w", err)
	}
	if credits.Valid {
		value := credits.Float64
		usage.Credits = &value
	}
	return usage, true, nil
}

// normalizeClientKeyScope trims entries, drops empties and de-duplicates while
// keeping the user's order.
func normalizeClientKeyScope(entries []string) []string {
	if len(entries) == 0 {
		return nil
	}
	out := make([]string, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" || seen[entry] {
			continue
		}
		seen[entry] = true
		out = append(out, entry)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// encodeClientKeyScope stores nil/empty as SQL NULL, so "unrestricted" has one
// representation instead of "" and "[]" drifting apart.
func encodeClientKeyScope(entries []string) (any, error) {
	scope := normalizeClientKeyScope(entries)
	if len(scope) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(scope)
	if err != nil {
		return nil, err
	}
	return string(raw), nil
}

// decodeClientKeyScope tolerates NULL, an empty string and a malformed array
// (treated as unrestricted rather than locking the key out).
func decodeClientKeyScope(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return nil
	}
	var scope []string
	if err := json.Unmarshal([]byte(raw), &scope); err != nil {
		return nil
	}
	return normalizeClientKeyScope(scope)
}
