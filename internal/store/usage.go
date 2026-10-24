package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"aiworkgate/internal/model"
)

// usageCursorKey is the kv key that stores the highest api_logs id already
// folded into usage_daily.
const usageCursorKey = "usage_rollup_last_id"

// UsageFilter selects usage_daily / quota_snapshots / api_logs rows. Days are
// local YYYY-MM-DD values and inclusive. Dimensions AND together, values
// within one dimension OR together (design §5.2).
type UsageFilter struct {
	FromDay  string
	ToDay    string
	Plugins  []string
	Accounts []model.AccountRef
	Groups   []string
}

// UsageTotals is the scalar rollup of one filtered range.
type UsageTotals struct {
	Requests         int64
	Errors           int64
	PromptTokens     int64
	CompletionTokens int64
	Credits          float64
	LatencyMSSum     int64
	TTFBMSSum        int64
	TTFBCount        int64
	ActiveAccounts   int64
}

// UsageAccountCredit is the newest quota snapshot of one account plus the
// credits reported by request logs inside the queried range.
type UsageAccountCredit struct {
	PluginID     string
	AccountID    string
	Remaining    float64
	Total        float64
	ExpiresAt    int64
	Plan         string
	At           int64
	DailyCredits float64
}

// pluginIDExpr renders the SQL that recovers the bare plugin id used by
// account_meta and quota_snapshots from a usage_daily.plugin_id /
// api_logs.source value.
//
// Plugin rows are stored as "plugin:<id>", but the same columns also hold
// non-plugin producers: "gateway", "custom:<hash>", and the empty string.
// The old CASE only special-cased 'a' and stripped the first eight bytes of
// everything else, which silently turned "gateway" into "" and "scheduler" into
// "er". Keying on the prefix instead leaves every non-plugin value intact and
// still resolves a legacy 'a' (it falls through to the ELSE branch unchanged).
func pluginIDExpr(column string) string {
	return "CASE WHEN " + column + " LIKE 'plugin:%' THEN substr(" + column + ", 8) ELSE " + column + " END"
}

// usagePluginValue normalises a plugin filter value to the form stored in
// usage_daily.plugin_id.
//
// The bare "a" case must stay until forwardA is deleted: A is still routed
// outside the plugin system, and its requests record source="a", so new A rows
// keep landing under the legacy value even after the historical rows are
// migrated to "plugin:a". Dropping this branch now would make a filter for "a"
// silently miss all live A traffic. Removing forwardA (next round) removes the
// writer, and this branch with it.
func usagePluginValue(raw string) string {
	raw = strings.TrimSpace(raw)
	switch {
	case raw == "", raw == "a", strings.HasPrefix(raw, "plugin:"):
		return raw
	default:
		return "plugin:" + raw
	}
}

// rawPluginValue strips the usage_daily "plugin:" prefix so the value can be
// compared with quota_snapshots.plugin_id / account_meta.plugin_id.
func rawPluginValue(raw string) string {
	return strings.TrimPrefix(strings.TrimSpace(raw), "plugin:")
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func pairPlaceholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("(?,?),", n), ",")
}

