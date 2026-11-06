package store

// Read-only queries behind the /me self-service page (P14, D-59). They are
// deliberately narrow: one key's counters and request metadata only, never
// request/response bodies or any other key's rows.

import (
	"database/sql"
	"fmt"
	"strings"
)

// KeyUsageRange sums a key's day buckets in [fromDay, toDay] (inclusive).
func (s *Store) KeyUsageRange(keyID, fromDay, toDay string) (ClientKeyUsage, error) {
	if s == nil || s.db == nil {
		return ClientKeyUsage{}, fmt.Errorf("store: key usage range: store is not open")
	}
	keyID = strings.TrimSpace(keyID)
	if keyID == "" || fromDay == "" || toDay == "" {
		return ClientKeyUsage{}, fmt.Errorf("store: key usage range: %w: key id and days are required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	var (
		out     ClientKeyUsage
		credits sql.NullFloat64
	)
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(requests), 0),
		COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0),
		COALESCE(SUM(cached_tokens), 0), SUM(credits)
		FROM client_key_usage WHERE key_id = ? AND day >= ? AND day <= ?`,
		keyID, fromDay, toDay).Scan(&out.Requests, &out.PromptTokens,
		&out.CompletionTokens, &out.CachedTokens, &credits); err != nil {
		return ClientKeyUsage{}, fmt.Errorf("store: key usage range: %w", err)
	}
	if credits.Valid {
		v := credits.Float64
		out.Credits = &v
	}
	return out, nil
}

// KeyModelUsageRow is one model's share of a key's traffic.
type KeyModelUsageRow struct {
	Model            string
	Requests         int64
	PromptTokens     int64
	CompletionTokens int64
}

// KeyModelUsage groups a key's request logs by served model inside the day
// window, newest volume first.
func (s *Store) KeyModelUsage(keyID, fromDay, toDay string, limit int) ([]KeyModelUsageRow, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: key model usage: store is not open")
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT
		COALESCE(NULLIF(logical_model, ''), COALESCE(model, '')) AS served,
		COUNT(*), COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0)
		FROM api_logs WHERE key_id = ? AND substr(ts, 1, 10) >= ? AND substr(ts, 1, 10) <= ?
		GROUP BY served ORDER BY COUNT(*) DESC LIMIT ?`, keyID, fromDay, toDay, limit)
	if err != nil {
		return nil, fmt.Errorf("store: key model usage: %w", err)
	}
	defer rows.Close()
	var out []KeyModelUsageRow
	for rows.Next() {
		var row KeyModelUsageRow
		if err := rows.Scan(&row.Model, &row.Requests, &row.PromptTokens, &row.CompletionTokens); err != nil {
			return nil, fmt.Errorf("store: key model usage: scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// MeLogRow is one request as the self-service page may see it: no bodies, no
// conversation ids, no account credentials.
type MeLogRow struct {
	TS               string
	Model            string
	Source           string
	Status           int
	LatencyMS        int64
	PromptTokens     int64
	CompletionTokens int64
}

// RecentKeyLogs returns the newest requests of one key (metadata only).
func (s *Store) RecentKeyLogs(keyID string, limit int) ([]MeLogRow, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: recent key logs: store is not open")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT COALESCE(ts, ''),
		COALESCE(NULLIF(logical_model, ''), COALESCE(model, '')), COALESCE(source, ''),
		COALESCE(status, 0), COALESCE(latency_ms, 0),
		COALESCE(prompt_tokens, 0), COALESCE(completion_tokens, 0)
		FROM api_logs WHERE key_id = ? ORDER BY ts DESC LIMIT ?`, keyID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: recent key logs: %w", err)
	}
	defer rows.Close()
	out := make([]MeLogRow, 0, limit)
	for rows.Next() {
		var row MeLogRow
		if err := rows.Scan(&row.TS, &row.Model, &row.Source, &row.Status,
			&row.LatencyMS, &row.PromptTokens, &row.CompletionTokens); err != nil {
			return nil, fmt.Errorf("store: recent key logs: scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
