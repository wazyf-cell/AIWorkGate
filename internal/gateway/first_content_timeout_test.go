package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aiworkgate/internal/model"
)

// D-43 首字超时换成员（plans/2026-11-05 D-43 / D-25① 同判定点）的网关侧测试：
// upstreamFirstContentTimeout 缩短到毫秒级，上游真实挂起，不注入时钟——超时
// 窗口由 time.AfterFunc 驱动，缩短常量即可让整个失败转移在亚秒级完成。

// TestD43FirstContentTimeoutSwitchesAccount covers 上游发出响应头但不吐任何正文：
// 首字截止触发，尝试按可重试失败上报（status 0 → planRetry 走 retrySwitchAccount），
// 下一个账号成功接管。
func TestD43FirstContentTimeoutSwitchesAccount(t *testing.T) {
	withShortTimeout(t, &upstreamFirstContentTimeout, 200*time.Millisecond)

	srv, rec := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(accountHeader, r.Header.Get(accountHeader))
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// 挂起不写正文：只有账号 acc1 的尝试挂，acc2 直接成功。
		if r.Header.Get(accountHeader) == "acc1" {
			<-r.Context().Done()
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	logs := &fakeLogSink{}
	g := scheduledGateway(t, reg, logs, singlePoolSnapshot("p1", 2, "acc1", "acc2"))

	start := time.Now()
	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p1/m1","messages":[]}`))
	elapsed := time.Since(start)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want the second account's 200 (body %.200s)", rr.Code, rr.Body.String())
	}
	if got := rr.Body.String(); !strings.Contains(got, `"ok"`) {
		t.Fatalf("body = %.200s, want the acc2 answer", got)
	}
	// 两次上游调用：acc1（首字超时）→ acc2（成功）。
	if got := rec.count(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2", got)
	}
	rows := logs.wait(t, 1)
	if len(rows) == 0 || rows[0].Account != "acc2" {
		t.Fatalf("log rows = %+v, want acc2 to serve", rows)
	}
	// 首字截止（200ms）驱动整个失败转移；若误入秒级 same-account backoff
	// 会远超 2s。
	if elapsed > 2*time.Second {
		t.Fatalf("failover took %v, want a fast first-content switch", elapsed)
	}
}

// TestD43FirstContentTimeoutKeepsStreamedReply covers 已吐首字节后上游挂起：
// 正文已开始写给客户端，res.streamed 生效，绝不换号、不重放（重放会重复输出），
// 客户端拿到已开始的流并等它结束（idle 读超时兜底收尾）。
func TestD43FirstContentTimeoutKeepsStreamedReply(t *testing.T) {
	withShortTimeout(t, &upstreamIdleReadTimeout, 200*time.Millisecond)
	// 首字截止比 idle 窗口更远：第一字节在首字截止前就到了，之后挂起走 idle。
	withShortTimeout(t, &upstreamFirstContentTimeout, 500*time.Millisecond)

	srv, rec := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set(accountHeader, r.Header.Get(accountHeader))
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// 先吐一帧真实正文再挂起。
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	})
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	logs := &fakeLogSink{}
	g := scheduledGateway(t, reg, logs, singlePoolSnapshot("p1", 2, "acc1", "acc2"))

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p1/m1","stream":true,"messages":[]}`))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want the already-started stream (headers 200)", rr.Code)
	}
	if body := rr.Body.String(); !strings.Contains(body, "hi") {
		t.Fatalf("body = %.200s, want the first frame", body)
	}
	// 只有一次上游调用：正文开始后不允许任何切换/重试。
	if got := rec.count(); got != 1 {
		t.Fatalf("upstream calls = %d, want exactly 1 (no switch after first byte)", got)
	}
	rows := logs.wait(t, 1)
	if len(rows) != 1 {
		t.Fatalf("log rows = %d, want 1", len(rows))
	}
	if rows[0].Account != "acc1" {
		t.Fatalf("log account = %q, want acc1", rows[0].Account)
	}
}

// TestD43RequestAffinityHeaders covers the request-side mapping: the three
// D-43 headers map onto scheduler inputs, invalid values degrade to defaults,
// and scheduleModel passes them through (observed via the scheduler's behavior
// in the switch test above; here the parser itself).
func TestD43RequestAffinityHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if aff := requestAffinity(req); aff.Turn != 0 || aff.Within != 0 || aff.Strict {
		t.Fatalf("empty headers = %+v, want defaults", aff)
	}
	req.Header.Set("X-Conversation-Turn", "3")
	req.Header.Set("X-Affinity-Within", "600")
	req.Header.Set("X-Affinity-Strict", "true")
	aff := requestAffinity(req)
	if aff.Turn != 3 || aff.Within != 600*time.Second || !aff.Strict {
		t.Fatalf("affinity = %+v, want turn 3 / 600s window / strict", aff)
	}

	// 非法值按缺省处理，不让请求失败。
	bad := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	bad.Header.Set("X-Conversation-Turn", "-2")
	bad.Header.Set("X-Affinity-Within", "abc")
	bad.Header.Set("X-Affinity-Strict", "no")
	if aff := requestAffinity(bad); aff.Turn != 0 || aff.Within != 0 || aff.Strict {
		t.Fatalf("invalid headers = %+v, want defaults", aff)
	}
}
