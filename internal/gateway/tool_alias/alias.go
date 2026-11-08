// Package tool_alias 移植自 Trae（TraeWorkAssistant, MIT, 朱天伟）
// src-tauri/src/api_server/wb_payload.rs:511-829 / sse.rs:170-180（issue #150/#158）。
//
// 目的（指纹清洗）：客户端 SDK 的独家工具名是强指纹（如 Claude Code 的
// camelCase 连写、Cline 的双编辑器拆分、DSH 的 team_task_* 家族）。网关在
// 转发前把 tools[].function.name / tool_choice / 历史 tool_calls 的命中名
// 逐字改写为语义保持的别名，上游看到的是别名；响应侧按请求作用域的逆映射
// 把别名换回原名——客户端按名匹配 tool_calls，单向改名即废。
//
// 语义逐条对齐 Trae：
//   · 逐字精确匹配（按真实注册名大小写），命中才改（窄口径）；
//   · 别名非空（空 to = 停用该规则，原名透传）；
//   · 双向一致：不支持 `|` 多候选——候选随轮换种子变化会让响应侧逆映射失配；
//   · 碰撞防御：别名命中请求内其他存活真实工具名（且该名未被更早规则让出）
//     时整条跳过，tools/tool_choice/历史轮三处统一原名透传；
//   · 同别名先到先得：两条规则映射到同一别名时第二个原名不改写；
//   · 无 tools / 零命中 = 原样返回（响应侧零开销直通）。
package tool_alias

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Rule 一条别名规则：from 逐字精确匹配真实工具名，to 为别名（非空）。
type Rule struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// File 是持久化形态（webui GET/PUT 的 body；对齐 Trae ToolAliasFile）。
// Enabled=false 时网关整表停用（原名透传）。UpdatedAt unix 秒。
type File struct {
	Enabled   bool   `json:"enabled"`
	Rules     []Rule `json:"rules"`
	UpdatedAt int64  `json:"updated_at,omitempty"`
}

