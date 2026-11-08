// 问题6A 地基（task-48）：gateway 逐请求 trace 事件流 + 长轮询。
//
// 定位：旁路观察者。emit() 收束点是全请求唯一出口（所有协议面/成功失败都走），
// 在那里一条完整事件（单事件方案，字段一次齐；三态可由前端按 reqID 的时间线
// 后续演化）。采集是 sticky 门控：任何订阅者（poll/recent 访问）出现即开启并
// 保持到进程结束——环形缓冲固定上限，内存恒定（500 条 × ~200B），无增长风险；
// 未开启时 emit 只付一次 atomic.Bool 读的代价。
//
// 并发模型：互斥锁保护环形与 seq（emit 在请求收束路径，QPS 千级下锁竞争可忽略；
// 写路径无重 IO，临界区是纯内存切片操作）。waiter 用 channel 唤醒，不忙等。
//
// 契约（task-49 前端照此实现，字段改动需同步本文件与本回执）：
//
//	GET /api/gateway/trace/recent?limit=N      → {"events":[TraceEvent…],"last_seq":N}
//	GET /api/gateway/trace?after=<seq>&wait=1  → 同形状；wait=1 且无新事件时挂起
//	                                             至多 tracePollWait 再空返回
//	TraceEvent:
//	  seq          int64  全局单调（从 1 开始；重启归零，前端以 last_seq 重置）
//	  ts           int64  unix 毫秒
//	  req_id       string
//	  agent        string 客户端 key 名（key_name，缺 key 用 key_id）
//	  logical_model string 逻辑模型（空 = 原样模型名）
//	  group        string 命中的路由组 = 逻辑模型（现无独立组 id，语义对齐路由层）
//	  member       string 实际选中成员 "插件/账号"（账号空时只有插件）
//	  status       int    最终响应码（0 = 上游不可达）
//	  latency_ms   int64
//	  stream       bool
//	  err_kind     string "" 成功；"transport" status=0；"canceled" 499；
//	                      "upstream" 5xx；"request" 其它 4xx
package gateway

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// traceRingCap 是环形缓冲保留的事件数；淘汰最旧的。
const traceRingCap = 500

// tracePollWait 是 wait=1 长轮询的默认挂起上限；tracePollWaitMax 是上限封顶。
const (
	tracePollWait    = 25 * time.Second
	tracePollWaitMax = 30 * time.Second
)

// TraceEvent 是一条逐请求观察事件（字段契约见包注释，task-49 前端照此消费）。
type TraceEvent struct {
	Seq          int64  `json:"seq"`
	TS           int64  `json:"ts"`
	ReqID        string `json:"req_id"`
	Agent        string `json:"agent,omitempty"`
	LogicalModel string `json:"logical_model,omitempty"`
	Group        string `json:"group,omitempty"`
	Member       string `json:"member,omitempty"`
	Status       int    `json:"status"`
	LatencyMS    int64  `json:"latency_ms"`
	Stream       bool   `json:"stream"`
	ErrKind      string `json:"err_kind,omitempty"`
}

// traceWaiter 是一个挂起中的长轮询；emit 广播唤醒。
type traceWaiter struct {
	wake chan struct{}
}

type traceLog struct {
	mu      sync.Mutex
	ring    []TraceEvent // 定长环形；head 为下一写入槽
	head    int
	count   int   // 当前保留条数（≤ cap）
	lastSeq int64 // 最后发出的 seq；0 = 尚无事件
	waiters map[*traceWaiter]struct{}
	// active 是 sticky 采集开关（atomic：emit 热路径只付一次原子读）。
	// Lock 顺序：先读 active（无锁），命中才进 mu。
	active atomic.Bool
}

func newTraceLog() *traceLog {
	return &traceLog{waiters: map[*traceWaiter]struct{}{}}
}

// activate 开启采集（sticky）。任何订阅路径进入时调用。
func (t *traceLog) activate() {
	t.active.Store(true)
}

// emit 追加一条事件并唤醒挂起的轮询。未激活时直接丢弃（一次原子读的旁路成本）。
func (t *traceLog) emit(ev TraceEvent) {
	if !t.active.Load() {
		return
	}
	t.mu.Lock()
	if !t.active.Load() {
		t.mu.Unlock()
		return
	}
	t.lastSeq++
	ev.Seq = t.lastSeq
	if len(t.ring) < traceRingCap {
		t.ring = append(t.ring, ev)
		t.count++
	} else {
		t.ring[t.head] = ev
		t.head = (t.head + 1) % traceRingCap
	}
	// 唤醒挂起中的轮询（非阻塞 close 广播）
	for w := range t.waiters {
		close(w.wake)
		delete(t.waiters, w)
	}
	t.mu.Unlock()
}

