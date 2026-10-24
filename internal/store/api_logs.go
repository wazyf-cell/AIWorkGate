package store

import (
	"database/sql"
	"fmt"
	"math"
	"time"

	"aiworkgate/internal/model"
)

const (
	defaultLogLimit = 100
	// maxLogLimit bounds one HTTP page of the request-log table. The admin API
	// never asks for more, and a bigger page would only move memory from the
	// database into the handler.
	maxLogLimit = 1000
	// maxLogScanLimit bounds how many rows one timeline scan may pull. The
	// timeline merges four sources in memory and filters them there, so it
	// needs a deeper window than one page; internal/logging caps the merged
	// result again. Without this the API source stopped at 1000 rows and the
	// second page of the timeline looked empty, as if logs had been lost.
	maxLogScanLimit = 50000
	// MaxExportRows is the hard cap of a single CSV request-log export. A
	// truncated export is reported through the X-Export-Truncated header.
	MaxExportRows = 50000
)

// Status classes accepted by Filter.StatusClass.
const (
	StatusClass2xx   = "2xx"
	StatusClass4xx   = "4xx"
	StatusClass5xx   = "5xx"
	StatusClassError = "error"
)

// ValidStatusClass reports whether value is a supported status class. The
// empty string means "no constraint".
func ValidStatusClass(value string) bool {
	switch value {
	case "", StatusClass2xx, StatusClass4xx, StatusClass5xx, StatusClassError:
		return true
	}
	return false
}

// Filter selects API request log rows. Zero values mean "no constraint"
// except Limit, where zero falls back to 100 (hard maximum 1000).
// From/To are unix seconds compared against the RFC3339 `ts` column.
//
// Model is a case-insensitive substring match; Source, Account and KeyID are
// exact matches. Source is the log's origin: "plugin:<id>", "custom:<id>" or
// "a". StatusClass narrows to 2xx/4xx/5xx, while "error" selects transport
// failures (status 0) and every 4xx/5xx response. Stream nil means both.
type Filter struct {
	From        int64
	To          int64
	Source      string
	Model       string
	Status      int
	KeyID       string
	Account     string
	StatusClass string
	Stream      *bool
	// Keyword is a case-insensitive substring matched against req_id, model,
	// source, account and error.
	Keyword string
	Limit   int
	Offset  int
	// NoBodies skips the two body columns so list views never load previews.
	NoBodies bool
}

const apiLogColumns = `ts, COALESCE(req_id, ''), COALESCE(source, ''), COALESCE(model, ''),
	COALESCE(logical_model, ''), COALESCE(key_id, ''), COALESCE(key_name, ''),
	COALESCE(account, ''), COALESCE(conversation_id, ''), COALESCE(stream, 0), COALESCE(status, 0),
	COALESCE(latency_ms, 0), COALESCE(ttfb_ms, 0), COALESCE(first_token_at, ''),
	COALESCE(prompt_tokens, 0), COALESCE(completion_tokens, 0), COALESCE(cached_tokens, 0), credits,
	COALESCE(error, ''), COALESCE(body_req, ''), COALESCE(body_resp, '')`

// apiLogColumnsNoBodies mirrors apiLogColumns without the two body columns so
// list/timeline queries never load large previews.
const apiLogColumnsNoBodies = `COALESCE(ts, ''), COALESCE(req_id, ''), COALESCE(source, ''), COALESCE(model, ''),
	COALESCE(logical_model, ''), COALESCE(key_id, ''), COALESCE(key_name, ''),
	COALESCE(account, ''), COALESCE(conversation_id, ''), COALESCE(stream, 0), COALESCE(status, 0),
	COALESCE(latency_ms, 0), COALESCE(ttfb_ms, 0), COALESCE(first_token_at, ''),
	COALESCE(prompt_tokens, 0), COALESCE(completion_tokens, 0), COALESCE(cached_tokens, 0), credits, COALESCE(error, '')`

