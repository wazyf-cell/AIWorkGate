package tool_alias

// task-43 单测：默认表条数、规则校验、合并/停用/别名冲突、请求正向改写
// （tools/tool_choice/历史轮/碰撞防御/同别名先到先得）、响应逆向回改、热替换语义。

import (
	"encoding/json"
	"testing"
)

func TestDefaultRulesCount(t *testing.T) {
	rules := DefaultRules()
	if len(rules) != 55 {
		t.Fatalf("默认表 = %d 条, want 55", len(rules))
	}
	seen := map[string]string{}
	for _, r := range rules {
		if r.From == "" || r.To == "" {
			t.Fatalf("默认表存在空规则: %+v", r)
		}
		if r.From == r.To {
			t.Fatalf("from==to 无意义: %s", r.From)
		}
		if prev, dup := seen[r.To]; dup {
			t.Fatalf("默认表别名冲突: %s 与 %s 都映射到 %s", prev, r.From, r.To)
		}
		if prev, dup := seen["FROM:"+r.From]; dup {
			t.Fatalf("默认表 from 重复: %s（已有 %s）", r.From, prev)
		}
		seen[r.To] = r.From
		seen["FROM:"+r.From] = r.From
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		r     Rule
		valid bool
	}{
		{Rule{From: "a", To: "b"}, true},
		{Rule{From: "a", To: ""}, true}, // 空 to = 停用
		{Rule{From: "", To: "b"}, false},
		{Rule{From: "a", To: "  "}, false},
		{Rule{From: "a|b", To: "c"}, false},
		{Rule{From: "a", To: "c|d"}, false},
	}
	for _, c := range cases {
		err := Validate(c.r)
		if (err == nil) != c.valid {
			t.Fatalf("Validate(%+v) = %v, want valid=%v", c.r, err, c.valid)
		}
	}
}

func TestMergeOverrideAndDisable(t *testing.T) {
	// 自定义覆盖：TodoWrite 的别名可换；停用：write_to_file 空串移除；追加：新条目
	tbl := Merge([]Rule{
		{From: "TodoWrite", To: "custom_notes"},
		{From: "write_to_file", To: ""},
		{From: "brand_new_tool", To: "new_alias"},
	})
	froms := map[string]string{}
	for _, r := range tbl.Rules() {
		froms[r.From] = r.To
	}
	if froms["TodoWrite"] != "custom_notes" {
		t.Fatalf("自定义覆盖失效: %q", froms["TodoWrite"])
	}
	if _, alive := froms["write_to_file"]; alive {
		t.Fatalf("停用规则仍在生效表: write_to_file")
	}
	if froms["brand_new_tool"] != "new_alias" {
		t.Fatalf("追加失败: %q", froms["brand_new_tool"])
	}
	if len(tbl.Rules()) != 55 {
		t.Fatalf("合并后 = %d 条（55 - 1 停用 + 1 追加）, want 55", len(tbl.Rules()))
	}
}

