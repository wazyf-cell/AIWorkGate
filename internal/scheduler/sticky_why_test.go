package scheduler

import (
	"context"
	"testing"
	"time"
)

// D-43 会话亲和与 StickyWhy 七态的调度器侧测试（plans/2026-11-05 计划 D-43）。
// 全部用注入时钟（clock.at 手动推进），不真实 sleep。七态定义见 sticky_why.go。

// d43Pool builds a fixed two-account pool for the sticky tests: a always wins
// the first pick (Priority 1, higher remaining), b only serves when a is not
// selectable. Each test can then weaken a to force a re-selection.
func d43Pool() *Pool {
	a := account("a")
	a.Priority = 1
	a.Remaining, a.Total = 1000, 1000
	b := account("b")
	b.Priority = 2
	b.Remaining, b.Total = 100, 1000
	p := pool("p", a, b)
	p.Strategy = StrategyRemainingFirst
	p.ConvStickyTTL = 0 // pool-level stickiness off: the tests opt in per request via Within
	return p
}

// d43Sched builds a scheduler over one pool with the injected clock and store.
func d43Sched(clk *clock, store *memSticky) *Scheduler {
	return New(Options{Now: clk.now, Sticky: store})
}

// TestD43StickyPinnedWithinWindow covers 计划语义「同会话（会话键）在同 Within
// 窗口内 sticky 到同一账号，Turn 递增不换号」：窗口内第二次请求（Turn 2）必须
// 仍选中第一次的账号，Decision.StickyWhy = session_pinned，且 Explain 的选中行
// 带同样的七态。Within 生效于 ConvStickyTTL=0 的池（请求级亲和窗口）。
func TestD43StickyPinnedWithinWindow(t *testing.T) {
	clk := &clock{at: testBase}
	store := newMemSticky()
	s := d43Sched(clk, store)
	snap := snapOf(d43Pool())

	req := Request{Model: "p/m1", ConversationID: "conv-1", Turn: 1, Within: 600 * time.Second}
	first, err := s.Pick(context.Background(), snap, req)
	if err != nil || first.AccountID != "a" {
		t.Fatalf("first = %+v, %v", first, err)
	}
	if first.StickyWhy != StickyFirstPick {
		t.Fatalf("first sticky_why = %q, want %q", first.StickyWhy, StickyFirstPick)
	}
	if row := store.rows[stickyKey{Scope: StickyScopeConversation, Key: "conv-1"}]; row.AccountID != "a" || row.ExpiresAt != testBase+600 {
		t.Fatalf("pin row = %+v, want a pinned until first+600s", row)
	}

	// Turn 递增（第 2 轮）：必须命中 pin，不换号。
	req.Turn = 2
	second, err := s.Pick(context.Background(), snap, req)
	if err != nil {
		t.Fatal(err)
	}
	if second.AccountID != "a" || !second.Sticky {
		t.Fatalf("second = %+v, want sticky a", second)
	}
	if second.StickyWhy != StickySessionPinned {
		t.Fatalf("second sticky_why = %q, want %q", second.StickyWhy, StickySessionPinned)
	}
	// 命中不续期：窗口保持从首次选定起算。
	if row := store.rows[stickyKey{Scope: StickyScopeConversation, Key: "conv-1"}]; row.ExpiresAt != testBase+600 {
		t.Fatalf("pin expiry = %d, want unchanged %d", row.ExpiresAt, testBase+600)
	}

	// Explain 的选中行也要携带七态，供 trace 页面回答「为什么 sticky」。
	expl, err := s.Explain(context.Background(), snap, req)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range expl.Candidates {
		if c.Selected && c.AccountID == "a" {
			found = true
			if !c.Sticky || c.StickyWhy != StickySessionPinned {
				t.Fatalf("explain row = %+v, want sticky_why=%q", c, StickySessionPinned)
			}
		}
	}
	if !found {
		t.Fatal("explain has no selected row for a")
	}
}