// InsertAPIRequestLog stores one request log row.
func (s *Store) InsertAPIRequestLog(rec model.APIRequestLog) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: insert api log: store is not open")
	}
	ts := rec.TS
	if ts == "" {
		ts = time.Now().Format(time.RFC3339)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	_, err := s.db.ExecContext(ctx, `INSERT INTO api_logs
		(ts, req_id, source, model, logical_model, key_id, key_name, account,
		 conversation_id, stream, status, latency_ms, ttfb_ms, first_token_at,
		 prompt_tokens, completion_tokens, cached_tokens, credits, error, body_req, body_resp)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ts,
		nullStr(rec.ReqID), nullStr(rec.Source), nullStr(rec.Model), nullStr(rec.LogicalModel),
		nullStr(rec.KeyID), nullStr(rec.KeyName), nullStr(rec.Account), nullStr(rec.ConversationID),
		boolInt(rec.Stream), rec.Status,
		rec.LatencyMS, rec.TTFBMS, nullStr(rec.FirstTokenAt), rec.PromptTokens, rec.CompletionTokens,
		rec.CachedTokens, optFloat(rec.Credits), nullStr(rec.Error), nullStr(rec.BodyReq), nullStr(rec.BodyResp),
	)
	if err != nil {
		return fmt.Errorf("store: insert api log: %w", err)
	}
	return nil
}

// QueryAPIRequestLogs returns matching rows newest first together with the
// total number of matching rows (ignoring limit/offset). The limit is clamped
// to one HTTP page; use QueryAPIRequestLogsForTimeline for a deeper scan.
func (s *Store) QueryAPIRequestLogs(f Filter) ([]model.APIRequestLog, int, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultLogLimit
	}
	if limit > maxLogLimit {
		limit = maxLogLimit
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}
	return s.queryAPIRequestLogs(f, limit, offset)
}

// APILogRow pairs one request-log row with its database id. The merged
// timeline pages by keyset and needs the id as tie-breaker; the public list
// endpoints keep their existing shape without it.
type APILogRow struct {
	ID  int64
	Log model.APIRequestLog
}

// QueryAPILogsTimelinePage returns up to limit request-log rows older than the
// (beforeAt, beforeID) position, newest first. beforeAt is Unix milliseconds
// and beforeID is exclusive; zero beforeAt means "from the newest row". A
// zero beforeID includes every row at beforeAt.
//
// Keyset paging (rather than OFFSET) keeps page N as cheap as page 1 and lets
// the timeline walk past any fixed scan window.
func (s *Store) QueryAPILogsTimelinePage(f Filter, beforeAt, beforeID int64, limit int) ([]APILogRow, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: query api logs page: store is not open")
	}
	if limit <= 0 {
		limit = defaultLogLimit
	}
	if limit > maxLogScanLimit {
		limit = maxLogScanLimit
	}
	where, args := f.where()
	if beforeAt > 0 {
		if beforeID <= 0 {
			beforeID = math.MaxInt64
		}
		where += fmt.Sprintf(" AND (%s < ? OR (%s = ? AND id < ?))", timelineEpochExpr, timelineEpochExpr)
		args = append(args, beforeAt, beforeAt, beforeID)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	q := `SELECT id, ` + apiLogColumnsNoBodies + ` FROM api_logs` + where +
		` ORDER BY ` + timelineEpochExpr + ` DESC, id DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("store: query api logs page: %w", err)
	}
	defer rows.Close()

	out := make([]APILogRow, 0, limit)
	for rows.Next() {
		var (
			row     APILogRow
			stream  int64
			credits sql.NullFloat64
		)
		if err := rows.Scan(
			&row.ID, &row.Log.TS, &row.Log.ReqID, &row.Log.Source, &row.Log.Model,
			&row.Log.LogicalModel, &row.Log.KeyID, &row.Log.KeyName, &row.Log.Account,
			&row.Log.ConversationID, &stream, &row.Log.Status,
			&row.Log.LatencyMS, &row.Log.TTFBMS, &row.Log.FirstTokenAt,
			&row.Log.PromptTokens, &row.Log.CompletionTokens, &row.Log.CachedTokens,
			&credits, &row.Log.Error,
		); err != nil {
			return nil, fmt.Errorf("store: query api logs page: scan: %w", err)
		}
		row.Log.Stream = stream != 0
		if credits.Valid {
			v := credits.Float64
			row.Log.Credits = &v
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: query api logs page: iterate: %w", err)
	}
	return out, nil
}

// ExportAPIRequestLogs returns up to max matching rows newest first (bodies
// excluded) and reports whether the result hit that cap. max is clamped to
// MaxExportRows.
func (s *Store) ExportAPIRequestLogs(f Filter, max int) ([]model.APIRequestLog, bool, error) {
	if max <= 0 || max > MaxExportRows {
		max = MaxExportRows
	}
	f.NoBodies = true
	// Fetch one extra row so truncation is detectable without a second count.
	rows, _, err := s.queryAPIRequestLogs(f, max+1, 0)
	if err != nil {
		return nil, false, err
	}
	if len(rows) > max {
		return rows[:max], true, nil
	}
	return rows, false, nil
}

func (s *Store) queryAPIRequestLogs(f Filter, limit, offset int) ([]model.APIRequestLog, int, error) {
	if s == nil || s.db == nil {
		return nil, 0, fmt.Errorf("store: query api logs: store is not open")
	}
	if !ValidStatusClass(f.StatusClass) {
		return nil, 0, fmt.Errorf("store: query api logs: %w: unknown status_class %q", ErrInvalidArgument, f.StatusClass)
	}
	where, args := f.where()

	ctx, cancel := s.ctx()
	defer cancel()

	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_logs`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: query api logs: count: %w", err)
	}

	columns := apiLogColumns
	if f.NoBodies {
		columns = apiLogColumnsNoBodies
	}
	q := `SELECT ` + columns + ` FROM api_logs` + where +
		` ORDER BY ts DESC, id DESC LIMIT ? OFFSET ?`
	rows, err := s.db.QueryContext(ctx, q, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("store: query api logs: %w", err)
	}
	defer rows.Close()

	out := make([]model.APIRequestLog, 0, limit)
	for rows.Next() {
		var (
			rec     model.APIRequestLog
			stream  int64
			credits sql.NullFloat64
		)
		if f.NoBodies {
			if err := rows.Scan(
				&rec.TS, &rec.ReqID, &rec.Source, &rec.Model, &rec.LogicalModel,
				&rec.KeyID, &rec.KeyName, &rec.Account, &rec.ConversationID, &stream, &rec.Status,
				&rec.LatencyMS, &rec.TTFBMS, &rec.FirstTokenAt,
				&rec.PromptTokens, &rec.CompletionTokens, &rec.CachedTokens, &credits, &rec.Error,
			); err != nil {
				return nil, 0, fmt.Errorf("store: query api logs: scan: %w", err)
			}
		} else if err := rows.Scan(
			&rec.TS, &rec.ReqID, &rec.Source, &rec.Model, &rec.LogicalModel,
			&rec.KeyID, &rec.KeyName, &rec.Account, &rec.ConversationID, &stream, &rec.Status,
			&rec.LatencyMS, &rec.TTFBMS, &rec.FirstTokenAt,
			&rec.PromptTokens, &rec.CompletionTokens, &rec.CachedTokens, &credits,
			&rec.Error, &rec.BodyReq, &rec.BodyResp,
		); err != nil {
			return nil, 0, fmt.Errorf("store: query api logs: scan: %w", err)
		}
		rec.Stream = stream != 0
		if credits.Valid {
			v := credits.Float64
			rec.Credits = &v
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: query api logs: iterate: %w", err)
	}
	return out, total, nil
}

func (f Filter) where() (string, []any) {
	var (
		conds []string
		args  []any
	)
	if f.From > 0 {
		conds = append(conds, "ts >= ?")
		args = append(args, time.Unix(f.From, 0).Format(time.RFC3339))
	}
	if f.To > 0 {
		conds = append(conds, "ts <= ?")
		args = append(args, time.Unix(f.To, 0).Format(time.RFC3339))
	}
	if f.Source != "" {
		conds = append(conds, "source = ?")
		args = append(args, f.Source)
	}
	if f.Model != "" {
		// instr() keeps % and _ literal, so a model name is never treated as a
		// LIKE pattern. lower() gives the requested case-insensitive match.
		conds = append(conds, "instr(lower(model), lower(?)) > 0")
		args = append(args, f.Model)
	}
	if f.Status != 0 {
		conds = append(conds, "status = ?")
		args = append(args, f.Status)
	}
	if f.KeyID != "" {
		conds = append(conds, "key_id = ?")
		args = append(args, f.KeyID)
	}
	if f.Account != "" {
		conds = append(conds, "account = ?")
		args = append(args, f.Account)
	}
	switch f.StatusClass {
	case StatusClass2xx:
		conds = append(conds, "status >= 200 AND status < 300")
	case StatusClass4xx:
		conds = append(conds, "status >= 400 AND status < 500")
	case StatusClass5xx:
		conds = append(conds, "status >= 500 AND status < 600")
	case StatusClassError:
		// Transport failures store status 0, so both shapes count as errors.
		conds = append(conds, "(status = 0 OR status >= 400)")
	}
	if f.Stream != nil {
		conds = append(conds, "stream = ?")
		args = append(args, boolInt(*f.Stream))
	}
	if f.Keyword != "" {
		conds = append(conds, `(instr(lower(COALESCE(req_id, '')), lower(?)) > 0
			OR instr(lower(COALESCE(model, '')), lower(?)) > 0
			OR instr(lower(COALESCE(source, '')), lower(?)) > 0
			OR instr(lower(COALESCE(account, '')), lower(?)) > 0
			OR instr(lower(COALESCE(error, '')), lower(?)) > 0)`)
		args = append(args, f.Keyword, f.Keyword, f.Keyword, f.Keyword, f.Keyword)
	}
	if len(conds) == 0 {
		return "", args
	}
	q := " WHERE " + conds[0]
	for _, c := range conds[1:] {
		q += " AND " + c
	}
	return q, args
}

// cleanupBatch bounds one retention DELETE. Deleting every old row in a single
// statement holds the write lock for the whole scan and starves the request-log
// inserts that share this database; bounded batches let other writers interleave.
const cleanupBatch = 5000

// CleanupAPIRequestLogs deletes rows older than the given unix timestamp and
// reports how many were removed. Rows go in cleanupBatch-sized chunks so a long
// backlog cannot monopolise the single SQLite write channel.
func (s *Store) CleanupAPIRequestLogs(before int64) (int64, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("store: cleanup api logs: store is not open")
	}
	if before <= 0 {
		return 0, fmt.Errorf("store: cleanup api logs: %w: before must be positive", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	cutoff := time.Unix(before, 0).Format(time.RFC3339)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		res, err := s.db.ExecContext(ctx, `DELETE FROM api_logs WHERE id IN (
			SELECT id FROM api_logs WHERE ts < ? LIMIT ?)`, cutoff, cleanupBatch)
		if err != nil {
			return total, fmt.Errorf("store: cleanup api logs: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("store: cleanup api logs: rows affected: %w", err)
		}
		total += n
		if n < cleanupBatch {
			return total, nil
		}
	}
}
