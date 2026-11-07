package store

// D-32③ 验收：keyset 分页 vs 传统 COUNT(*)+OFFSET 分组的耗时对比。
// 临时基准（回执证据用），按卡验收口径跑一次后保留（benches 不影响 go test）。
import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"aiworkgate/internal/model"
)

// openBenchStore 是 benchmark 版的 openTestStore（b 无法复用 t.Helper 的清理）。
func openBenchStore(b *testing.B) *Store {
	b.Helper()
	dir, err := os.MkdirTemp("", "aigate-bench-*")
	if err != nil {
		b.Fatalf("temp dir: %v", err)
	}
	s, err := Open(dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() {
		_ = s.Close()
		_ = os.RemoveAll(dir)
	})
	_ = filepath.Separator
	return s
}

func seedConversationBench(b *testing.B, s *Store) {
	b.Helper()
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.Local)
	// 4000 会话 × 每会话 8 请求 = 32000 行
	for conv := 0; conv < 4000; conv++ {
		for req := 0; req < 8; req++ {
			rec := model.APIRequestLog{
				TS:               base.Add(time.Duration(conv*8+req) * time.Second).Format(time.RFC3339),
				ReqID:            fmt.Sprintf("r%08d", conv*8+req),
				Source:           fmt.Sprintf("plugin:p%d", conv%3),
				Model:            "m1",
				LogicalModel:     "org/m1",
				Account:          fmt.Sprintf("acc%d", conv%5),
				ConversationID:   fmt.Sprintf("conv%06d", conv),
				Status:           200,
				PromptTokens:     10,
				CompletionTokens: 20,
			}
			if err := s.InsertAPIRequestLog(rec); err != nil {
				b.Fatalf("seed: %v", err)
			}
		}
	}
}

// BenchmarkUsageConversationsKeyset 是新路径：keyset 游标，只物化当前页。
func BenchmarkUsageConversationsKeyset(b *testing.B) {
	s := openBenchStore(b)
	seedConversationBench(b, s)
	f := UsageFilter{}
	lastTS, lastConv := "", ""
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, hasMore, err := s.UsageConversations(f, "", lastTS, lastConv, 50)
		if err != nil {
			b.Fatalf("keyset: %v", err)
		}
		if len(rows) > 0 {
			lastTS, lastConv = rows[len(rows)-1].LastTS, rows[len(rows)-1].ConversationID
		} else {
			lastTS, lastConv, hasMore = "", "", false
		}
		_ = hasMore
	}
}

// BenchmarkUsageConversationsCountOffset 是对照组：COUNT(*) 总数 + OFFSET 分组，
// 即逐请求列表现在的老分页方式套在会话聚合上。
func BenchmarkUsageConversationsCountOffset(b *testing.B) {
	s := openBenchStore(b)
	seedConversationBench(b, s)
	f := UsageFilter{}
	where, args := f.apiLogWhere()
	page := 0
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		page = (page + 1) % 40
		offset := page * 50
		ctx, cancel := s.ctx()
		var total int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM (SELECT COALESCE(conversation_id, '') AS c FROM api_logs`+where+` GROUP BY COALESCE(conversation_id, ''))`,
			args...).Scan(&total); err != nil {
			cancel()
			b.Fatalf("count: %v", err)
		}
		q := `SELECT ` + convGroupKeyExpr + `, MIN(ts), MAX(ts), COUNT(*),
			COALESCE(SUM(CASE WHEN COALESCE(status, 0) = 0 OR COALESCE(status, 0) >= 400 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0),
			SUM(credits),
			GROUP_CONCAT(DISTINCT COALESCE(NULLIF(logical_model, ''), model)),
			GROUP_CONCAT(DISTINCT COALESCE(source, '')),
			GROUP_CONCAT(DISTINCT COALESCE(account, ''))
			FROM api_logs` + where + `
			GROUP BY ` + convGroupByExpr + ` ORDER BY MAX(ts) DESC LIMIT 50 OFFSET ?`
		rows, err := s.db.QueryContext(ctx, q, append(append([]any{}, args...), offset)...)
		if err != nil {
			cancel()
			b.Fatalf("page: %v", err)
		}
		for rows.Next() {
		}
		rows.Close()
		cancel()
	}
}