// snapshot 返回当前保留的事件（旧→新，与 seq 升序一致）。
func (t *traceLog) snapshot() []TraceEvent {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]TraceEvent, 0, t.count)
	if t.count < traceRingCap {
		out = append(out, t.ring[:t.count]...)
		return out
	}
	out = append(out, t.ring[t.head:]...)
	out = append(out, t.ring[:t.head]...)
	return out
}

// afterSeq 返回 seq 之后（不含）的事件；after=0 = 全部保留历史。
func (t *traceLog) afterSeq(after int64) []TraceEvent {
	var out []TraceEvent
	for _, ev := range t.snapshot() {
		if ev.Seq > after {
			out = append(out, ev)
		}
	}
	return out
}

// Recent 返回最近 limit 条（limit ≤ 0 = 全部保留历史）。
func (t *traceLog) Recent(limit int) []TraceEvent {
	t.activate()
	all := t.snapshot()
	if limit > 0 && len(all) > limit {
		return all[len(all)-limit:]
	}
	return all
}

// LastSeq 返回最后发出的事件 seq（0 = 无事件）。
func (t *traceLog) LastSeq() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastSeq
}

// WaitAfter 挂起直到 seq 之后出现新事件或超时/取消；返回增量（可为空）。
// 进入即开启采集（sticky），保证挂起期间 emit 不被丢弃。
func (t *traceLog) WaitAfter(ctx context.Context, after int64, timeout time.Duration) []TraceEvent {
	t.activate()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		if evs := t.afterSeq(after); len(evs) > 0 {
			return evs
		}
		w := &traceWaiter{wake: make(chan struct{})}
		t.mu.Lock()
		// 双检：注册前可能刚好有新事件
		if t.lastSeq > after {
			t.mu.Unlock()
			return t.afterSeq(after)
		}
		t.waiters[w] = struct{}{}
		t.mu.Unlock()
		select {
		case <-w.wake:
		case <-deadline.C:
			t.mu.Lock()
			delete(t.waiters, w)
			t.mu.Unlock()
			return t.afterSeq(after)
		case <-ctx.Done():
			t.mu.Lock()
			delete(t.waiters, w)
			t.mu.Unlock()
			return nil
		}
	}
}

// errKindOf 把请求结局折算成 err_kind（契约见包注释）。
func errKindOf(status int) string {
	switch {
	case status == 0:
		return "transport"
	case status == 499:
		return "canceled"
	case status >= 500:
		return "upstream"
	case status >= 400:
		return "request"
	}
	return ""
}

// traceMember 把 source/account 折算成 member（"插件/账号"）。
func traceMember(source, account string) string {
	plugin := strings.TrimPrefix(source, "plugin:")
	plugin = strings.TrimPrefix(plugin, "custom:")
	switch {
	case plugin != "" && account != "":
		return plugin + "/" + account
	case plugin != "":
		return plugin
	default:
		return account
	}
}

// traceEventOf 从 reqRecord 折算一条事件（emit 收束点调用，字段全齐）。
func traceEventOf(rec *reqRecord, latencyMS int64) TraceEvent {
	agent := rec.keyName
	if agent == "" {
		agent = rec.keyID
	}
	group := rec.routeModel
	if group == "" {
		group = rec.model
	}
	return TraceEvent{
		TS:           time.Now().UnixMilli(),
		ReqID:        rec.reqID,
		Agent:        agent,
		LogicalModel: rec.logicalModel,
		Group:        group,
		Member:       traceMember(rec.source, rec.account),
		Status:       rec.status,
		LatencyMS:    latencyMS,
		Stream:       rec.stream,
		ErrKind:      errKindOf(rec.status),
	}
}

// ---------------------------------------------------------------- gateway hooks

// TraceRecent 是 recent 回放端点的宿主入口（最近 limit 条；limit ≤ 0 = 全部）。
func (g *Gateway) TraceRecent(limit int) []TraceEvent {
	if g == nil || g.trace == nil {
		return []TraceEvent{}
	}
	return g.trace.Recent(limit)
}

// TraceLastSeq 是当前最后事件 seq（前端 after 游标初始化用）。
func (g *Gateway) TraceLastSeq() int64 {
	if g == nil || g.trace == nil {
		return 0
	}
	return g.trace.LastSeq()
}

// TracePoll 是长轮询端点的宿主入口：wait=false 立即返回增量（无新事件空返回）；
// wait=true 挂起至多 timeout（缺省/超上限 = tracePollWait）再空返回。
func (g *Gateway) TracePoll(ctx context.Context, after int64, wait bool, timeout time.Duration) []TraceEvent {
	if g == nil || g.trace == nil {
		return []TraceEvent{}
	}
	if !wait {
		return g.trace.WaitAfter(ctx, after, 0)
	}
	if timeout <= 0 || timeout > tracePollWaitMax {
		timeout = tracePollWait
	}
	return g.trace.WaitAfter(ctx, after, timeout)
}
