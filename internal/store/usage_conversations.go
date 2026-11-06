package store

// D-32③ 会话真聚合：api_logs 按 conversation_id 聚合成会话行。
//
// 分页对齐 magpie request_page 的语义（chunk 复用 + 只物化当前页 + 有界回收）
// 在 SQLite 上的等价实现：keyset 游标（上一页末行的 MAX(ts) + 会话键），
// 不做 COUNT(*)、不做 OFFSET——每页只物化 limit 个会话组，翻页不重扫已见数据。
// 无 conversation_id 的行（探针、旧客户端）按 req:<req_id>:<id> 兜底成独立组，
// 保证它们仍出现在聚合列表里且可下钻到逐请求。

import (
	"database/sql"
	"fmt"
	"strings"
)

// convKeyExpr 是一行的会话键：真实 conversation_id，或无会话 ID 行的
// req:<req_id>:<id> 合成键（id 保证空 req_id 行也各自成组）。
const convKeyExpr = `CASE WHEN COALESCE(conversation_id, '') <> '' THEN conversation_id
	ELSE 'req:' || COALESCE(req_id, '') || ':' || id END`

// convGroupKeyExpr 是聚合行的会话键（聚合语境，id 取 MAX）。
const convGroupKeyExpr = `CASE WHEN COALESCE(conversation_id, '') <> '' THEN conversation_id
	ELSE 'req:' || COALESCE(req_id, '') || ':' || MAX(id) END`

// convGroupByExpr 把无会话 ID 行各自成组、有会话 ID 的行按会话归并。
const convGroupByExpr = `COALESCE(conversation_id, ''), CASE WHEN COALESCE(conversation_id, '') <> '' THEN 0 ELSE id END`

// UsageConversation is one aggregated conversation of /api/usage/conversations.
type UsageConversation struct {
	ConversationID   string
	FirstTS          string
	LastTS           string
	Requests         int64
	Errors           int64
	PromptTokens     int64
	CompletionTokens int64
	Credits          float64 // sum of the non-null credits in the group
	CreditsSampled   bool    // at least one row carried a credit value
	Models           []string
	Plugins          []string
	Accounts         []string
}

// UsageConversations returns one UsageConversation per conversation_id, newest
// conversation first (by its latest request), materialising only the requested
// page. beforeTS/beforeConv form the keyset cursor of the previous page's last
// row (both empty = first page). hasMore says another page exists; the total
// count is deliberately not computed (no COUNT(*) over the whole table).
func (s *Store) UsageConversations(f UsageFilter, modelFilter, beforeTS, beforeConv string, limit int) ([]UsageConversation, bool, error) {
	if s == nil || s.db == nil {
		return nil, false, fmt.Errorf("store: usage conversations: store is not open")
	}
	if limit <= 0 {
		limit = 1
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
	having := ""
	if strings.TrimSpace(beforeTS) != "" {
		// keyset：上一页末行是 (MAX(ts), 会话键)，严格小于它的地方才开始本页
		having = ` HAVING MAX(ts) < ? OR (MAX(ts) = ? AND ` + convGroupKeyExpr + ` < ?)`
		args = append(args, beforeTS, beforeTS, beforeConv)
	}
	q := `SELECT ` + convGroupKeyExpr + `, MIN(ts), MAX(ts), COUNT(*),
		COALESCE(SUM(CASE WHEN COALESCE(status, 0) = 0 OR COALESCE(status, 0) >= 400 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0),
		SUM(credits),
		GROUP_CONCAT(DISTINCT COALESCE(NULLIF(logical_model, ''), model)),
		GROUP_CONCAT(DISTINCT COALESCE(source, '')),
		GROUP_CONCAT(DISTINCT COALESCE(account, ''))
		FROM api_logs` + where + `
		GROUP BY ` + convGroupByExpr + having + `
		ORDER BY MAX(ts) DESC, ` + convGroupKeyExpr + ` DESC LIMIT ?`
	args = append(args, limit+1)

	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, false, fmt.Errorf("store: usage conversations: %w", err)
	}
	defer rows.Close()
	out := make([]UsageConversation, 0, limit)
	for rows.Next() {
		var (
			item        UsageConversation
			credits     sql.NullFloat64
			modelsList  string
			pluginsList string
			accounts    string
		)
		if err := rows.Scan(&item.ConversationID, &item.FirstTS, &item.LastTS,
			&item.Requests, &item.Errors, &item.PromptTokens, &item.CompletionTokens,
			&credits, &modelsList, &pluginsList, &accounts); err != nil {
			return nil, false, fmt.Errorf("store: usage conversations: scan: %w", err)
		}
		if credits.Valid {
			item.Credits = credits.Float64
			item.CreditsSampled = true
		}
		item.Models = splitConcat(modelsList)
		item.Plugins = splitConcat(pluginsList)
		item.Accounts = splitConcat(accounts)
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("store: usage conversations: iterate: %w", err)
	}
	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	return out, hasMore, nil
}

// splitConcat splits a GROUP_CONCAT result, dropping empty tokens.
func splitConcat(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
