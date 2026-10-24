package store

import (
	"database/sql"
	"fmt"
	"math"
	"sort"
	"time"
)

// Observability queries over raw request logs (plan 2026-10-21 §5).
//
// Ported from A (TraeWorkAssistant, MIT, 朱天伟)
// src-tauri/src/api_server/usage.rs:86-91 (day-level latency buckets),
// usage.rs:200-205 (nearest-rank percentile) and usage.rs:655-663
// (per-day percentile view). A samples in memory with fixed capacities; this
// host recomputes from api_logs on demand, because the rows are already
// retained for the log window and no extra sampler state is needed.

const (
	// DefaultAnalyticsDays matches the admin API defaults when a caller passes
	// a non-positive window.
	DefaultAnalyticsDays = 14
	// MaxAnalyticsDays bounds the window the admin API accepts.
	MaxAnalyticsDays = 90
)

// DayLatency is one local day of latency percentiles. A nil percentile means
// "no sample", which the UI renders as "—"; a real 0 ms stays 0.
type DayLatency struct {
	Day       string `json:"day"`
	Requests  int64  `json:"requests"`
	P50MS     *int64 `json:"p50_ms"`
	P95MS     *int64 `json:"p95_ms"`
	P50TTFBMS *int64 `json:"p50_ttfb_ms"`
	P95TTFBMS *int64 `json:"p95_ttfb_ms"`
}

// KeyUsageRow is one key aggregated over the window. Credits stays nil when
// the upstream never reported a cost, so 0 credits is not confused with
// "unknown".
type KeyUsageRow struct {
	KeyID            string   `json:"key_id"`
	Name             string   `json:"name"`
	Requests         int64    `json:"requests"`
	PromptTokens     int64    `json:"prompt_tokens"`
	CompletionTokens int64    `json:"completion_tokens"`
	CachedTokens     int64    `json:"cached_tokens"`
	Credits          *float64 `json:"credits"`
}

// ModelUsageRow is one model aggregated over the window. It mirrors KeyUsageRow
// so the UI can share its number formatting; Credits stays nil when the
// upstream never reported a cost.
type ModelUsageRow struct {
	Model            string   `json:"model"`
	Requests         int64    `json:"requests"`
	PromptTokens     int64    `json:"prompt_tokens"`
	CompletionTokens int64    `json:"completion_tokens"`
	Credits          *float64 `json:"credits"`
}

// ModelLatencyRow is one model's latency percentiles over the window. Same
// population and nearest-rank rule as LatencyStats; nil means "no sample".
type ModelLatencyRow struct {
	Model     string `json:"model"`
	Requests  int64  `json:"requests"`
	P50MS     *int64 `json:"p50_ms"`
	P95MS     *int64 `json:"p95_ms"`
	P50TTFBMS *int64 `json:"p50_ttfb_ms"`
	P95TTFBMS *int64 `json:"p95_ttfb_ms"`
}

// analyticsWindow returns the inclusive local-day bounds of the last `days`
// days, ending today, plus the normalised day count.
func analyticsWindow(days int, now time.Time) (from, to string, normalized int) {
	if days <= 0 {
		days = DefaultAnalyticsDays
	}
	if days > MaxAnalyticsDays {
		days = MaxAnalyticsDays
	}
	local := now.In(time.Local)
	toDate := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.Local)
	fromDate := toDate.AddDate(0, 0, -(days - 1))
	return fromDate.Format("2006-01-02"), toDate.Format("2006-01-02"), days
}

