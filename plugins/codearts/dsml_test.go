package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDsmlSystemPromptLiteral(t *testing.T) {
	tools := []map[string]any{{
		"type": "function",
		"function": map[string]any{
			"name":        "get_weather",
			"description": "weather",
			"parameters":  map[string]any{"type": "string"},
		},
	}}
	got := buildDsmlSystemPrompt(tools)
	wantLines := []string{
		"以下是你可用的工具及其 JSON Schema。当需要调用工具完成任务时，",
		"必须使用原生 DSML 工具调用语法输出，格式如下：",
		"<｜DSML｜tool_calls><｜DSML｜invoke name=\"工具名\"><｜DSML｜parameter name=\"参数名\" string=\"true\">参数值</｜DSML｜parameter></｜DSML｜invoke></｜DSML｜tool_calls>",
		"",
		"规则：",
		"- 工具名必须是下面列表中的 name。",
		"- 每个参数用一个 <｜DSML｜parameter> 标签包裹，参数值放在标签之间。",
		"- 字符串参数加 string=\"true\" 属性；对象/数组/数字/布尔参数不要加该属性。",
		"- 一次可以输出多个 <｜DSML｜invoke> 调用（工具可以并行）。",
		"- 文件内容请一次性完整写入单个 write 调用的 content 参数，不要拆分或省略。",
		"",
		"工具列表（JSON Schema）：",
		"",
	}
	parts := strings.Split(got, "\n")
	if len(parts) < len(wantLines) {
		t.Fatalf("prompt has %d lines: %q", len(parts), got)
	}
	for i, want := range wantLines {
		if i == len(wantLines)-1 {
			break
		}
		if parts[i] != want {
			t.Fatalf("prompt line %d = %q, want %q", i, parts[i], want)
		}
	}
	if !strings.Contains(got, `"name": "get_weather"`) || !strings.Contains(got, `"parameters": {`) {
		t.Fatalf("tool schema missing: %s", got)
	}
}

func TestDsmlScalarParsing(t *testing.T) {
	block := "<｜DSML｜tool_calls>" +
		"<｜DSML｜invoke name=\"write\">" +
		"<｜DSML｜parameter name=\"count\" string=\"false\">1</｜DSML｜parameter>" +
		"<｜DSML｜parameter name=\"padded\" string=\"true\">001</｜DSML｜parameter>" +
		"<｜DSML｜parameter name=\"flag\" string=\"true\">true</｜DSML｜parameter>" +
		"<｜DSML｜parameter name=\"text\" string=\"true\">hello</｜DSML｜parameter>" +
		"<｜DSML｜parameter name=\"obj\" string=\"false\">{\"a\":1}</｜DSML｜parameter>" +
		"</｜DSML｜invoke></｜DSML｜tool_calls>"
	calls, ok := parseDsmlToolCalls(block)
	if !ok || len(calls) != 1 || calls[0].Name != "write" {
		t.Fatalf("calls = %+v ok=%v", calls, ok)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
		t.Fatalf("arguments are not valid JSON: %q (%v)", calls[0].Arguments, err)
	}
	if args["count"] != float64(1) || args["padded"] != float64(1) {
		t.Fatalf("numeric scalars = %v", args)
	}
	if args["flag"] != true || args["text"] != "hello" {
		t.Fatalf("scalars = %v", args)
	}
	obj, ok := args["obj"].(map[string]any)
	if !ok || obj["a"] != float64(1) {
		t.Fatalf("object parameter = %v", args["obj"])
	}
}

func TestDsmlExtractorHandlesSplitTags(t *testing.T) {
	input := "<thought>re</thought>a<｜DSML｜tool_calls><｜DSML｜invoke name=\"x\">" +
		"<｜DSML｜parameter name=\"v\" string=\"true\">1</｜DSML｜parameter>" +
		"</｜DSML｜invoke></｜DSML｜tool_calls>b"
	var text, reasoning strings.Builder
	var calls []dsmlCall
	ex := &dsmlExtractor{}
	for _, r := range []rune(input) {
		tx, re, cs := ex.feed(string(r))
		text.WriteString(tx)
		reasoning.WriteString(re)
		calls = append(calls, cs...)
	}
	tx, re := ex.flush()
	text.WriteString(tx)
	reasoning.WriteString(re)
	if text.String() != "ab" || reasoning.String() != "re" {
		t.Fatalf("text=%q reasoning=%q", text.String(), reasoning.String())
	}
	if len(calls) != 1 || calls[0].Name != "x" || calls[0].Arguments != `{"v":1}` {
		t.Fatalf("calls = %+v", calls)
	}
}