// TestD43StickyWindowExpiredReselects covers 窗口过期：过期后丢弃 pin 重选，
// StickyWhy = window_expired，并且新选择重新落 pin。
func TestD43StickyWindowExpiredReselects(t *testing.T) {
	clk := &clock{at: testBase}
	store := newMemSticky()
	s := d43Sched(clk, store)
	p := d43Pool()
	snap := snapOf(p)

	req := Request{Model: "p/m1", ConversationID: "conv-1", Within: 300 * time.Second}
	first, err := s.Pick(context.Background(), snap, req)
	if err != nil || first.AccountID != "a" {
		t.Fatalf("first = %+v, %v", first, err)
	}

	// 窗口内 a 变差（b 剩余更多），pin 仍要赢：这证明过期判定是重选的根因。
	p.Accounts[0].Remaining = 1
	p.Accounts[1].Remaining = 900
	within, err := s.Pick(context.Background(), snap, req)
	if err != nil || within.AccountID != "a" || within.StickyWhy != StickySessionPinned {
		t.Fatalf("within-window = %+v, %v", within, err)
	}

	// 推进 301s：窗口过期（600ms 精度无关，窗口按秒比较），重新选择选 b。
	clk.advance(301)
	expired, err := s.Pick(context.Background(), snap, req)
	if err != nil {
		t.Fatal(err)
	}
	if expired.AccountID != "b" || expired.Sticky {
		t.Fatalf("expired = %+v, want fresh b without stickiness", expired)
	}
	if expired.StickyWhy != StickyWindowExpired {
		t.Fatalf("expired sticky_why = %q, want %q", expired.StickyWhy, StickyWindowExpired)
	}
	// 过期重选落了新 pin。
	if row := store.rows[stickyKey{Scope: StickyScopeConversation, Key: "conv-1"}]; row.AccountID != "b" || row.ExpiresAt != testBase+301+300 {
		t.Fatalf("fresh pin row = %+v, want b pinned until the new window", row)
	}
}

// TestD43StrictHoldsTemporarilyDownAccount covers strict=true 的临时不可用：
// pin 的账号 429 冷却中（临时），strict 仍维持亲和（StickyWhy=strict_hold）；
// 冷却结束后回到 session_pinned。非 strict 对照用第二个调度器实例（共享 store、
// 同一份冷却内存），因为非 strict Pick 会当场弃 pin 落新 pin，不能先跑再测
// strict；429 冷却是调度器内部态，用 ReportCause 在两个实例上各写一次。
func TestD43StrictHoldsTemporarilyDownAccount(t *testing.T) {
	clk := &clock{at: testBase}
	store := newMemSticky()
	s := d43Sched(clk, store)
	p := d43Pool()
	p.ConvStickyTTL = 900 // strict hold 测试走池配置的窗口，不用 Within
	snap := snapOf(p)
	key := AccountKey{PluginID: "p", AccountID: "a"}

	req := Request{Model: "p/m1", ConversationID: "conv-1", Strict: true}
	first, err := s.Pick(context.Background(), snap, req)
	if err != nil || first.AccountID != "a" {
		t.Fatalf("first = %+v, %v", first, err)
	}

	// a 上报 429（临时冷却 30s）；对照实例也记录同一次失败（上游只答了一次，
	// 两个调度器视角都要看到它冷却）。
	s.Report(key, "m1", 429, 0, 0)

	// 非 strict 对照（独立调度器、共享 store）：冷却中的 a 被弃，换 b，
	// StickyWhy=account_down。它同时会把 store 里的 pin 换到 b——s 的内存
	// 缓存不受影响，因此后面的 strict Pick 仍持有 a 的 pin。
	nonStrict := d43Sched(clk, store)
	nonStrict.Report(key, "m1", 429, 0, 0)
	nonStrictPick, err := nonStrict.Pick(context.Background(), snap, Request{Model: "p/m1", ConversationID: "conv-1"})
	if err != nil {
		t.Fatal(err)
	}
	if nonStrictPick.AccountID != "b" || nonStrictPick.StickyWhy != StickyAccountDown {
		t.Fatalf("non-strict cooldown pick = %+v sticky_why=%q, want b/%q",
			nonStrictPick, nonStrictPick.StickyWhy, StickyAccountDown)
	}

	// strict 下同一个冷却中的 a 仍被选中：亲和优先于临时不可用。
	strictPick, err := s.Pick(context.Background(), snap, req)
	if err != nil {
		t.Fatal(err)
	}
	if strictPick.AccountID != "a" || strictPick.StickyWhy != StickyStrictHold {
		t.Fatalf("strict pick = %+v sticky_why=%q, want a/%q",
			strictPick, strictPick.StickyWhy, StickyStrictHold)
	}
	if reason := strictPick.Reasons[len(strictPick.Reasons)-1]; reason != "sticky strict hold: pinned account temporarily unavailable" {
		t.Fatalf("strict reason = %q", reason)
	}
	// strict hold（pin 命中而非过期）不重写 pin：亲和靠内存/存储里已有的
	// pin 维持。共享 store 已被对照实例换到 b，这是设计内的暂时偏差——
	// s 的内存缓存仍持有 a，直到下一次窗口过期才由 strictRepin 重写。
	s.mu.Lock()
	mem := s.stickyEntries[stickyKey{Scope: StickyScopeConversation, Key: "conv-1"}]
	s.mu.Unlock()
	if mem.AccountID != "a" {
		t.Fatalf("in-memory pin = %+v, want a", mem)
	}

	// 冷却结束后回 session_pinned。
	clk.advance(31)
	after, err := s.Pick(context.Background(), snap, req)
	if err != nil {
		t.Fatal(err)
	}
	if after.AccountID != "a" || after.StickyWhy != StickySessionPinned {
		t.Fatalf("after cooldown = %+v sticky_why=%q, want a/%q",
			after, after.StickyWhy, StickySessionPinned)
	}
}