func TestForwardRewritesToolsChoiceHistory(t *testing.T) {
	tbl := Merge(nil)
	body := []byte(`{
		"model": "p/m1",
		"tool_choice": "TodoWrite",
		"tools": [
			{"type": "function", "function": {"name": "TodoWrite", "parameters": {}}},
			{"type": "function", "function": {"name": "read_file", "parameters": {}}}
		],
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": null, "tool_calls": [
				{"id": "c1", "type": "function", "function_call": {"name": "TodoWrite", "arguments": "{}"}}
			]},
			{"role": "tool", "tool_call_id": "c1", "content": "ok"},
			{"role": "assistant", "content": null, "tool_calls": [
				{"id": "c2", "type": "function", "function": {"name": "write_to_file", "arguments": "{}"}}
			]}
		]
	}`)
	out, reverse, changed := tbl.Forward(body)
	if !changed {
		t.Fatal("应有改写")
	}
	if reverse["task_notes"] != "TodoWrite" || reverse["file_edit"] != "write_to_file" {
		t.Fatalf("逆映射错误: %v", reverse)
	}
	var tree map[string]any
	if err := json.Unmarshal(out, &tree); err != nil {
		t.Fatalf("产物不是合法 JSON: %v", err)
	}
	tools := tree["tools"].([]any)
	if got := aliasNameOf(tools[0]); got != "task_notes" {
		t.Fatalf("tools[0].function.name = %q, want task_notes", got)
	}
	if got := aliasNameOf(tools[1]); got != "read_file" {
		t.Fatalf("未命中的通用名不应改写: %q", got)
	}
	if tc, _ := tree["tool_choice"].(string); tc != "task_notes" {
		t.Fatalf("tool_choice = %q, want task_notes", tc)
	}
	msgs := tree["messages"].([]any)
	h1 := msgs[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if got := h1["function_call"].(map[string]any)["name"].(string); got != "task_notes" {
		t.Fatalf("历史轮 function_call.name = %q, want task_notes", got)
	}
	h2 := msgs[3].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if got := h2["function"].(map[string]any)["name"].(string); got != "file_edit" {
		t.Fatalf("历史轮 function.name = %q, want file_edit", got)
	}
}

func aliasNameOf(tool any) string {
	return tool.(map[string]any)["function"].(map[string]any)["name"].(string)
}

func TestForwardCollisionSkipsRule(t *testing.T) {
	// 碰撞防御：别名 task_notes 恰好是请求内的另一个存活真实工具名 → 该规则整条跳过，
	// tools/tool_choice/历史轮三处统一原名透传
	tbl := Merge(nil)
	body := []byte(`{
		"tool_choice": "TodoWrite",
		"tools": [
			{"type": "function", "function": {"name": "TodoWrite"}},
			{"type": "function", "function": {"name": "task_notes"}}
		],
		"messages": [{"role": "assistant", "tool_calls": [
			{"id": "c1", "type": "function", "function": {"name": "TodoWrite", "arguments": "{}"}}
		]}]
	}`)
	out, reverse, changed := tbl.Forward(body)
	if changed {
		t.Fatalf("碰撞规则应整条跳过（TodoWrite 无其他规则命中）")
	}
	if reverse != nil {
		t.Fatalf("碰撞跳过不应产出逆映射: %v", reverse)
	}
	var tree map[string]any
	_ = json.Unmarshal(out, &tree)
	tools := tree["tools"].([]any)
	if aliasNameOf(tools[0]) != "TodoWrite" || aliasNameOf(tools[1]) != "task_notes" {
		t.Fatalf("碰撞时原名透传: %v / %v", aliasNameOf(tools[0]), aliasNameOf(tools[1]))
	}
}

func TestForwardSameAliasFirstWins(t *testing.T) {
	// 自定义制造同别名冲突：a→dup 与 b→dup；构建期剔除后者，运行时只改 a
	tbl := Merge([]Rule{{From: "aaa", To: "dup"}, {From: "bbb", To: "dup"}})
	if len(tbl.Warnings()) == 0 {
		t.Fatalf("构建期应报告别名冲突")
	}
	body := []byte(`{"tools":[{"type":"function","function":{"name":"aaa"}},{"type":"function","function":{"name":"bbb"}}]}`)
	out, reverse, changed := tbl.Forward(body)
	if !changed {
		t.Fatal("应改写 aaa")
	}
	if reverse["dup"] != "aaa" {
		t.Fatalf("逆映射应先到先得 aaa: %v", reverse)
	}
	var tree map[string]any
	_ = json.Unmarshal(out, &tree)
	tools := tree["tools"].([]any)
	if aliasNameOf(tools[0]) != "dup" || aliasNameOf(tools[1]) != "bbb" {
		t.Fatalf("同别名冲突第二个原名不改写: %v / %v", aliasNameOf(tools[0]), aliasNameOf(tools[1]))
	}
}

func TestForwardNoHitPassthrough(t *testing.T) {
	tbl := Merge(nil)
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	out, reverse, changed := tbl.Forward(body)
	if changed || reverse != nil {
		t.Fatalf("无 tools 应零开销直通")
	}
	if string(out) != string(body) {
		t.Fatal("原样返回")
	}
	// 非 JSON 也不 panic
	out, _, changed = tbl.Forward([]byte("not json"))
	if changed || string(out) != "not json" {
		t.Fatal("非法 JSON 原样返回")
	}
}

func TestReverseOpenAIAndAnthropic(t *testing.T) {
	reverse := map[string]string{"task_notes": "TodoWrite", "file_edit": "write_to_file"}
	// OpenAI chunk（流式 delta）
	chunk := parseMap(t, `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"task_notes","arguments":"{}"}}]}}]}`)
	if !ReverseOpenAI(chunk, reverse) {
		t.Fatal("chunk 应有回改")
	}
	got := chunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["name"].(string)
	if got != "TodoWrite" {
		t.Fatalf("chunk 回改后 = %q, want TodoWrite", got)
	}
	// OpenAI 非流式整包
	env := parseMap(t, `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"file_edit","arguments":"{}"}}]}}]}`)
	if !ReverseOpenAI(env, reverse) {
		t.Fatal("整包应有回改")
	}
	got = env["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["name"].(string)
	if got != "write_to_file" {
		t.Fatalf("整包回改后 = %q, want write_to_file", got)
	}
	// Anthropic 流式 content_block_start
	ev := parseMap(t, `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"task_notes"}}`)
	if !ReverseAnthropic(ev, reverse) {
		t.Fatal("Anthropic 事件应有回改")
	}
	got = ev["content_block"].(map[string]any)["name"].(string)
	if got != "TodoWrite" {
		t.Fatalf("Anthropic 回改后 = %q, want TodoWrite", got)
	}
	// Anthropic 非流式 content[]
	whole := parseMap(t, `{"content":[{"type":"text","text":"hi"},{"type":"tool_use","id":"t","name":"file_edit","input":{}}]}`)
	if !ReverseAnthropic(whole, reverse) {
		t.Fatal("Anthropic 整包应有回改")
	}
	got = whole["content"].([]any)[1].(map[string]any)["name"].(string)
	if got != "write_to_file" {
		t.Fatalf("Anthropic 整包回改后 = %q, want write_to_file", got)
	}
	// 未命中的名字原样
	plain := parseMap(t, `{"choices":[{"delta":{"tool_calls":[{"function":{"name":"read_file"}}]}}]}`)
	if ReverseOpenAI(plain, reverse) {
		t.Fatal("未命中不应改动")
	}
}

func TestMergeNilIsDefault(t *testing.T) {
	tbl := Merge(nil)
	if len(tbl.Rules()) != 55 {
		t.Fatalf("纯默认表 = %d, want 55", len(tbl.Rules()))
	}
	if len(tbl.Warnings()) != 0 {
		t.Fatalf("默认表不应有冲突警告: %v", tbl.Warnings())
	}
}

func parseMap(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("fixture 不合法: %v", err)
	}
	return m
}
