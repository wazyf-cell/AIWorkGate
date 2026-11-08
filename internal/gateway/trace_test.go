package gateway

// task-48 单测：seq 全局单调、after 增量、环形淘汰、wait 挂起/超时、
// 事件字段折算（errKind/member/agent）、sticky 采集门控。

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestTraceSeqMonotonicConcurrent(t *testing.T) {
	tl := newTraceLog()
	tl.activate()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				tl.emit(TraceEvent{ReqID: "r"})
			}
		}()
	}
	wg.Wait()
	evs := tl.snapshot()
	if len(evs) != 320 {
		t.Fatalf("保留 %d 条, want 320（未达淘汰线）", len(evs))
	}
	for i := 1; i < len(evs); i++ {
		if evs[i].Seq <= evs[i-1].Seq {
			t.Fatalf("seq 非单调：%d 后跟 %d", evs[i-1].Seq, evs[i].Seq)
		}
	}
	if evs[0].Seq != 1 || evs[len(evs)-1].Seq != 320 {
		t.Fatalf("seq 范围 [%d, %d], want [1, 320]", evs[0].Seq, evs[len(evs)-1].Seq)
	}
	if tl.LastSeq() != 320 {
		t.Fatalf("LastSeq = %d, want 320", tl.LastSeq())
	}
}

func TestTraceAfterIncrement(t *testing.T) {
	tl := newTraceLog()
	tl.activate()
	for i := 0; i < 10; i++ {
		tl.emit(TraceEvent{ReqID: "r"})
	}
	if got := len(tl.afterSeq(0)); got != 10 {
		t.Fatalf("after=0 → %d 条, want 10", got)
	}
	evs := tl.afterSeq(5)
	if len(evs) != 5 || evs[0].Seq != 6 || evs[4].Seq != 10 {
		t.Fatalf("after=5 → %d 条（首 seq=%d）, want 5 条从 6 起", len(evs), firstSeq(evs))
	}
	if got := len(tl.afterSeq(10)); got != 0 {
		t.Fatalf("after=末尾 → %d 条, want 0", got)
	}
}

func TestTraceRingEviction(t *testing.T) {
	tl := newTraceLog()
	tl.activate()
	for i := 0; i < traceRingCap+20; i++ {
		tl.emit(TraceEvent{ReqID: "r"})
	}
	evs := tl.snapshot()
	if len(evs) != traceRingCap {
		t.Fatalf("保留 %d 条, want %d", len(evs), traceRingCap)
	}
	// 淘汰最旧：首个保留 seq = 21（共发出 traceRingCap+20 条）
	if evs[0].Seq != 21 {
		t.Fatalf("首条保留 seq = %d, want 21", evs[0].Seq)
	}
	if evs[len(evs)-1].Seq != int64(traceRingCap+20) {
		t.Fatalf("末条 seq = %d, want %d", evs[len(evs)-1].Seq, traceRingCap+20)
	}
	// Recent(limit) 取最新 N 条
	recent := tl.Recent(5)
	if len(recent) != 5 || recent[0].Seq != int64(traceRingCap+16) {
		t.Fatalf("Recent(5) 首条 seq = %d, want %d", firstSeq(recent), traceRingCap+16)
	}
	// after 落在被淘汰区间：返回现存全部（前端重放语义）
	all := tl.afterSeq(5)
	if len(all) != traceRingCap {
		t.Fatalf("after=5（已淘汰区间）→ %d 条, want %d", len(all), traceRingCap)
	}
}

func TestTraceWaitTimeoutAndWake(t *testing.T) {
	tl := newTraceLog()
	tl.activate()
	// ① 超时空返回
	start := time.Now()
	evs := tl.WaitAfter(context.Background(), 0, 80*time.Millisecond)
	if len(evs) != 0 {
		t.Fatalf("超时前不应有事件: %d", len(evs))
	}
	if time.Since(start) < 60*time.Millisecond {
		t.Fatalf("wait 未挂起就返回了")
	}
	// ② 有事件立即返回
	tl.emit(TraceEvent{ReqID: "r1"})
	if got := len(tl.WaitAfter(context.Background(), 0, time.Second)); got != 1 {
		t.Fatalf("已有事件应立即返回 1 条, got %d", got)
	}
	// ③ 挂起中被 emit 唤醒
	go func() {
		time.Sleep(50 * time.Millisecond)
		tl.emit(TraceEvent{ReqID: "r2"})
	}()
	evs = tl.WaitAfter(context.Background(), 1, 5*time.Second)
	if len(evs) != 1 || evs[0].ReqID != "r2" {
		t.Fatalf("挂起唤醒应收到 r2: %+v", evs)
	}
	// ④ ctx 取消
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if evs := tl.WaitAfter(ctx, 999, 5*time.Second); evs != nil {
		t.Fatalf("取消后应返回 nil: %+v", evs)
	}
}

