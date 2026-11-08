package gateway

// task-43 集成单测：请求侧改写（tools/tool_choice/历史轮）→ 上游收到别名；
// 响应侧回改（非流式 + SSE）→ 客户端收到原名；SetToolAlias 热替换保存即生效。

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"aiworkgate/internal/gateway/tool_alias"
	"aiworkgate/internal/model"
	"aiworkgate/internal/scheduler"
)

func TestToolAliasForwardAndReverseNonStream(t *testing.T) {
	var upstreamBody atomic.Value
	srv, _ := newUpstreamServer(t, func(w http.ResponseWriter, _ *http.Request, b []byte) {
		upstreamBody.Store(append([]byte(nil), b...))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":null,
			"tool_calls":[{"id":"call_1","type":"function","function":{"name":"task_notes","arguments":"{}"}}]}}]}`)
	})
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	snap := singlePoolSnapshot("p1", 2, "acc1")
	g := newTestGateway(t, Options{
		Plugins:   reg,
		Log:       &fakeLogSink{},
		Schedule:  scheduler.New(scheduler.Options{}),
		Snapshot:  staticSnapshot(snap),
		ToolAlias: tool_alias.Merge(nil),
	})

	body := `{"model":"p1/m1","tool_choice":"TodoWrite","tools":[
			{"type":"function","function":{"name":"TodoWrite","parameters":{}}},
			{"type":"function","function":{"name":"read_file","parameters":{}}}],
		"messages":[
			{"role":"user","content":"hi"},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"c0","type":"function","function":{"name":"TodoWrite","arguments":"{}"}}]}]}`
	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	// 上游收到别名（tools/tool_choice/历史轮三处）
	up := string(upstreamBody.Load().([]byte))
	if !strings.Contains(up, `"task_notes"`) || strings.Contains(up, `"TodoWrite"`) {
		t.Fatalf("上游应收到别名 task_notes 而非原名：%.400s", up)
	}
	if !strings.Contains(up, `"read_file"`) {
		t.Fatalf("未命中的通用名应原样透传：%s", up)
	}

	// 客户端收到原名
	got := rr.Body.String()
	if !strings.Contains(got, `"TodoWrite"`) || strings.Contains(got, `"task_notes"`) {
		t.Fatalf("客户端应收到原名 TodoWrite：%s", got)
	}
}

func TestToolAliasReverseStream(t *testing.T) {
	srv, _ := newUpstreamServer(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"task_notes","arguments":""}}]}}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: "+`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	g := newTestGateway(t, Options{
		Plugins:   reg,
		Log:       &fakeLogSink{},
		Schedule:  scheduler.New(scheduler.Options{}),
		Snapshot:  staticSnapshot(singlePoolSnapshot("p1", 2, "acc1")),
		ToolAlias: tool_alias.Merge(nil),
	})

	body := `{"model":"p1/m1","stream":true,"tools":[{"type":"function","function":{"name":"TodoWrite","parameters":{}}}],"messages":[{"role":"user","content":"hi"}]}`
	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"TodoWrite"`) || strings.Contains(rr.Body.String(), `"task_notes"`) {
		t.Fatalf("SSE 回改后客户端应收到原名：%s", rr.Body.String())
	}
	if strings.Count(rr.Body.String(), "data: ") < 3 {
		t.Fatalf("帧结构不应被破坏：%s", rr.Body.String())
	}
}

func TestToolAliasHotSwap(t *testing.T) {
	var upstreamBody atomic.Value
	srv, _ := newUpstreamServer(t, func(w http.ResponseWriter, _ *http.Request, b []byte) {
		upstreamBody.Store(append([]byte(nil), b...))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	})
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	g := newTestGateway(t, Options{
		Plugins:   reg,
		Log:       &fakeLogSink{},
		Schedule:  scheduler.New(scheduler.Options{}),
		Snapshot:  staticSnapshot(singlePoolSnapshot("p1", 2, "acc1")),
		ToolAlias: tool_alias.Merge(nil),
	})
	body := `{"model":"p1/m1","tools":[{"type":"function","function":{"name":"TodoWrite","parameters":{}}}],"messages":[{"role":"user","content":"hi"}]}`

	// 开：上游收到别名
	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if up := string(upstreamBody.Load().([]byte)); !strings.Contains(up, "task_notes") {
		t.Fatalf("启用时上游应收到别名：%s", up)
	}

	// 热替换为 nil（停用）：原名透传，响应不包回改 writer
	g.SetToolAlias(nil)
	rr = serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if up := string(upstreamBody.Load().([]byte)); !strings.Contains(up, "TodoWrite") {
		t.Fatalf("停用后上游应收到原名：%s", up)
	}

	// 热替换回启用：立即生效
	g.SetToolAlias(tool_alias.Merge(nil))
	rr = serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if up := string(upstreamBody.Load().([]byte)); !strings.Contains(up, "task_notes") {
		t.Fatalf("重新启用后上游应收到别名：%s", up)
	}
}

// TestToolAliasCustomRuleOverride pins the webui PUT path semantics: a custom
// rule overrides the built-in alias for the same from (and disables one).
func TestToolAliasCustomRuleOverride(t *testing.T) {
	tbl := tool_alias.Merge([]tool_alias.Rule{
		{From: "TodoWrite", To: "custom_notes"},
		{From: "write_to_file", To: ""},
	})
	out, reverse, changed := tbl.Forward([]byte(`{"tools":[{"type":"function","function":{"name":"TodoWrite"}}]}`))
	if !changed || reverse["custom_notes"] != "TodoWrite" {
		t.Fatalf("自定义覆盖未生效: %v %v", changed, reverse)
	}
	var tree map[string]any
	_ = json.Unmarshal(out, &tree)
	tools := tree["tools"].([]any)
	if aliasNameOfTool(tools[0]) != "custom_notes" {
		t.Fatalf("别名 = %q, want custom_notes", aliasNameOfTool(tools[0]))
	}
	// 停用规则：原名透传（changed=false）
	_, _, changed2 := tbl.Forward([]byte(`{"tools":[{"type":"function","function":{"name":"write_to_file"}}]}`))
	if changed2 {
		t.Fatalf("停用规则应原名透传")
	}
}

func aliasNameOfTool(tool any) string {
	return tool.(map[string]any)["function"].(map[string]any)["name"].(string)
}