// DefaultRules 是内置默认表：55 条，逐条移植自 wb_payload.rs:511-629
// （Trae SOLO/CN、Claude Code、Cline/Roo、DSH、CodeBuddy、Qoder、Codex 七段）。
func DefaultRules() []Rule {
	return []Rule{
		// ==================== Trae SOLO 独家（v3.8.5 首发实证） ====================
		{From: "ask_user_question", To: "ask_user"},
		{From: "create_goal", To: "set_goal"},
		{From: "exit_plan_mode", To: "finish_plan"},
		{From: "get_goal", To: "read_goal"},
		{From: "interrupt_agent", To: "pause_agent"},
		{From: "job_kill", To: "stop_job"},
		{From: "job_list", To: "list_jobs"},
		// Trae CN 1.107.1 补两条：camelCase/长蛇形独家名，别名拆 snake_case 语义保持
		{From: "PureShowWidget", To: "show_widget"},
		{From: "browser_waiting_for_user_interaction", To: "wait_for_user"},
		// ==================== Claude Code 独家 ====================
		// camelCase 多词连写是 Claude Code 工具名强特征；别名反向用 snake_case
		{From: "TodoWrite", To: "task_notes"},
		{From: "AskUserQuestion", To: "user_input"},
		{From: "ListAgents", To: "list_subagents"},
		{From: "ReportFindings", To: "summarize_results"},
		{From: "ScheduleWakeup", To: "set_reminder"},
		{From: "ToolSearch", To: "find_tools"},
		{From: "DeferredToolPlaceholder", To: "reserved_tool_slot"},
		{From: "Workflow", To: "pipeline"},
		// ==================== Cline / Roo 独家 ====================
		{From: "write_to_file", To: "file_edit"},
		{From: "replace_in_file", To: "file_patch"},
		{From: "attempt_completion", To: "present_result"},
		{From: "ask_followup_question", To: "user_reply"},
		// ==================== DSH 独家（0.2.1-rc.2 抓包实证） ====================
		// subagent_fork 的别名不可取 subagent（DSH 同时注册两者，会触发碰撞防御）
		{From: "subagent_fork", To: "spawn_subagent"},
		{From: "job_output", To: "read_job_log"},
		{From: "pwsh", To: "run_shell"},
		{From: "working_directory", To: "work_dir"},
		// issue #195：DSH ACP 桥接三件（acp_ 前缀独家命名）
		{From: "acp_cache", To: "tool_cache"},
		{From: "acp_read_original_image", To: "read_source_image"},
		{From: "acp_status", To: "runtime_status"},
		// DSH 0.2.1-rc.3 补十一条（141 工具抓包）：Cordis 内省、Agent Teams、插件管理
		{From: "cordis_inspect_list", To: "ext_inspect_list"},
		{From: "cordis_inspect_query", To: "ext_inspect_query"},
		{From: "list_agents", To: "list_teammates"},
		{From: "spawn_teammate", To: "add_teammate"},
		{From: "wait_agent", To: "await_teammate"},
		{From: "team_task_create", To: "new_shared_task"},
		{From: "team_task_get", To: "read_shared_task"},
		{From: "team_task_list", To: "list_shared_tasks"},
		{From: "team_task_update", To: "update_shared_task"},
		{From: "plugin_manager", To: "extension_manager"},
		{From: "load_workspace_dependencies", To: "load_deps"},
		// ==================== CodeBuddy 独家（1.106.1 抓包实证） ====================
		{From: "read_lints", To: "check_lints"},
		{From: "execute_command", To: "run_command"},
		{From: "update_memory", To: "save_memory"},
		{From: "use_skill", To: "load_skill"},
		{From: "read_rules", To: "load_rules"},
		{From: "send_message", To: "notify_user"},
		{From: "DeferExecuteTool", To: "defer_execute"},
		// ==================== Qoder 独家（0.4.11 混淆包逆向实证） ====================
		{From: "mcp_list", To: "tool_catalog"},
		{From: "mcp_get", To: "tool_schema"},
		{From: "mcp_call", To: "tool_invoke"},
		{From: "SendMessage", To: "relay_message"},
		{From: "TaskCreate", To: "queue_task"},
		// ==================== Codex 独家 ====================
		{From: "update_goal", To: "refresh_goal"},
		{From: "request_user_input", To: "request_input"},
		{From: "apply_patch", To: "patch_source"},
		{From: "update_plan", To: "refresh_plan"},
	}
}

// Validate 校验一条自定义规则。to 为空 = 停用（合法，语义是移除该 from 的改写）；
// 多候选 `|` 一律拒绝（双向一致性约束）。
func Validate(r Rule) error {
	if strings.TrimSpace(r.From) == "" {
		return fmt.Errorf("tool_alias: from 不能为空")
	}
	if strings.ContainsAny(r.From, "|\r\n") || strings.ContainsAny(r.To, "|\r\n") {
		return fmt.Errorf("tool_alias: 规则不支持多候选 |（from=%q）", r.From)
	}
	if r.To != "" && strings.TrimSpace(r.To) == "" {
		return fmt.Errorf("tool_alias: to 只能是非空别名或空串（停用），不能是纯空白（from=%q）", r.From)
	}
	return nil
}

// Table 是生效表：内置默认表全量 + 自定义按 from 覆盖/追加/停用。
// 构造时完成碰撞预检（两条规则的 to 相同 → 后者按配置冲突跳过并记 warnings，
// Trae 语义是运行时先到先得——构建期先把确定冲突的剔除，运行时防御保留兜底）。
type Table struct {
	rules    []Rule
	warnings []string
}