// dailyWhere builds the usage_daily predicate of one filter.
func (f UsageFilter) dailyWhere() (string, []any) {
	var (
		conds []string
		args  []any
	)
	if f.FromDay != "" {
		conds = append(conds, "day >= ?")
		args = append(args, f.FromDay)
	}
	if f.ToDay != "" {
		conds = append(conds, "day <= ?")
		args = append(args, f.ToDay)
	}
	if len(f.Plugins) > 0 {
		values := make([]string, 0, len(f.Plugins))
		for _, raw := range f.Plugins {
			if v := usagePluginValue(raw); v != "" {
				values = append(values, v)
			}
		}
		if len(values) > 0 {
			conds = append(conds, "plugin_id IN ("+placeholders(len(values))+")")
			for _, v := range values {
				args = append(args, v)
			}
		}
	}
	if len(f.Accounts) > 0 {
		pairs := make([]string, 0, len(f.Accounts)*2)
		for _, ref := range f.Accounts {
			pairs = append(pairs, usagePluginValue(ref.PluginID), ref.AccountID)
		}
		conds = append(conds, "(plugin_id, account_id) IN (VALUES "+pairPlaceholders(len(f.Accounts))+")")
		for _, v := range pairs {
			args = append(args, v)
		}
	}
	if len(f.Groups) > 0 {
		conds = append(conds, `EXISTS (SELECT 1 FROM account_meta m
			WHERE m.group_id IN (`+placeholders(len(f.Groups))+`)
			  AND m.account_id = usage_daily.account_id
			  AND m.plugin_id = `+pluginIDExpr("usage_daily.plugin_id")+`)`)
		for _, g := range f.Groups {
			args = append(args, g)
		}
	}
	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// apiLogWhere builds the api_logs predicate of one filter. Day bounds are
// compared against the local date prefix of the RFC3339 ts column.
func (f UsageFilter) apiLogWhere() (string, []any) {
	var (
		conds []string
		args  []any
	)
	if f.FromDay != "" || f.ToDay != "" {
		conds, args = dayRangeConds(conds, args, "ts", f.FromDay, f.ToDay)
	}
	if len(f.Plugins) > 0 {
		values := make([]string, 0, len(f.Plugins))
		for _, raw := range f.Plugins {
			if v := usagePluginValue(raw); v != "" {
				values = append(values, v)
			}
		}
		if len(values) > 0 {
			conds = append(conds, "source IN ("+placeholders(len(values))+")")
			for _, v := range values {
				args = append(args, v)
			}
		}
	}
	if len(f.Accounts) > 0 {
		conds = append(conds, "(source, COALESCE(account, '')) IN (VALUES "+pairPlaceholders(len(f.Accounts))+")")
		for _, ref := range f.Accounts {
			args = append(args, usagePluginValue(ref.PluginID), ref.AccountID)
		}
	}
	if len(f.Groups) > 0 {
		conds = append(conds, `EXISTS (SELECT 1 FROM account_meta m
			WHERE m.group_id IN (`+placeholders(len(f.Groups))+`)
			  AND m.account_id = COALESCE(api_logs.account, '')
			  AND m.plugin_id = `+pluginIDExpr("api_logs.source")+`)`)
		for _, g := range f.Groups {
			args = append(args, g)
		}
	}
	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// quotaWhere builds the quota_snapshots predicate of one filter (no days: the
// endpoint always wants the newest snapshot per account).
func (f UsageFilter) quotaWhere(alias string) (string, []any) {
	var (
		conds []string
		args  []any
	)
	if len(f.Plugins) > 0 {
		values := make([]string, 0, len(f.Plugins))
		for _, raw := range f.Plugins {
			if v := rawPluginValue(raw); v != "" {
				values = append(values, v)
			}
		}
		if len(values) > 0 {
			conds = append(conds, alias+"plugin_id IN ("+placeholders(len(values))+")")
			for _, v := range values {
				args = append(args, v)
			}
		}
	}
	if len(f.Accounts) > 0 {
		conds = append(conds, "("+alias+"plugin_id, "+alias+"account_id) IN (VALUES "+pairPlaceholders(len(f.Accounts))+")")
		for _, ref := range f.Accounts {
			args = append(args, rawPluginValue(ref.PluginID), ref.AccountID)
		}
	}
	if len(f.Groups) > 0 {
		conds = append(conds, `EXISTS (SELECT 1 FROM account_meta m
			WHERE m.group_id IN (`+placeholders(len(f.Groups))+`)
			  AND m.plugin_id = `+alias+`plugin_id
			  AND m.account_id = `+alias+`account_id)`)
		for _, g := range f.Groups {
			args = append(args, g)
		}
	}
	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// UsageTotalsOf sums usage_daily over the filtered range.
func (s *Store) UsageTotalsOf(f UsageFilter) (UsageTotals, error) {
	var out UsageTotals
	if s == nil || s.db == nil {
		return out, fmt.Errorf("store: usage totals: store is not open")
	}
	where, args := f.dailyWhere()
	q := `SELECT COALESCE(SUM(requests), 0), COALESCE(SUM(errors), 0),
		COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0),
		COALESCE(SUM(credits), 0), COALESCE(SUM(latency_ms_sum), 0),
		COALESCE(SUM(ttfb_ms_sum), 0), COALESCE(SUM(ttfb_count), 0),
		COALESCE(COUNT(DISTINCT CASE WHEN account_id <> ''
			THEN plugin_id || '/' || account_id END), 0)
		FROM usage_daily` + where
	ctx, cancel := s.ctx()
	defer cancel()
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(
		&out.Requests, &out.Errors, &out.PromptTokens, &out.CompletionTokens,
		&out.Credits, &out.LatencyMSSum, &out.TTFBMSSum, &out.TTFBCount,
		&out.ActiveAccounts,
	); err != nil {
		return out, fmt.Errorf("store: usage totals: %w", err)
	}
	return out, nil
}

// UsageSeriesOf returns usage_daily grouped by day and plugin_id.
func (s *Store) UsageSeriesOf(f UsageFilter) ([]model.UsageDailyRow, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: usage series: store is not open")
	}
	where, args := f.dailyWhere()
	q := `SELECT day, plugin_id, COALESCE(SUM(requests), 0), COALESCE(SUM(errors), 0),
		COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0),
		COALESCE(SUM(credits), 0), COALESCE(SUM(latency_ms_sum), 0),
		COALESCE(SUM(ttfb_ms_sum), 0), COALESCE(SUM(ttfb_count), 0)
		FROM usage_daily` + where + ` GROUP BY day, plugin_id ORDER BY day, plugin_id`
	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: usage series: %w", err)
	}
	defer rows.Close()
	var out []model.UsageDailyRow
	for rows.Next() {
		var row model.UsageDailyRow
		if err := rows.Scan(&row.Day, &row.PluginID, &row.Requests, &row.Errors,
			&row.PromptTokens, &row.CompletionTokens, &row.Credits,
			&row.LatencyMSSum, &row.TTFBMSSum, &row.TTFBCount); err != nil {
			return nil, fmt.Errorf("store: usage series: scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: usage series: iterate: %w", err)
	}
	return out, nil
}

// UsageAccountCredits returns the latest quota snapshot of every matching
// account together with the credits reported for the range.
func (s *Store) UsageAccountCredits(f UsageFilter) ([]UsageAccountCredit, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: usage credits: store is not open")
	}
	where, args := f.quotaWhere("q.")
	q := `SELECT q.plugin_id, q.account_id, COALESCE(q.remaining, 0), COALESCE(q.total, 0),
		COALESCE(q.expires_at, 0), COALESCE(q.plan, ''), q.at
		FROM quota_snapshots q
		JOIN (SELECT plugin_id, account_id, MAX(at) AS at FROM quota_snapshots GROUP BY plugin_id, account_id) m
		  ON q.plugin_id = m.plugin_id AND q.account_id = m.account_id AND q.at = m.at` + where + `
		ORDER BY q.plugin_id, q.account_id`
	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: usage credits: %w", err)
	}
	var out []UsageAccountCredit
	for rows.Next() {
		var item UsageAccountCredit
		if err := rows.Scan(&item.PluginID, &item.AccountID, &item.Remaining, &item.Total,
			&item.ExpiresAt, &item.Plan, &item.At); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: usage credits: scan: %w", err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("store: usage credits: iterate: %w", err)
	}
	rows.Close()

	// Daily credits reported per account within the range.
	dailyWhere, dailyArgs := f.dailyWhere()
	dailyQ := `SELECT ` + pluginIDExpr("plugin_id") + `,
		account_id, COALESCE(SUM(credits), 0)
		FROM usage_daily` + dailyWhere + ` GROUP BY 1, 2`
	rows, err = s.db.QueryContext(ctx, dailyQ, dailyArgs...)
	if err != nil {
		return nil, fmt.Errorf("store: usage credits: daily: %w", err)
	}
	defer rows.Close()
	daily := make(map[string]float64, len(out))
	for rows.Next() {
		var (
			pluginID, accountID string
			credits             float64
		)
		if err := rows.Scan(&pluginID, &accountID, &credits); err != nil {
			return nil, fmt.Errorf("store: usage credits: daily scan: %w", err)
		}
		daily[pluginID+"\x00"+accountID] = credits
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: usage credits: daily iterate: %w", err)
	}
	for i := range out {
		out[i].DailyCredits = daily[out[i].PluginID+"\x00"+out[i].AccountID]
	}
	return out, nil
}

