package main

import (
	"bytes"
	"encoding/json"
	"strings"
)

// buildDsmlSystemPrompt is the literal template from the reference
// implementation (buildDsmlSystemPrompt).
func buildDsmlSystemPrompt(tools []map[string]any) string {
	type toolShape struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	}
	list := make([]toolShape, 0, len(tools))
	for _, t := range tools {
		fn, _ := t["function"].(map[string]any)
		tool := toolShape{}
		tool.Name, _ = fn["name"].(string)
		tool.Description, _ = fn["description"].(string)
		if p, ok := fn["parameters"]; ok {
			raw, _ := json.Marshal(p)
			tool.Parameters = raw
		}
		list = append(list, tool)
	}
	// Mirror JSON.stringify(..., null, 2): no HTML escaping (Go would
	// Unicode-escape "<", changing what the model sees in tool schemas).
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(list)
	b := bytes.TrimRight(buf.Bytes(), "\n")
	return strings.Join([]string{
		"以下是你可用的工具及其 JSON Schema。当需要调用工具完成任务时，",
		"必须使用原生 DSML 工具调用语法输出，格式如下：",
		dsmlCallsOpen + dsmlInvokeOpen + ` name="工具名">` + dsmlParamOpen + ` name="参数名" string="true">参数值` + dsmlParamEnd + dsmlInvokeEnd + dsmlCallsClose,
		"",
		"规则：",
		"- 工具名必须是下面列表中的 name。",
		"- 每个参数用一个 " + dsmlParamOpen + "> 标签包裹，参数值放在标签之间。",
		`- 字符串参数加 string="true" 属性；对象/数组/数字/布尔参数不要加该属性。`,
		"- 一次可以输出多个 " + dsmlInvokeOpen + "> 调用（工具可以并行）。",
		"- 文件内容请一次性完整写入单个 write 调用的 content 参数，不要拆分或省略。",
		"",
		"工具列表（JSON Schema）：",
		string(b),
	}, "\n")
}