// TestD43StrictWindowExpiryRepins covers strict=true 下窗口过期：亲和使用过期
// pin 的账号再维持一个窗口（StickyWhy=strict_hold，reasons 说明窗口过期），
// 且 persistSticky 重新落 pin（过期行已被懒删除，不重写就断了亲和链）。
func TestD43StrictWindowExpiryRepins(t *testing.T) {
	clk := &clock{at: testBase}
	store := newMemSticky()
	s := d43Sched(clk, store)
	p := d43Pool()
	p.ConvStickyTTL = 300
	snap := snapOf(p)

	req := Request{Model: "p/m1", ConversationID: "conv-1", Strict: true}
	first, err := s.Pick(context.Background(), snap, req)
	if err != nil || first.AccountID != "a" {
		t.Fatalf("first = %+v, %v", first, err)
	}

	clk.advance(301)
	held, err := s.Pick(context.Background(), snap, req)
	if err != nil {
		t.Fatal(err)
	}
	if held.AccountID != "a" || held.StickyWhy != StickyStrictHold {
		t.Fatalf("strict expired hold = %+v sticky_why=%q, want a/%q",
			held, held.StickyWhy, StickyStrictHold)
	}
	if row := store.rows[stickyKey{Scope: StickyScopeConversation, Key: "conv-1"}]; row.AccountID != "a" || row.ExpiresAt != testBase+301+300 {
		t.Fatalf("repin row = %+v, want a re-pinned for a fresh window", row)
	}

	// 非 strict 的同场景走 window_expired（对照；池内 a 剩余仍最高，重选回 a）。
	clk.advance(301)
	expired, err := s.Pick(context.Background(), snap, Request{Model: "p/m1", ConversationID: "conv-1"})
	if err != nil {
		t.Fatal(err)
	}
	if expired.StickyWhy != StickyWindowExpired || expired.Sticky {
		t.Fatalf("non-strict expired = %+v sticky_why=%q, want non-sticky/%q",
			expired, expired.StickyWhy, StickyWindowExpired)
	}
}