// AccountDisplayNames maps "plugin\x00account" to alias, cached label or the
// account id (design §8.1: alias || label || id).
func (s *Store) AccountDisplayNames() (map[string]string, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: account display names: store is not open")
	}
	ctx, cancel := s.ctx()
	defer cancel()
	out := make(map[string]string)

	rows, err := s.db.QueryContext(ctx, `SELECT plugin_id, account_id, COALESCE(alias, '') FROM account_meta`)
	if err != nil {
		return nil, fmt.Errorf("store: account display names: meta: %w", err)
	}
	for rows.Next() {
		var pluginID, accountID, alias string
		if err := rows.Scan(&pluginID, &accountID, &alias); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: account display names: meta scan: %w", err)
		}
		out[pluginID+"\x00"+accountID] = alias
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("store: account display names: meta iterate: %w", err)
	}
	rows.Close()

	rows, err = s.db.QueryContext(ctx, `SELECT plugin_id, account_id, payload FROM account_cache`)
	if err != nil {
		return nil, fmt.Errorf("store: account display names: cache: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var pluginID, accountID, payload string
		if err := rows.Scan(&pluginID, &accountID, &payload); err != nil {
			return nil, fmt.Errorf("store: account display names: cache scan: %w", err)
		}
		if out[pluginID+"\x00"+accountID] != "" {
			continue // alias wins
		}
		var acc model.Account
		if err := json.Unmarshal([]byte(payload), &acc); err != nil {
			continue
		}
		if label := strings.TrimSpace(acc.Label); label != "" {
			out[pluginID+"\x00"+accountID] = label
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: account display names: cache iterate: %w", err)
	}
	return out, nil
}

// UsageSessions returns request-level rows newest first plus the total.
func (s *Store) UsageSessions(f UsageFilter, modelFilter string, limit, offset int) ([]model.APIRequestLog, int, error) {
	if s == nil || s.db == nil {
		return nil, 0, fmt.Errorf("store: usage sessions: store is not open")
	}
	where, args := f.apiLogWhere()
	if filter := strings.TrimSpace(modelFilter); filter != "" {
		if where == "" {
			where = " WHERE model = ?"
		} else {
			where += " AND model = ?"
		}
		args = append(args, filter)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_logs`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: usage sessions: count: %w", err)
	}
	q := `SELECT ts, COALESCE(req_id, ''), COALESCE(source, ''), COALESCE(model, ''),
		COALESCE(logical_model, ''), COALESCE(account, ''), COALESCE(conversation_id, ''),
		COALESCE(prompt_tokens, 0), COALESCE(completion_tokens, 0), credits,
		COALESCE(ttfb_ms, 0), COALESCE(latency_ms, 0), COALESCE(status, 0)
		FROM api_logs` + where + ` ORDER BY ts DESC, id DESC LIMIT ? OFFSET ?`
	rows, err := s.db.QueryContext(ctx, q, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("store: usage sessions: %w", err)
	}
	defer rows.Close()
	out := make([]model.APIRequestLog, 0, limit)
	for rows.Next() {
		var (
			rec     model.APIRequestLog
			credits sql.NullFloat64
		)
		if err := rows.Scan(&rec.TS, &rec.ReqID, &rec.Source, &rec.Model, &rec.LogicalModel,
			&rec.Account, &rec.ConversationID, &rec.PromptTokens, &rec.CompletionTokens,
			&credits, &rec.TTFBMS, &rec.LatencyMS, &rec.Status); err != nil {
			return nil, 0, fmt.Errorf("store: usage sessions: scan: %w", err)
		}
		if credits.Valid {
			v := credits.Float64
			rec.Credits = &v
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: usage sessions: iterate: %w", err)
	}
	return out, total, nil
}

// APILogBodies returns the stored (already redacted) bodies of the newest row
// carrying reqID.
func (s *Store) APILogBodies(reqID string) (reqBody, respBody string, found bool, err error) {
	if s == nil || s.db == nil {
		return "", "", false, fmt.Errorf("store: api log bodies: store is not open")
	}
	reqID = strings.TrimSpace(reqID)
	if reqID == "" {
		return "", "", false, fmt.Errorf("store: api log bodies: %w: req_id is required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	err = s.db.QueryRowContext(ctx, `SELECT COALESCE(body_req, ''), COALESCE(body_resp, '')
		FROM api_logs WHERE req_id = ? ORDER BY id DESC LIMIT 1`, reqID).Scan(&reqBody, &respBody)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", "", false, nil
		}
		return "", "", false, fmt.Errorf("store: api log bodies: %w", err)
	}
	return reqBody, respBody, true, nil
}

// CleanupAPILogBodies NULLs the stored bodies of rows older than before while
// keeping the request rows themselves (D97).
func (s *Store) CleanupAPILogBodies(before int64) (int64, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("store: cleanup api log bodies: store is not open")
	}
	if before <= 0 {
		return 0, fmt.Errorf("store: cleanup api log bodies: %w: before must be positive", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	cutoff := time.Unix(before, 0).Format(time.RFC3339)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		res, err := s.db.ExecContext(ctx, `UPDATE api_logs SET body_req = NULL, body_resp = NULL
			WHERE id IN (SELECT id FROM api_logs
				WHERE ts < ? AND (body_req IS NOT NULL OR body_resp IS NOT NULL) LIMIT ?)`,
			cutoff, cleanupBatch)
		if err != nil {
			return total, fmt.Errorf("store: cleanup api log bodies: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("store: cleanup api log bodies: rows affected: %w", err)
		}
		total += n
		if n < cleanupBatch {
			return total, nil
		}
	}
}

// CleanupUsageDaily deletes aggregates older than the given day.
func (s *Store) CleanupUsageDaily(beforeDay string) (int64, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("store: cleanup usage daily: store is not open")
	}
	if strings.TrimSpace(beforeDay) == "" {
		return 0, fmt.Errorf("store: cleanup usage daily: %w: day is required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		res, err := s.db.ExecContext(ctx, `DELETE FROM usage_daily WHERE rowid IN (
			SELECT rowid FROM usage_daily WHERE day < ? LIMIT ?)`, beforeDay, cleanupBatch)
		if err != nil {
			return total, fmt.Errorf("store: cleanup usage daily: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("store: cleanup usage daily: rows affected: %w", err)
		}
		total += n
		if n < cleanupBatch {
			return total, nil
		}
	}
}

// ------------------------------------------------------------------ rollup

// RollUsage folds one batch of api_logs into usage_daily. The upsert adds
// deltas and the kv cursor advances in the same transaction, so a crash
// reverts both and rows are never counted twice (design §8.3). It returns the
// number of rows aggregated, the highest id consumed and whether another batch
// is ready.
func (s *Store) RollUsage(ctx context.Context, cutoff time.Time, batch int) (rolled int64, lastID int64, more bool, err error) {
	if s == nil || s.db == nil {
		return 0, 0, false, fmt.Errorf("store: roll usage: store is not open")
	}
	if batch <= 0 {
		batch = 1000
	}
	if ctx == nil {
		ctx = context.Background()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, false, fmt.Errorf("store: roll usage: begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	var cursor int64
	var content string
	rowErr := tx.QueryRowContext(ctx, `SELECT COALESCE(content, '') FROM kv WHERE key = ?`, usageCursorKey).Scan(&content)
	switch {
	case errors.Is(rowErr, sql.ErrNoRows):
		cursor = 0
	case rowErr != nil:
		return 0, 0, false, fmt.Errorf("store: roll usage: read cursor: %w", rowErr)
	default:
		cursor, _ = strconv.ParseInt(strings.TrimSpace(content), 10, 64)
	}

	// First run (or a reset cursor): skip rows older than the catch-up window
	// instead of folding the whole log history.
	if cursor == 0 && !cutoff.IsZero() {
		var skipped int64
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM api_logs WHERE ts < ?`,
			cutoff.Format(time.RFC3339)).Scan(&skipped); err != nil {
			return 0, 0, false, fmt.Errorf("store: roll usage: catch-up bound: %w", err)
		}
		if skipped > cursor {
			cursor = skipped
			if err = setUsageCursorTx(ctx, tx, cursor); err != nil {
				return 0, 0, false, err
			}
		}
	}

	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MAX(id), 0)
		FROM (SELECT id FROM api_logs WHERE id > ? ORDER BY id LIMIT ?)`,
		cursor, batch).Scan(&rolled, &lastID); err != nil {
		return 0, 0, false, fmt.Errorf("store: roll usage: batch bounds: %w", err)
	}
	if rolled == 0 {
		if err = tx.Commit(); err != nil {
			return 0, 0, false, fmt.Errorf("store: roll usage: commit empty: %w", err)
		}
		return 0, cursor, false, nil
	}

	const upsert = `INSERT INTO usage_daily
		(day, plugin_id, account_id, model, requests, errors,
		 prompt_tokens, completion_tokens, cached_tokens, credits, latency_ms_sum, ttfb_ms_sum, ttfb_count)
		SELECT day, plugin_id, account_id, model, COUNT(*),
			COALESCE(SUM(CASE WHEN status = 0 OR status >= 400 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0),
			COALESCE(SUM(cached_tokens), 0),
			SUM(credits), COALESCE(SUM(latency_ms), 0),
			COALESCE(SUM(CASE WHEN status = 200 AND ttfb_ms > 0 THEN ttfb_ms ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 200 AND ttfb_ms > 0 THEN 1 ELSE 0 END), 0)
		FROM (
			SELECT CASE WHEN length(ts) >= 10 THEN substr(ts, 1, 10) ELSE date(ts) END AS day,
				COALESCE(source, '') AS plugin_id,
				COALESCE(account, '') AS account_id,
				COALESCE(model, '') AS model,
				COALESCE(status, 0) AS status,
				COALESCE(prompt_tokens, 0) AS prompt_tokens,
				COALESCE(completion_tokens, 0) AS completion_tokens,
				COALESCE(cached_tokens, 0) AS cached_tokens,
				credits AS credits,
				COALESCE(latency_ms, 0) AS latency_ms,
				COALESCE(ttfb_ms, 0) AS ttfb_ms
			FROM api_logs
			-- Model-less rows are probes (GET /v1/models, auth rejections, A
			-- health): they belong in the request log, not in model usage.
			WHERE id > ? AND id <= ?
			  AND COALESCE(model, '') <> ''
		) b
		GROUP BY day, plugin_id, account_id, model
		ON CONFLICT(day, plugin_id, account_id, model) DO UPDATE SET
			requests = COALESCE(usage_daily.requests, 0) + excluded.requests,
			errors = COALESCE(usage_daily.errors, 0) + excluded.errors,
			prompt_tokens = COALESCE(usage_daily.prompt_tokens, 0) + excluded.prompt_tokens,
			completion_tokens = COALESCE(usage_daily.completion_tokens, 0) + excluded.completion_tokens,
			cached_tokens = COALESCE(usage_daily.cached_tokens, 0) + excluded.cached_tokens,
			credits = CASE WHEN excluded.credits IS NULL THEN usage_daily.credits
				ELSE COALESCE(usage_daily.credits, 0) + excluded.credits END,
			latency_ms_sum = COALESCE(usage_daily.latency_ms_sum, 0) + excluded.latency_ms_sum,
			ttfb_ms_sum = COALESCE(usage_daily.ttfb_ms_sum, 0) + excluded.ttfb_ms_sum,
			ttfb_count = COALESCE(usage_daily.ttfb_count, 0) + excluded.ttfb_count`
	if _, err = tx.ExecContext(ctx, upsert, cursor, lastID); err != nil {
		return 0, 0, false, fmt.Errorf("store: roll usage: upsert: %w", err)
	}
	if err = setUsageCursorTx(ctx, tx, lastID); err != nil {
		return 0, 0, false, err
	}
	if err = tx.Commit(); err != nil {
		return 0, 0, false, fmt.Errorf("store: roll usage: commit: %w", err)
	}
	return rolled, lastID, rolled == int64(batch), nil
}

func setUsageCursorTx(ctx context.Context, tx *sql.Tx, id int64) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO kv (key, content, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET content = excluded.content, updated_at = excluded.updated_at`,
		usageCursorKey, strconv.FormatInt(id, 10), time.Now().Unix()); err != nil {
		return fmt.Errorf("store: roll usage: write cursor: %w", err)
	}
	return nil
}