// LatencyStats returns per-day request counts and latency/TTFB percentiles.
//
// Population: only successful requests (status = 200) count. Total latency is
// taken from every such row; TTFB is only defined for rows whose ttfb_ms > 0
// (streaming replies), so its percentiles may be nil while p50/p95 are set.
// Requests is the status-200 row count of that day.
//
// Percentiles follow A's nearest-rank rule exactly: sort ascending, then index
// ceil(p/100*n)-1 clamped to the last element (usage.rs:200-205). No
// interpolation, so every result is an observed sample.
func (s *Store) LatencyStats(days int) ([]DayLatency, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: latency stats: store is not open")
	}
	from, to, _ := analyticsWindow(days, time.Now())
	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT substr(ts, 1, 10), COALESCE(latency_ms, 0),
		COALESCE(ttfb_ms, 0)
		FROM api_logs
		WHERE COALESCE(status, 0) = 200
		  AND `+dayRangeSQL("ts", from, to), dayRangeArgs(from, to)...)
	if err != nil {
		return nil, fmt.Errorf("store: latency stats: %w", err)
	}
	defer rows.Close()

	type bucket struct {
		latency []int64
		ttfb    []int64
	}
	buckets := make(map[string]*bucket)
	var order []string
	for rows.Next() {
		var (
			day       string
			latencyMS int64
			ttfbMS    int64
		)
		if err := rows.Scan(&day, &latencyMS, &ttfbMS); err != nil {
			return nil, fmt.Errorf("store: latency stats: scan: %w", err)
		}
		b := buckets[day]
		if b == nil {
			b = &bucket{}
			buckets[day] = b
			order = append(order, day)
		}
		b.latency = append(b.latency, latencyMS)
		if ttfbMS > 0 {
			b.ttfb = append(b.ttfb, ttfbMS)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: latency stats: iterate: %w", err)
	}
	sort.Strings(order)

	out := make([]DayLatency, 0, len(order))
	for _, day := range order {
		b := buckets[day]
		out = append(out, DayLatency{
			Day:       day,
			Requests:  int64(len(b.latency)),
			P50MS:     nearestRankPercentile(b.latency, 50),
			P95MS:     nearestRankPercentile(b.latency, 95),
			P50TTFBMS: nearestRankPercentile(b.ttfb, 50),
			P95TTFBMS: nearestRankPercentile(b.ttfb, 95),
		})
	}
	return out, nil
}

// KeyUsageSummary aggregates client_key_usage (written by the key quota path,
// read-only here) per key over the window. Names come from client_keys; a key
// deleted after being used keeps its row and falls back to its id.
func (s *Store) KeyUsageSummary(days int) ([]KeyUsageRow, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: key usage summary: store is not open")
	}
	from, to, _ := analyticsWindow(days, time.Now())
	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT u.key_id, COALESCE(k.name, ''),
		COALESCE(SUM(u.requests), 0), COALESCE(SUM(u.prompt_tokens), 0),
		COALESCE(SUM(u.completion_tokens), 0), COALESCE(SUM(u.cached_tokens), 0),
		SUM(u.credits)
		FROM client_key_usage u
		LEFT JOIN client_keys k ON k.id = u.key_id
		WHERE u.day >= ? AND u.day <= ?
		GROUP BY u.key_id
		ORDER BY SUM(u.requests) DESC, u.key_id ASC`, from, to)
	if err != nil {
		return nil, fmt.Errorf("store: key usage summary: %w", err)
	}
	defer rows.Close()

	out := make([]KeyUsageRow, 0)
	for rows.Next() {
		var (
			row     KeyUsageRow
			credits sql.NullFloat64
		)
		if err := rows.Scan(&row.KeyID, &row.Name, &row.Requests, &row.PromptTokens,
			&row.CompletionTokens, &row.CachedTokens, &credits); err != nil {
			return nil, fmt.Errorf("store: key usage summary: scan: %w", err)
		}
		if row.Name == "" {
			row.Name = row.KeyID
		}
		if credits.Valid {
			value := credits.Float64
			row.Credits = &value
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: key usage summary: iterate: %w", err)
	}
	return out, nil
}

