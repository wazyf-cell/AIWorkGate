package usage

// task-60 单测：SessionItem.CachedTokens 透出——store 行的 cached_tokens 经
// UsageSessions 读出原样带给前端（单条命中率 = cached/prompt 由前端算）。
// gateway 侧落库链路的断言在 internal/gateway/cached_tokens_test.go。

import (
	"net/url"
	"testing"

	"aiworkgate/internal/model"
)

func TestCachedTokensSessionsPassthrough(t *testing.T) {
	st := openTestStore(t)
	if err := st.InsertAPIRequestLog(model.APIRequestLog{
		TS: "2026-10-19T10:00:00+08:00", ReqID: "req-cache", Source: "plugin:mock",
		Account: "acc_1", Model: "m1", LogicalModel: "logical-m1", ConversationID: "conv-1",
		Status: 200, PromptTokens: 100, CompletionTokens: 10, CachedTokens: 90,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// 对照行：无缓存（cached=0），前端应显示「—」。
	if err := st.InsertAPIRequestLog(model.APIRequestLog{
		TS: "2026-10-19T11:00:00+08:00", ReqID: "req-nocache", Source: "plugin:mock",
		Account: "acc_1", Model: "m1", LogicalModel: "logical-m1", ConversationID: "conv-1",
		Status: 200, PromptTokens: 50, CompletionTokens: 5,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	svc := NewService(st)
	svc.Now = fixedNow
	q, err := ParseQuery(url.Values{"from": {"2026-10-19"}, "to": {"2026-10-19"}}, fixedNow())
	if err != nil {
		t.Fatalf("ParseQuery: %v", err)
	}
	page, err := svc.Sessions(q)
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(page.Items))
	}
	// 最新在前（ts DESC）：req-nocache 先、req-cache 后。
	byReq := map[string]CachedRow{}
	for _, item := range page.Items {
		byReq[item.ReqID] = CachedRow{Cached: item.CachedTokens, Prompt: item.PromptTokens}
	}
	if byReq["req-cache"].Cached != 90 || byReq["req-cache"].Prompt != 100 {
		t.Fatalf("req-cache 缓存字段错误: %+v", byReq["req-cache"])
	}
	if byReq["req-nocache"].Cached != 0 || byReq["req-nocache"].Prompt != 50 {
		t.Fatalf("req-nocache 缓存字段错误: %+v", byReq["req-nocache"])
	}
}

type CachedRow struct {
	Cached int64
	Prompt int64
}
