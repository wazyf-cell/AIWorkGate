package gateway

// task-48 集成测试：一条真实 chat 请求（上游 200）后 trace 里出现对应事件，
// 字段与 reqRecord 折算一致（等价 curl 验收：recent 回放 + poll 增量）。

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"aiworkgate/internal/model"
	"aiworkgate/internal/scheduler"
)

func TestTraceChatRequestEmitsEvent(t *testing.T) {
	srv, calls := newUpstreamServer(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	})
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	g := newTestGateway(t, Options{
		Plugins:   reg,
		Log:       &fakeLogSink{},
		Schedule:  scheduler.New(scheduler.Options{}),
		Snapshot:  staticSnapshot(singlePoolSnapshot("p1", 2, "acc1")),
	})
	// 首屏回放访问 → sticky 激活（前端订阅驱动语义）
	if got := len(g.TraceRecent(10)); got != 0 {
		t.Fatalf("激活前应为空: %d", got)
	}

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"p1/m1","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if calls.count() != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls.count())
	}

	// recent 回放：一条事件，字段齐
	evs := g.TraceRecent(10)
	if len(evs) != 1 {
		t.Fatalf("trace 事件 = %d 条, want 1", len(evs))
	}
	ev := evs[0]
	if ev.ReqID == "" {
		t.Fatal("事件缺 req_id")
	}
	if ev.Status != 200 || ev.Stream || ev.ErrKind != "" {
		t.Fatalf("status/stream/err_kind 折算错误: %+v", ev)
	}
	if ev.Member == "" || !strings.Contains(ev.Member, "p1") {
		t.Fatalf("member 应含插件 id: %q", ev.Member)
	}
	if ev.LatencyMS < 0 {
		t.Fatalf("latency 异常: %d", ev.LatencyMS)
	}
	if ev.Seq != 1 {
		t.Fatalf("seq = %d, want 1", ev.Seq)
	}

	// poll 增量：after=1 → 空（无新事件）；新请求后 → 1 条
	if got := g.TracePoll(t.Context(), ev.Seq, false, 0); len(got) != 0 {
		t.Fatalf("after=末尾 poll 应为空: %+v", got)
	}
	rr2 := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"p1/m1","messages":[{"role":"user","content":"again"}]}`))
	if rr2.Code != http.StatusOK {
		t.Fatalf("second status = %d", rr2.Code)
	}
	got := g.TracePoll(t.Context(), ev.Seq, false, 0)
	if len(got) != 1 || got[0].Seq != ev.Seq+1 {
		t.Fatalf("poll 增量错误: %+v", got)
	}
}