// TestD43StickyPoolChangedAndNoSession covers 剩余两态：
//   - pin 的池不在本轮候选里 → pool_changed，亲和不跨池。场景：逻辑绑定两池，
//     pin 落在 pa 上，pa 被禁用后候选只剩 pb。
//   - 无会话键 → no_session。
func TestD43StickyPoolChangedAndNoSession(t *testing.T) {
	clk := &clock{at: testBase}
	store := newMemSticky()
	s := d43Sched(clk, store)
	pa := d43Pool()
	pa.PluginID, pa.Name = "pa", "pa"
	pa.ConvStickyTTL = 600
	pb := d43Pool()
	pb.PluginID, pb.Name = "pb", "pb"
	pb.ConvStickyTTL = 600 // pb 也要开亲和，否则 pool_changed 换池后不会落新 pin
	snap := snapOf(pa, pb)
	snap.Bindings["l"] = Binding{Logical: "l", Items: []Source{
		{PluginID: "pa", Model: "m1"}, {PluginID: "pb", Model: "m1"},
	}}

	req := Request{Model: "l", ConversationID: "conv-1"}
	first, err := s.Pick(context.Background(), snap, req)
	if err != nil || first.AccountID != "a" || first.PluginID != "pa" {
		t.Fatalf("first = %+v, %v", first, err)
	}

	// pa 不可用（上游禁用）：候选只剩 pb，亲和不跨池，丢弃 pin 换到 pb。
	pa.UpstreamEnabled = false
	changed, err := s.Pick(context.Background(), snap, req)
	if err != nil {
		t.Fatal(err)
	}
	if changed.AccountID != "a" || changed.PluginID != "pb" || changed.Sticky {
		t.Fatalf("pool changed = %+v, want pb without stickiness", changed)
	}
	if changed.StickyWhy != StickyPoolChanged {
		t.Fatalf("pool changed sticky_why = %q, want %q", changed.StickyWhy, StickyPoolChanged)
	}
	// pa 的旧 pin 已被丢弃，pb 落了新 pin。
	if _, ok := store.rows[stickyKey{Scope: StickyScopeConversation, Key: "conv-1"}]; !ok {
		t.Fatal("no pin row after the pool change; want a fresh pin on pb")
	}

	// 无会话键的请求：no_session。
	plain, err := s.Pick(context.Background(), snap, Request{Model: "l"})
	if err != nil {
		t.Fatal(err)
	}
	if plain.Sticky || plain.StickyWhy != StickyNoSession {
		t.Fatalf("plain pick = %+v sticky_why=%q, want no stickiness/%q",
			plain, plain.StickyWhy, StickyNoSession)
	}

	// pa 恢复后重选：pb 的 pin 仍有效，亲和保持（pin 不因池恢复而迁移）。
	pa.UpstreamEnabled = true
	back, err := s.Pick(context.Background(), snap, req)
	if err != nil {
		t.Fatal(err)
	}
	if back.PluginID != "pb" || back.AccountID != "a" || back.StickyWhy != StickySessionPinned {
		t.Fatalf("after pool recovery = %+v sticky_why=%q, want the pb pin kept",
			back, back.StickyWhy)
	}
}

// TestD43StickyStrictPermanentLossSwitches covers strict 的唯一让位条件：账号
// 永久不可用（disabled / no credential / not a member / 不在池快照里）也换号。
func TestD43StickyStrictPermanentLossSwitches(t *testing.T) {
	clk := &clock{at: testBase}
	store := newMemSticky()
	s := d43Sched(clk, store)
	p := d43Pool()
	p.ConvStickyTTL = 900
	snap := snapOf(p)

	req := Request{Model: "p/m1", ConversationID: "conv-1", Strict: true}
	first, err := s.Pick(context.Background(), snap, req)
	if err != nil || first.AccountID != "a" {
		t.Fatalf("first = %+v, %v", first, err)
	}

	// a 被禁用（永久）：strict 也不得不换。
	p.Accounts[0].Enabled = false
	switched, err := s.Pick(context.Background(), snap, req)
	if err != nil {
		t.Fatal(err)
	}
	if switched.AccountID != "b" || switched.StickyWhy != StickyAccountDown {
		t.Fatalf("permanent loss = %+v sticky_why=%q, want b/%q",
			switched, switched.StickyWhy, StickyAccountDown)
	}
	// 换号后落新 pin 到 b。
	if row := store.rows[stickyKey{Scope: StickyScopeConversation, Key: "conv-1"}]; row.AccountID != "b" {
		t.Fatalf("pin row = %+v, want b", row)
	}
}
