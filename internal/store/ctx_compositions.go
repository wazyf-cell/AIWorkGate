// ctx_compositions：六维上下文采样独立表（task-62）。一行一条采样请求；
// sections/items 以 gateway 序列化好的 JSON 字符串直通存取（store 不解释其
// 结构，契约归 gateway）。写入频率低（sticky 门控采样）、行大（条级明细），
// 独立于 api_logs 宽表；保留期清理沿用宿主的日志清理节奏（未接时不清理也
// 不膨胀——采样关闭时零写入）。
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// CtxCompositionRecord is one sampled request's six-bucket breakdown. The JSON
// blobs pass through verbatim: sections carry est/share/allocated per bucket,
// items carry the per-message / per-tool drill-down rows.
type CtxCompositionRecord struct {
	ReqID          string          `json:"req_id"`
	TS             int64           `json:"ts"`
	TotalEst       int             `json:"total_est"`
	Sections       json.RawMessage `json:"sections"`
	Items          json.RawMessage `json:"items,omitempty"`
	UpstreamPrompt int64           `json:"upstream_prompt_tokens,omitempty"`
	UsageKnown     bool            `json:"usage_known"`
	SampledReason  string          `json:"sampled_reason,omitempty"`
}

// SaveCtxComposition inserts (or replaces, same req_id = idempotent retry) one
// sampled row. It never fails the request path: callers log the error.
func (s *Store) SaveCtxComposition(ctx context.Context, rec CtxCompositionRecord) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: save ctx composition: store is not open")
	}
	if rec.ReqID == "" {
		return fmt.Errorf("store: save ctx composition: empty req_id")
	}
	if len(rec.Sections) == 0 {
		return fmt.Errorf("store: save ctx composition: empty sections")
	}
	var items any
	if len(rec.Items) > 0 {
		items = string(rec.Items)
	}
	var upstream any
	if rec.UpstreamPrompt > 0 {
		upstream = rec.UpstreamPrompt
	}
	var reason any
	if rec.SampledReason != "" {
		reason = rec.SampledReason
	}
	usageKnown := 0
	if rec.UsageKnown {
		usageKnown = 1
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO ctx_compositions
		(req_id, ts, total_est, sections_json, items_json, upstream_prompt_tokens, usage_known, sampled_reason)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(req_id) DO UPDATE SET
			ts = excluded.ts, total_est = excluded.total_est,
			sections_json = excluded.sections_json, items_json = excluded.items_json,
			upstream_prompt_tokens = excluded.upstream_prompt_tokens,
			usage_known = excluded.usage_known, sampled_reason = excluded.sampled_reason`,
		rec.ReqID, rec.TS, rec.TotalEst, string(rec.Sections), items,
		upstream, usageKnown, reason)
	if err != nil {
		return fmt.Errorf("store: save ctx composition %s: %w", rec.ReqID, err)
	}
	return nil
}

// GetCtxComposition reads one sampled row; ok=false means the request was not
// sampled (gate closed, body unparsable, or pre-v30 row).
func (s *Store) GetCtxComposition(ctx context.Context, reqID string) (CtxCompositionRecord, bool, error) {
	if s == nil || s.db == nil {
		return CtxCompositionRecord{}, false, fmt.Errorf("store: get ctx composition: store is not open")
	}
	if reqID == "" {
		return CtxCompositionRecord{}, false, fmt.Errorf("store: get ctx composition: empty req_id")
	}
	var rec CtxCompositionRecord
	var sections string
	var items, reason sql.NullString
	var upstream sql.NullInt64
	var usageKnown int
	err := s.db.QueryRowContext(ctx, `SELECT req_id, ts, total_est, sections_json, items_json,
			upstream_prompt_tokens, usage_known, sampled_reason
		FROM ctx_compositions WHERE req_id = ?`, reqID).Scan(
		&rec.ReqID, &rec.TS, &rec.TotalEst, &sections, &items, &upstream, &usageKnown, &reason)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return CtxCompositionRecord{}, false, nil
		}
		return CtxCompositionRecord{}, false, fmt.Errorf("store: get ctx composition %s: %w", reqID, err)
	}
	rec.Sections = json.RawMessage(sections)
	if items.Valid && items.String != "" {
		rec.Items = json.RawMessage(items.String)
	}
	if upstream.Valid {
		rec.UpstreamPrompt = upstream.Int64
	}
	rec.UsageKnown = usageKnown != 0
	if reason.Valid {
		rec.SampledReason = reason.String
	}
	return rec, true, nil
}
