package usage

// D-32③ 会话真聚合：/api/usage/conversations 的会话级汇总。
//
// 每条 ConversationItem 对应一个 conversation_id（跨 provider 归并：同一
// 会话经不同插件/账号的请求都并入一行，models/plugins/accounts 给出分布）。
// 分页走 store 的 keyset 游标（只物化当前页，无 COUNT(*)/OFFSET）；
// cursor 是上一页末行 (last_ts, conversation_id) 的不透明编码。

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// ConversationItem is one aggregated conversation row.
type ConversationItem struct {
	ConversationID   string   `json:"conversation_id"`
	FirstTS          string   `json:"first_ts,omitempty"`
	LastTS           string   `json:"last_ts,omitempty"`
	Requests         int64    `json:"requests"`
	Errors           int64    `json:"errors"`
	PromptTokens     int64    `json:"prompt_tokens"`
	CompletionTokens int64    `json:"completion_tokens"`
	Credits          *float64 `json:"credits,omitempty"`
	// 涉及的模型/插件/账号分布（去重；无采样值的列缺省）。
	Models   []string `json:"models,omitempty"`
	Plugins  []string `json:"plugins,omitempty"`
	Accounts []string `json:"accounts,omitempty"`
}

// ConversationsResponse is the body of GET /api/usage/conversations.
// HasMore 说还有下一页；NextCursor 传回给下一次请求的 cursor 参数
// （空 = 没有下一页）。不提供总数：keyset 分页不做 COUNT(*) 全表扫。
type ConversationsResponse struct {
	Items      []ConversationItem `json:"items"`
	Page       int                `json:"page"`
	PageSize   int                `json:"page_size"`
	HasMore    bool               `json:"has_more"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

// encodeConvCursor packs a keyset cursor (last_ts \x00 conversation key).
func encodeConvCursor(lastTS, convKey string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(lastTS + "\x00" + convKey))
}

// decodeConvCursor unpacks encodeConvCursor; "" stays "".
func decodeConvCursor(cursor string) (lastTS, convKey string, err error) {
	if strings.TrimSpace(cursor) == "" {
		return "", "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(cursor))
	if err != nil {
		return "", "", fmt.Errorf("%w: cursor is not valid", ErrBadQuery)
	}
	lastTS, convKey, ok := strings.Cut(string(raw), "\x00")
	if !ok {
		return "", "", fmt.Errorf("%w: cursor is malformed", ErrBadQuery)
	}
	return lastTS, convKey, nil
}

// Conversations returns the aggregated conversation page for q.
func (s *Service) Conversations(q Query, cursor string) (ConversationsResponse, error) {
	out := ConversationsResponse{Items: []ConversationItem{}, Page: q.Page, PageSize: q.PageSize}
	if s == nil || s.Store == nil {
		return out, fmt.Errorf("usage: store is not available")
	}
	if out.Page < 1 {
		out.Page = 1
	}
	if out.PageSize < 1 {
		out.PageSize = DefaultPageSize
	}
	if out.PageSize > MaxPageSize {
		out.PageSize = MaxPageSize
	}
	beforeTS, beforeConv, err := decodeConvCursor(cursor)
	if err != nil {
		return out, err
	}
	rows, hasMore, err := s.Store.UsageConversations(q.Filter, q.Model, beforeTS, beforeConv, out.PageSize)
	if err != nil {
		return out, err
	}
	for _, row := range rows {
		item := ConversationItem{
			ConversationID:   row.ConversationID,
			FirstTS:          row.FirstTS,
			LastTS:           row.LastTS,
			Requests:         row.Requests,
			Errors:           row.Errors,
			PromptTokens:     row.PromptTokens,
			CompletionTokens: row.CompletionTokens,
			Models:           row.Models,
			Plugins:          row.Plugins,
			Accounts:         row.Accounts,
		}
		if row.CreditsSampled {
			v := row.Credits
			item.Credits = &v
		}
		out.Items = append(out.Items, item)
	}
	out.HasMore = hasMore
	if hasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		out.NextCursor = encodeConvCursor(last.LastTS, last.ConversationID)
	}
	return out, nil
}