func TestTraceStickyGate(t *testing.T) {
	tl := newTraceLog()
	// 未激活：emit 丢弃，recent 空且不 panic
	tl.emit(TraceEvent{ReqID: "dropped"})
	if got := tl.snapshot(); len(got) != 0 {
		t.Fatalf("未激活不应保留事件: %d", len(got))
	}
	// recent 访问激活（sticky）
	_ = tl.Recent(5)
	if !tl.active.Load() {
		t.Fatal("订阅后应激活采集")
	}
	tl.emit(TraceEvent{ReqID: "kept"})
	if got := len(tl.snapshot()); got != 1 {
		t.Fatalf("激活后应保留事件: %d", got)
	}
}

func TestTraceEventFields(t *testing.T) {
	rec := &reqRecord{
		reqID:        "req-1",
		keyName:      "my-key",
		model:        "m1",
		routeModel:   "org/m1",
		logicalModel: "org/m1",
		source:       "plugin:p1",
		account:      "acc1",
		status:       502,
		stream:       true,
	}
	ev := traceEventOf(rec, 1234)
	if ev.ReqID != "req-1" || ev.Agent != "my-key" || ev.LogicalModel != "org/m1" || ev.Group != "org/m1" {
		t.Fatalf("字段折算错误: %+v", ev)
	}
	if ev.Member != "p1/acc1" || ev.Status != 502 || ev.LatencyMS != 1234 || !ev.Stream {
		t.Fatalf("member/status/latency/stream 折算错误: %+v", ev)
	}
	if ev.ErrKind != "upstream" {
		t.Fatalf("err_kind = %q, want upstream", ev.ErrKind)
	}
	// errKind 分类
	cases := map[int]string{0: "transport", 499: "canceled", 500: "upstream", 400: "request", 200: "", 401: "request"}
	for status, want := range cases {
		if got := errKindOf(status); got != want {
			t.Fatalf("errKindOf(%d) = %q, want %q", status, got, want)
		}
	}
	// member 折算：无账号 / 非 plugin 前缀（source 原样保留，语义"来源/账号"）
	if got := traceMember("plugin:p2", ""); got != "p2" {
		t.Fatalf("member(无账号) = %q, want p2", got)
	}
	if got := traceMember("gateway", "acc9"); got != "gateway/acc9" {
		t.Fatalf("member(非插件来源) = %q, want gateway/acc9", got)
	}
	// 无 keyName 时回落 keyID
	rec.keyName = ""
	rec.keyID = "kid"
	if got := traceEventOf(rec, 1).Agent; got != "kid" {
		t.Fatalf("agent 回落 keyID: %q", got)
	}
}

func TestTraceGatewayHooks(t *testing.T) {
	g := newTestGateway(t, Options{
		Plugins:  newFakeRegistry(),
		Log:      &fakeLogSink{},
		Schedule: nil,
		Snapshot: nil,
	})
	if g.trace == nil {
		t.Fatal("newGateway 应创建 traceLog")
	}
	// 未激活时 TraceRecent 空且激活 sticky
	if got := len(g.TraceRecent(3)); got != 0 {
		t.Fatalf("初始应为空: %d", got)
	}
	g.trace.emit(TraceEvent{ReqID: "r"})
	if got := len(g.TraceRecent(3)); got != 1 {
		t.Fatalf("激活后应保留: %d", got)
	}
	if g.TraceLastSeq() != 1 {
		t.Fatalf("TraceLastSeq = %d, want 1", g.TraceLastSeq())
	}
	// 立即轮询（wait=false）
	g.trace.emit(TraceEvent{ReqID: "r2"})
	evs := g.TracePoll(context.Background(), 1, false, time.Second)
	if len(evs) != 1 || evs[0].ReqID != "r2" {
		t.Fatalf("TracePoll 增量错误: %+v", evs)
	}
}

func firstSeq(evs []TraceEvent) int64 {
	if len(evs) == 0 {
		return 0
	}
	return evs[0].Seq
}