// Merge 构造生效表。custom 为 nil/空 = 纯内置默认表；to 为空串 = 停用该 from。
func Merge(custom []Rule) *Table {
	merged := DefaultRules()
	apply := func(from, to string) {
		if strings.TrimSpace(to) == "" {
			// 停用：从生效表移除
			for i, r := range merged {
				if r.From == from {
					merged = append(merged[:i], merged[i+1:]...)
					return
				}
			}
			return
		}
		for i := range merged {
			if merged[i].From == from {
				merged[i].To = to
				return
			}
		}
		merged = append(merged, Rule{From: from, To: to})
	}
	for _, r := range custom {
		if err := Validate(r); err != nil {
			continue // 校验不过的自定义条目直接丢弃（webui PUT 层已先行校验拒绝）
		}
		apply(r.From, r.To)
	}
	t := &Table{rules: merged}
	// 同别名冲突预检：保留先到者，后到者剔除（ Trae 运行时先到先得的构建期等价）
	seen := map[string]string{} // to → from
	var out []Rule
	for _, r := range merged {
		if prev, dup := seen[r.To]; dup {
			t.warnings = append(t.warnings, fmt.Sprintf("别名冲突：%s 与 %s 都映射到 %s，保留 %s", prev, r.From, r.To, prev))
			continue
		}
		seen[r.To] = r.From
		out = append(out, r)
	}
	t.rules = out
	return t
}

// Rules 返回生效表（只读副本）。
func (t *Table) Rules() []Rule {
	out := make([]Rule, len(t.rules))
	copy(out, t.rules)
	return out
}

// Warnings 返回构建期冲突说明。
func (t *Table) Warnings() []string { return t.warnings }

// forwardMap 返回 from→to 生效映射（空表 nil）。
func (t *Table) forwardMap() map[string]string {
	if t == nil || len(t.rules) == 0 {
		return nil
	}
	m := make(map[string]string, len(t.rules))
	for _, r := range t.rules {
		if r.To != "" {
			m[r.From] = r.To
		}
	}
	return m
}

// Forward 把请求体里的命中工具名改写为别名（wb_payload.rs:714-829 的移植）。
// body 必须是 OpenAI chat 形态（协议翻译都在改写点上游完成）。返回改写后的
// body 与逆映射（别名→首个原名）；无 tools / 零命中时 body 原样返回、changed=false。
func (t *Table) Forward(body []byte) (out []byte, reverse map[string]string, changed bool) {
	fwd := t.forwardMap()
	if fwd == nil || len(body) == 0 {
		return body, nil, false
	}
	var tree any
	if err := json.Unmarshal(body, &tree); err != nil {
		return body, nil, false
	}
	root, ok := tree.(map[string]any)
	if !ok {
		return body, nil, false
	}
	reverse, changed = forwardValue(root, fwd)
	if !changed {
		return body, nil, false
	}
	out, err := json.Marshal(tree)
	if err != nil {
		return body, nil, false
	}
	return out, reverse, true
}