// ModelUsageSummary aggregates usage_daily by model over the window
// (A: CreditsTab.tsx:533-563, usage.rs:143-145). Rows are ordered by request
// count, so the first rows are the consumption ranking. Credits are summed as
// stored: NULL stays NULL rather than becoming a fake 0.
func (s *Store) ModelUsageSummary(days int) ([]ModelUsageRow, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: model usage summary: store is not open")
	}
	from, to, _ := analyticsWindow(days, time.Now())
	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT model,
		COALESCE(SUM(requests), 0), COALESCE(SUM(prompt_tokens), 0),
		COALESCE(SUM(completion_tokens), 0), SUM(credits)
		FROM usage_daily
		WHERE day >= ? AND day <= ?
		GROUP BY model
		ORDER BY SUM(requests) DESC, model ASC`, from, to)
	if err != nil {
		return nil, fmt.Errorf("store: model usage summary: %w", err)
	}
	defer rows.Close()

	out := make([]ModelUsageRow, 0)
	for rows.Next() {
		var (
			row     ModelUsageRow
			credits sql.NullFloat64
		)
		if err := rows.Scan(&row.Model, &row.Requests, &row.PromptTokens,
			&row.CompletionTokens, &credits); err != nil {
			return nil, fmt.Errorf("store: model usage summary: scan: %w", err)
		}
		if credits.Valid {
			value := credits.Float64
			row.Credits = &value
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: model usage summary: iterate: %w", err)
	}
	return out, nil
}

// ModelLatencyStats is LatencyStats grouped by model instead of by day
// (A: usage.rs:191-196). The population is identical: only status=200 rows,
// total latency from every such row, TTFB only where ttfb_ms > 0.
func (s *Store) ModelLatencyStats(days int) ([]ModelLatencyRow, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: model latency stats: store is not open")
	}
	from, to, _ := analyticsWindow(days, time.Now())
	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT COALESCE(model, ''),
		COALESCE(latency_ms, 0), COALESCE(ttfb_ms, 0)
		FROM api_logs
		WHERE COALESCE(status, 0) = 200
		  AND `+dayRangeSQL("ts", from, to), dayRangeArgs(from, to)...)
	if err != nil {
		return nil, fmt.Errorf("store: model latency stats: %w", err)
	}
	defer rows.Close()

	type bucket struct {
		latency []int64
		ttfb    []int64
	}
	buckets := make(map[string]*bucket)
	for rows.Next() {
		var (
			model     string
			latencyMS int64
			ttfbMS    int64
		)
		if err := rows.Scan(&model, &latencyMS, &ttfbMS); err != nil {
			return nil, fmt.Errorf("store: model latency stats: scan: %w", err)
		}
		b := buckets[model]
		if b == nil {
			b = &bucket{}
			buckets[model] = b
		}
		b.latency = append(b.latency, latencyMS)
		if ttfbMS > 0 {
			b.ttfb = append(b.ttfb, ttfbMS)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: model latency stats: iterate: %w", err)
	}

	out := make([]ModelLatencyRow, 0, len(buckets))
	for model, b := range buckets {
		out = append(out, ModelLatencyRow{
			Model:     model,
			Requests:  int64(len(b.latency)),
			P50MS:     nearestRankPercentile(b.latency, 50),
			P95MS:     nearestRankPercentile(b.latency, 95),
			P50TTFBMS: nearestRankPercentile(b.ttfb, 50),
			P95TTFBMS: nearestRankPercentile(b.ttfb, 95),
		})
	}
	// Map iteration is unordered; the ranking must be stable (requests desc).
	sort.Slice(out, func(i, j int) bool {
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].Model < out[j].Model
	})
	return out, nil
}

// nearestRankPercentile is A's percentile(): nearest-rank with ceil(p%*n)-1,
// sorted ascending, clamped. It returns nil for an empty sample set.
func nearestRankPercentile(samples []int64, p float64) *int64 {
	if len(samples) == 0 {
		return nil
	}
	sorted := append([]int64(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	value := sorted[idx]
	return &value
}
