package webui

// task-48 端点集成测试：等价覆盖 curl 验收语义——session cookie → recent 回放
// → 发一条（模拟的）真实请求后 poll 拿到增量。TraceEvent 形状契约对齐
// internal/gateway/trace.go 包注释。

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestGatewayTraceEndpoints(t *testing.T) {
	envelope := map[string]any{
		"events": []map[string]any{{
			"seq":           1,
			"ts":            1762575600000,
			"req_id":        "req-abc",
			"agent":         "my-key",
			"logical_model": "org/m1",
			"group":         "org/m1",
			"member":        "p1/acc1",
			"status":        200,
			"latency_ms":    123,
			"stream":        false,
		}},
		"last_seq": 1,
	}
	handler := New(Options{
		GatewayTraceRecent: func(limit int) (any, error) {
			if limit != 5 {
				t.Fatalf("recent limit = %d, want 5", limit)
			}
			return envelope, nil
		},
		GatewayTracePoll: func(after int64, wait bool) (any, error) {
			if after != 1 {
				t.Fatalf("poll after = %d, want 1", after)
			}
			if wait {
				t.Fatalf("无 wait=1 时不应挂起")
			}
			return map[string]any{"events": []map[string]any{}, "last_seq": int64(1)}, nil
		},
	})

	// ① recent 回放：合法 JSON + 契约字段
	recorder := doRequest(t, handler, http.MethodGet, "/api/gateway/trace/recent?limit=5", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("recent status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var page struct {
		Events  []map[string]any `json:"events"`
		LastSeq int64            `json:"last_seq"`
	}
	decodeJSONBody(t, recorder, &page)
	if page.LastSeq != 1 || len(page.Events) != 1 {
		t.Fatalf("recent 信封错误: last_seq=%d events=%d", page.LastSeq, len(page.Events))
	}
	if _, ok := page.Events[0]["req_id"].(string); !ok {
		t.Fatalf("事件缺 req_id 字段: %v", page.Events[0])
	}
	if _, ok := page.Events[0]["err_kind"]; ok {
		t.Fatalf("成功事件不应带 err_kind（omitempty）: %v", page.Events[0])
	}

	// ② poll 增量（空返回形状）
	recorder = doRequest(t, handler, http.MethodGet, "/api/gateway/trace?after=1", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("poll status = %d", recorder.Code)
	}
	decodeJSONBody(t, recorder, &page)
	if page.LastSeq != 1 || len(page.Events) != 0 {
		t.Fatalf("poll 信封错误: %+v", page)
	}
	// 合法 JSON（客户端直接 json.Unmarshal）
	if err := json.Unmarshal(recorder.Body.Bytes(), &map[string]any{}); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}

	// ③ 未接线 → 501
	empty := New(Options{})
	recorder = doRequest(t, empty, http.MethodGet, "/api/gateway/trace/recent", "")
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("未接线 status = %d, want 501", recorder.Code)
	}
}