// forwardValue 是改写核心：tools[].function.name、tool_choice（字符串形态）、
// messages[].tool_calls[].function_call|function.name 三处（含碰撞防御与
// 同别名先到先得，逐条对齐 Trae）。
func forwardValue(root map[string]any, fwd map[string]string) (map[string]string, bool) {
	// 碰撞防御：改名前采集请求内全部真实工具名；别名命中存活真实名（且未被
	// 更早规则让出）时该规则整条跳过
	real := map[string]bool{}
	if tools, ok := root["tools"].([]any); ok {
		for _, tool := range tools {
			if tm, ok := tool.(map[string]any); ok {
				if fn, ok := tm["function"].(map[string]any); ok {
					if name, ok := fn["name"].(string); ok {
						real[name] = true
					}
				}
			}
		}
	}
	freed := map[string]bool{}
	effective := map[string]string{}
	for from, to := range fwd {
		if real[to] && !freed[to] {
			continue // 别名与存活真实名碰撞：整条跳过
		}
		freed[from] = true
		effective[from] = to
	}
	if len(effective) == 0 {
		return nil, false
	}
	reverse := map[string]string{}
	renamed := false
	// tools[].function/name
	if tools, ok := root["tools"].([]any); ok {
		for _, tool := range tools {
			tm, ok := tool.(map[string]any)
			if !ok {
				continue
			}
			fn, ok := tm["function"].(map[string]any)
			if !ok {
				continue
			}
			name, ok := fn["name"].(string)
			if !ok {
				continue
			}
			alias, hit := effective[name]
			if !hit {
				continue
			}
			if prev, dup := reverse[alias]; dup && prev != name {
				continue // 同别名冲突先到先得
			}
			fn["name"] = alias
			reverse[alias] = name
			renamed = true
		}
	}
	// tool_choice（字符串形态）：仅跟随 tools 实际发生的改写
	if tc, ok := root["tool_choice"].(string); ok {
		if _, isRenamed := func() (string, bool) {
			for alias, orig := range reverse {
				if orig == tc {
					return alias, true
				}
			}
			return "", false
		}(); isRenamed {
			root["tool_choice"] = effective[tc]
			renamed = true
		}
	}
	// messages[].tool_calls[].function_call|function.name（双形态历史轮）
	if msgs, ok := root["messages"].([]any); ok {
		for _, msg := range msgs {
			mm, ok := msg.(map[string]any)
			if !ok {
				continue
			}
			tcs, ok := mm["tool_calls"].([]any)
			if !ok {
				continue
			}
			for _, tc := range tcs {
				tcm, ok := tc.(map[string]any)
				if !ok {
					continue
				}
				for _, key := range []string{"function_call", "function"} {
					fn, ok := tcm[key].(map[string]any)
					if !ok {
						continue // 该键不存在 → 试下一个形态
					}
					name, ok := fn["name"].(string)
					if !ok {
						continue
					}
					if alias, hit := effective[name]; hit {
						if prev, dup := reverse[alias]; !dup || prev == name {
							fn["name"] = alias
							reverse[alias] = name
							renamed = true
						}
					}
					break // 命中一个形态即停（同一 tool_call 仅存其一）
				}
			}
		}
	}
	if !renamed {
		return nil, false
	}
	return reverse, true
}

// ReverseOpenAI 把 OpenAI 形态的响应树里的别名换回原名（就地修改）：
// 非流式 chatReply 的 choices[].message.tool_calls[].function.name，以及
// SSE chunk 的 choices[].delta.tool_calls[].function.name。返回是否有改动。
func ReverseOpenAI(tree map[string]any, reverse map[string]string) bool {
	if len(reverse) == 0 || tree == nil {
		return false
	}
	changed := false
	choices, ok := tree["choices"].([]any)
	if !ok {
		return false
	}
	for _, c := range choices {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		for _, msgKey := range []string{"message", "delta"} {
			msg, ok := cm[msgKey].(map[string]any)
			if !ok {
				continue
			}
			tcs, ok := msg["tool_calls"].([]any)
			if !ok {
				continue
			}
			for _, tc := range tcs {
				tcm, ok := tc.(map[string]any)
				if !ok {
					continue
				}
				if fn, ok := tcm["function"].(map[string]any); ok {
					if name, ok := fn["name"].(string); ok {
						if orig, hit := reverse[name]; hit {
							fn["name"] = orig
							changed = true
						}
					}
				}
			}
		}
	}
	return changed
}

// ReverseAnthropic 把 Anthropic 形态的响应树里的别名换回原名（就地修改）：
// 流式 content_block_start 的 content_block.tool_use.name，以及非流式整包的
// content[].tool_use.name。返回是否有改动。
func ReverseAnthropic(tree map[string]any, reverse map[string]string) bool {
	if len(reverse) == 0 || tree == nil {
		return false
	}
	changed := false
	// 流式事件：content_block_start / content_block（非流式块数组共用 name 字段路径）
	var blocks []any
	if cb, ok := tree["content_block"]; ok {
		blocks = []any{cb}
	} else if content, ok := tree["content"].([]any); ok {
		blocks = content
	}
	for _, b := range blocks {
		bm, ok := b.(map[string]any)
		if !ok {
			continue
		}
		if typ, _ := bm["type"].(string); typ != "tool_use" {
			continue
		}
		if name, ok := bm["name"].(string); ok {
			if orig, hit := reverse[name]; hit {
				bm["name"] = orig
				changed = true
			}
		}
	}
	return changed
}