// ------------------------------------------------------------------ timeline

// ListAuditBetween returns audit rows whose local day is inside the range,
// newest first. The limit is clamped to one page; ListAuditBetweenForTimeline
// is the deep-scan variant the merged timeline uses.
func (s *Store) ListAuditBetween(fromDay, toDay string, limit int) ([]model.AuditEntry, error) {
	return s.listAuditBetweenClamped(fromDay, toDay, limit, 5000)
}

// ListAuditTimelinePage returns up to limit audit rows older than the
// (beforeAt, beforeID) position inside the day window, newest first. beforeAt
// is Unix milliseconds; zero means "from the newest row", and a zero beforeID
// includes every row at beforeAt. The merged timeline pages by keyset so page N
// does not re-scan the rows already shown.
func (s *Store) ListAuditTimelinePage(fromDay, toDay string, beforeAt, beforeID int64, limit int) ([]model.AuditEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > maxLogScanLimit {
		limit = maxLogScanLimit
	}
	where := " WHERE 1 = 1"
	var args []any
	conds, args := dayRangeConds(nil, args, "ts", fromDay, toDay)
	for _, c := range conds {
		where += " AND " + c
	}
	if beforeAt > 0 {
		if beforeID <= 0 {
			beforeID = math.MaxInt64
		}
		where += fmt.Sprintf(" AND (%s < ? OR (%s = ? AND id < ?))", timelineEpochExpr, timelineEpochExpr)
		args = append(args, beforeAt, beforeAt, beforeID)
	}
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: list audit timeline page: store is not open")
	}
	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT id, ts, COALESCE(category, ''), COALESCE(action, ''),
		COALESCE(target, ''), COALESCE(detail, ''), COALESCE(result, '')
		FROM audit_log`+where+` ORDER BY `+timelineEpochExpr+` DESC, id DESC LIMIT ?`,
		append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("store: list audit timeline page: %w", err)
	}
	defer rows.Close()
	return scanAuditRows(rows)
}

func (s *Store) listAuditBetweenClamped(fromDay, toDay string, limit, cap int) ([]model.AuditEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > cap {
		limit = cap
	}
	where := " WHERE 1 = 1"
	var args []any
	whereConds, args := dayRangeConds(nil, args, "ts", fromDay, toDay)
	for _, c := range whereConds {
		where += " AND " + c
	}
	return s.listAuditWhere(where, args, limit, true)
}

// ListAuditSince returns audit rows with id > id inside the range, oldest
// first (stream tailing).
func (s *Store) ListAuditSince(id int64, fromDay, toDay string, limit int) ([]model.AuditEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 5000 {
		limit = 5000
	}
	where := " WHERE id > ?"
	args := []any{id}
	whereConds, args := dayRangeConds(nil, args, "ts", fromDay, toDay)
	for _, c := range whereConds {
		where += " AND " + c
	}
	return s.listAuditWhere(where, args, limit, false)
}

func scanAuditRows(rows *sql.Rows) ([]model.AuditEntry, error) {
	var out []model.AuditEntry
	for rows.Next() {
		var entry model.AuditEntry
		if err := rows.Scan(&entry.ID, &entry.TS, &entry.Category, &entry.Action,
			&entry.Target, &entry.Detail, &entry.Result); err != nil {
			return nil, fmt.Errorf("store: scan audit rows: %w", err)
		}
		out = append(out, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: scan audit rows: %w", err)
	}
	return out, nil
}

func (s *Store) listAuditWhere(where string, args []any, limit int, newestFirst bool) ([]model.AuditEntry, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: list audit range: store is not open")
	}
	q := `SELECT id, ts, COALESCE(category, ''), COALESCE(action, ''),
		COALESCE(target, ''), COALESCE(detail, ''), COALESCE(result, '')
		FROM audit_log` + where
	if newestFirst {
		q += ` ORDER BY id DESC LIMIT ?`
	} else {
		q += ` ORDER BY id ASC LIMIT ?`
	}
	args = append(args, limit)
	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list audit range: %w", err)
	}
	defer rows.Close()
	var out []model.AuditEntry
	for rows.Next() {
		var entry model.AuditEntry
		if err := rows.Scan(&entry.ID, &entry.TS, &entry.Category, &entry.Action,
			&entry.Target, &entry.Detail, &entry.Result); err != nil {
			return nil, fmt.Errorf("store: list audit range: scan: %w", err)
		}
		out = append(out, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list audit range: iterate: %w", err)
	}
	return out, nil
}

// QueryAPILogsSince returns request rows with id > id inside the range, oldest
// first, without bodies, plus the highest id seen (the SSE cursor).
func (s *Store) QueryAPILogsSince(id int64, fromDay, toDay string, limit int) ([]model.APIRequestLog, int64, error) {
	if s == nil || s.db == nil {
		return nil, id, fmt.Errorf("store: api logs since: store is not open")
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 5000 {
		limit = 5000
	}
	where := " WHERE id > ?"
	args := []any{id}
	whereConds, args := dayRangeConds(nil, args, "ts", fromDay, toDay)
	for _, c := range whereConds {
		where += " AND " + c
	}
	q := `SELECT id, ` + apiLogColumnsNoBodies + ` FROM api_logs` + where + ` ORDER BY id ASC LIMIT ?`
	args = append(args, limit)
	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, id, fmt.Errorf("store: api logs since: %w", err)
	}
	defer rows.Close()
	var (
		out    []model.APIRequestLog
		lastID int64
	)
	for rows.Next() {
		var (
			rec     model.APIRequestLog
			rowID   int64
			stream  int64
			credits sql.NullFloat64
		)
		if err := rows.Scan(&rowID, &rec.TS, &rec.ReqID, &rec.Source, &rec.Model,
			&rec.LogicalModel, &rec.KeyID, &rec.KeyName, &rec.Account, &rec.ConversationID,
			&stream, &rec.Status, &rec.LatencyMS, &rec.TTFBMS, &rec.FirstTokenAt,
			&rec.PromptTokens, &rec.CompletionTokens, &rec.CachedTokens, &credits, &rec.Error); err != nil {
			return nil, id, fmt.Errorf("store: api logs since: scan: %w", err)
		}
		rec.Stream = stream != 0
		if credits.Valid {
			v := credits.Float64
			rec.Credits = &v
		}
		lastID = rowID
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, id, fmt.Errorf("store: api logs since: iterate: %w", err)
	}
	if lastID < id {
		lastID = id
	}
	return out, lastID, nil
}

// MaxAPILogID and MaxAuditID seed an SSE cursor.
func (s *Store) MaxAPILogID() (int64, error) {
	return s.maxID("api_logs")
}

func (s *Store) MaxAuditID() (int64, error) {
	return s.maxID("audit_log")
}

func (s *Store) maxID(table string) (int64, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("store: max id: store is not open")
	}
	switch table {
	case "api_logs", "audit_log":
	default:
		return 0, fmt.Errorf("store: max id: %w: unsupported table", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	var id int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM `+table).Scan(&id); err != nil {
		return 0, fmt.Errorf("store: max id %s: %w", table, err)
	}
	return id, nil
}
