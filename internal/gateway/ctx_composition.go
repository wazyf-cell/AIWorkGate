// 上下文 Token 六维 Treemap · 一期采集（task-62；设计 docs/plans/2026-11-08-六维treemap设计要点.md）。
//
// 六桶精确归类（一期）：system / tools / tool_results / conversation / memory / other。
// 复用 prompt_compose.go 的估算口径（estimateTextTokens / estimateMessageContent），
// 但独立于 D-55 的 AnalyzePrompt 六段契约（Scheduler 粘贴分析在用，桶语义不同——
// 那里没有 tool_results 独立桶，且 user_input/history 是轮次切分而非内容分类）。
//
// 文件/记忆两维一期只做精确桶（顶层 memory/memories 字段）；启发式识别默认关（二期）。
//
// 门控：sticky 采样（trace.go:5-7 同模式）——SetCtxSampling 开启后才在请求热路径
// 分析 body；关闭时只付一次原子读。口径：在 redact/tool-alias 改写**前**的 body 上
// 算（用户真实上下文；挂点在 dispatch.go / forwardPlugin 的两个改写点之前，只读
// 不改写，与两个改写钩子无顺序耦合）。
//
// 对账：本地估算与上游 usage.prompt_tokens 系统性偏差按比例分摊
// （allocated = upstream × est/total，余数给最大桶），不引 tokenizer（D-55 决策）。
package gateway

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
)

// ctxSections 是六桶的固定输出序。
var ctxSections = []string{"system", "tools", "tool_results", "conversation", "memory", "other"}

// CtxSection is one bucket of the composition: the local estimate and, when the
// upstream reported a prompt total, the proportionally allocated share.
type CtxSection struct {
	Key       string  `json:"key"`
	Est       int     `json:"est"`
	Share     float64 `json:"share"`
	Allocated *int    `json:"allocated,omitempty"`
}

// CtxItem is one per-message / per-tool row for the treemap drill-down.
type CtxItem struct {
	Bucket string `json:"bucket"`
	Label  string `json:"label"`
	Tokens int    `json:"tokens"`
}

// CtxComposition is the sampled breakdown of one request body.
type CtxComposition struct {
	TotalEst int          `json:"total_est"`
	Sections []CtxSection `json:"sections"`
	Items    []CtxItem    `json:"items,omitempty"`
}

// CtxEmit is what the gateway hands the host sink at request close: the
// composition plus the upstream prompt total for the proportional allocation.
type CtxEmit struct {
	ReqID          string
	TS             int64
	Comp           CtxComposition
	UpstreamPrompt int64
	UsageKnown     bool
	Reason         string
}

// ctxItemLabelCap caps the drill-down label prefix per item.
const ctxItemLabelCap = 40

// AnalyzeCtxComposition splits a request body into the six exact buckets with
// per-message / per-tool drill-down rows. Unknown top-level fields land in
// other, so the buckets always cover the whole body. The heuristic buckets
// (files / memory block-level, task-67) stay off here — this is the phase-1
// contract; the sampling gate uses AnalyzeCtxCompositionHeuristics.
func AnalyzeCtxComposition(body []byte) (CtxComposition, error) {
	return analyzeCtxComposition(body, false)
}

// AnalyzeCtxCompositionHeuristics is the phase-2 variant: path-shaped
// substrings inside conversation text land in a separate "files" bucket and
// <memory>/CLAUDE.md-marked blocks land in "memory". Best-effort by design.
func AnalyzeCtxCompositionHeuristics(body []byte) (CtxComposition, error) {
	return analyzeCtxComposition(body, true)
}

// analyzeCtxComposition is the shared core; heuristics switches the bucket set
// (six exact buckets vs seven with "files") and the per-block splitting.
func analyzeCtxComposition(body []byte, heuristics bool) (CtxComposition, error) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return CtxComposition{}, err
	}
	buckets := map[string]float64{}
	var items []CtxItem
	consumed := map[string]bool{}

	// 1. system 段（三种 envelope 的拼写，与 AnalyzePrompt 同清单）。
	for _, key := range []string{"system", "systemInstruction", "system_instruction", "instructions"} {
		if value, ok := obj[key]; ok {
			buckets["system"] += estimateMessageContent(value)
			consumed[key] = true
		}
	}
	// 2. 工具定义：每工具一条 drill-down 行。
	for _, key := range []string{"tools", "functions"} {
		if value, ok := obj[key]; ok {
			tokens, rows := estimateToolsBucket(value)
			buckets["tools"] += tokens
			items = append(items, rows...)
			consumed[key] = true
		}
	}
	// 3. memory：一期精确桶（顶层字段）；二期启发式的块级判定在消息遍历里叠加。
	for _, key := range []string{"memory", "memories"} {
		if value, ok := obj[key]; ok {
			buckets["memory"] += estimateJSONTokens(value)
			consumed[key] = true
		}
	}
	// 4. 消息面（messages / contents / input / prompt）。
	for _, key := range []string{"messages", "contents", "input"} {
		if value, ok := obj[key]; ok {
			addCtxMessages(buckets, &items, value, heuristics)
			consumed[key] = true
		}
	}
	if value, ok := obj["prompt"]; ok {
		tokens := estimateMessageContent(value)
		buckets["conversation"] += tokens
		items = append(items, CtxItem{Bucket: "conversation", Label: ctxLabel("prompt", value), Tokens: int(math.Ceil(tokens))})
		consumed["prompt"] = true
	}

	rest := map[string]any{}
	for key, value := range obj {
		if !consumed[key] {
			rest[key] = value
		}
	}
	if len(rest) > 0 {
		buckets["other"] += estimateJSONTokens(rest)
	}

	return buildCtxComposition(buckets, items, heuristics), nil
}

// addCtxMessages routes every message (or Responses input item) to its bucket
// and appends one drill-down row per message (split rows when a single message
// carries both text and tool_result blocks). heuristics enables the phase-2
// files/memory block-level splitting.
func addCtxMessages(buckets map[string]float64, items *[]CtxItem, raw any, heuristics bool) {
	if text, ok := raw.(string); ok {
		addCtxText(buckets, items, text, heuristics)
		return
	}
	list, ok := raw.([]any)
	if !ok {
		tokens := estimateJSONTokens(raw)
		buckets["conversation"] += tokens
		*items = append(*items, CtxItem{Bucket: "conversation", Label: ctxLabel("input", raw), Tokens: int(math.Ceil(tokens))})
		return
	}
	for i, rawMessage := range list {
		message, _ := rawMessage.(map[string]any)
		addCtxMessage(buckets, items, i, message, heuristics)
	}
}

// addCtxText classifies one plain-text turn: with heuristics on, memory-marker
// blocks go to the memory bucket and path-shaped substrings to files; the rest
// stays in conversation. Without heuristics this is the verbatim phase-1 path.
func addCtxText(buckets map[string]float64, items *[]CtxItem, text string, heuristics bool) {
	tokens := estimateTextTokens(text)
	if !heuristics {
		buckets["conversation"] += tokens
		*items = append(*items, CtxItem{Bucket: "conversation", Label: ctxLabel("text", text), Tokens: int(math.Ceil(tokens))})
		return
	}
	if ctxMemoryBlockHit(text) {
		buckets["memory"] += tokens
		*items = append(*items, CtxItem{Bucket: "memory", Label: ctxLabel("memory-block", text), Tokens: int(math.Ceil(tokens))})
		return
	}
	fileTokens, bestPath := ctxFileTokens(text)
	conversation := tokens - fileTokens
	if conversation > 0 {
		buckets["conversation"] += conversation
		*items = append(*items, CtxItem{Bucket: "conversation", Label: ctxLabel("text", text), Tokens: int(math.Ceil(conversation))})
	}
	if fileTokens > 0 {
		buckets["files"] += fileTokens
		*items = append(*items, CtxItem{Bucket: "files", Label: ctxLabel("file", bestPath), Tokens: int(math.Ceil(fileTokens))})
	}
}

// addCtxMessage classifies one message. Exact buckets: role=system/developer →
// system; role=tool (OpenAI) and type=function_call_output (Responses) →
// tool_results; tool_result blocks inside an Anthropic message → tool_results
// with the remaining blocks staying conversation; everything else →
// conversation. tool_calls parameters are part of the assistant turn, so they
// stay in conversation.
func addCtxMessage(buckets map[string]float64, items *[]CtxItem, index int, message map[string]any, heuristics bool) {
	role := strings.ToLower(jsonStringValue(message["role"]))
	// Responses input items carry their own type spelling.
	itemType := strings.ToLower(jsonStringValue(message["type"]))

	switch {
	case role == "system" || role == "developer":
		tokens := 4 + estimateMessageTokens(message)
		buckets["system"] += tokens
		*items = append(*items, CtxItem{Bucket: "system", Label: ctxLabel(role+"#"+itoa(index), message), Tokens: int(math.Ceil(tokens))})
		return
	case role == "tool" || itemType == "function_call_output":
		tokens := 4 + estimateMessageTokens(message)
		buckets["tool_results"] += tokens
		*items = append(*items, CtxItem{Bucket: "tool_results", Label: ctxLabel("tool_result#"+itoa(index), message), Tokens: int(math.Ceil(tokens))})
		return
	}

	// 普通消息：content 块级拆分（Anthropic 的 text + tool_result 混排——
	// tool_result 块进 tool_results 桶，其余块与 tool_calls 参数进 conversation）。
	// heuristics 开时文本部分再拆 files/memory（ctxSplitTextHeuristics）。
	textTokens, resultTokens := splitMessageBlocks(message)
	calls := toolCallTokens(message)
	if resultTokens > 0 {
		buckets["tool_results"] += resultTokens
		*items = append(*items, CtxItem{Bucket: "tool_results", Label: ctxLabel("tool_result#"+itoa(index), message), Tokens: int(math.Ceil(resultTokens))})
	}
	if !heuristics {
		if conversation := textTokens + calls; conversation > 0 {
			buckets["conversation"] += conversation
			*items = append(*items, CtxItem{Bucket: "conversation", Label: ctxLabel(role+"#"+itoa(index), message), Tokens: int(math.Ceil(conversation))})
		}
		return
	}
	// 二期启发式：从消息文本里切出文件路径串与记忆标记块；无标记部分留在
	// conversation，三桶之和恒等于一期 conversation+memory 的口径（无 token 凭空增减）。
	fileTokens, memoryTokens, bestPath := ctxSplitTextHeuristics(message)
	if memoryTokens > 0 {
		buckets["memory"] += memoryTokens
		*items = append(*items, CtxItem{Bucket: "memory", Label: ctxLabel("memory-block#"+itoa(index), message), Tokens: int(math.Ceil(memoryTokens))})
	}
	if fileTokens > 0 {
		buckets["files"] += fileTokens
		*items = append(*items, CtxItem{Bucket: "files", Label: ctxLabel("file#"+itoa(index), bestPath), Tokens: int(math.Ceil(fileTokens))})
	}
	if conversation := textTokens - fileTokens - memoryTokens + calls; conversation > 0 {
		buckets["conversation"] += conversation
		*items = append(*items, CtxItem{Bucket: "conversation", Label: ctxLabel(role+"#"+itoa(index), message), Tokens: int(math.Ceil(conversation))})
	}
}

// splitMessageBlocks walks one message's content/parts blocks: tool_result
// blocks return in the second value, every other spelling (text, image, tool
// use, plain string) in the first.
func splitMessageBlocks(message map[string]any) (textTokens, resultTokens float64) {
	for _, key := range []string{"content", "parts"} {
		value, ok := message[key]
		if !ok {
			continue
		}
		switch blocks := value.(type) {
		case string:
			textTokens += estimateTextTokens(blocks)
		case []any:
			for _, rawBlock := range blocks {
				block, _ := rawBlock.(map[string]any)
				if block == nil {
					if text, ok := rawBlock.(string); ok {
						textTokens += estimateTextTokens(text)
					}
					continue
				}
				if strings.ToLower(jsonStringValue(block["type"])) == "tool_result" {
					resultTokens += estimateMessageContent(block["content"])
					continue
				}
				textTokens += estimateBlockTokens(block)
			}
		case map[string]any:
			if strings.ToLower(jsonStringValue(blocks["type"])) == "tool_result" {
				resultTokens += estimateMessageContent(blocks["content"])
			} else {
				textTokens += estimateBlockTokens(blocks)
			}
		}
	}
	// Responses input items: output carries function_call_output text.
	if out, ok := message["output"]; ok {
		resultTokens += estimateMessageContent(out)
	}
	return textTokens, resultTokens
}

// estimateBlockTokens prices one non-tool_result block (estimateMessageContent
// handles the block shapes; this wrapper keeps intent explicit at the call site).
func estimateBlockTokens(block map[string]any) float64 {
	return estimateMessageContent(block)
}

// toolCallTokens prices the tool_calls / function_call payloads of one message.
func toolCallTokens(message map[string]any) float64 {
	total := 0.0
	if calls, ok := message["tool_calls"].([]any); ok {
		for _, rawCall := range calls {
			call, _ := rawCall.(map[string]any)
			fn, _ := call["function"].(map[string]any)
			total += estimateTextTokens(jsonStringValue(fn["name"]))
			total += estimateTextTokens(jsonStringValue(fn["arguments"]))
		}
	}
	if fn, ok := message["function_call"].(map[string]any); ok {
		total += estimateTextTokens(jsonStringValue(fn["name"]))
		total += estimateTextTokens(jsonStringValue(fn["arguments"]))
	}
	return total
}

// estimateToolsBucket prices the tools array and emits one row per tool.
func estimateToolsBucket(value any) (float64, []CtxItem) {
	total := 0.0
	var items []CtxItem
	appendTool := func(name string, tokens float64) {
		total += tokens
		label := strings.TrimSpace(name)
		if label == "" {
			label = "tool"
		}
		items = append(items, CtxItem{Bucket: "tools", Label: "tool:" + label, Tokens: int(math.Ceil(tokens))})
	}
	list, ok := value.([]any)
	if !ok {
		appendTool("", estimateJSONTokens(value))
		return total, items
	}
	for _, rawTool := range list {
		tool, _ := rawTool.(map[string]any)
		if tool == nil {
			appendTool("", estimateJSONTokens(rawTool))
			continue
		}
		name := jsonStringValue(tool["name"])
		if fn, ok := tool["function"].(map[string]any); ok {
			name = jsonStringValue(fn["name"])
		}
		appendTool(name, estimateJSONTokens(tool))
	}
	return total, items
}

// buildCtxComposition materializes the fixed bucket output and shares. With
// heuristics on, the "files" bucket joins the fixed order (between tool_results
// and conversation); off, the phase-1 six-bucket order is kept verbatim.
func buildCtxComposition(buckets map[string]float64, items []CtxItem, heuristics bool) CtxComposition {
	order := ctxSections
	if heuristics {
		order = ctxSectionsHeuristics
	}
	comp := CtxComposition{Sections: make([]CtxSection, 0, len(order))}
	for _, key := range order {
		tokens := int(math.Ceil(buckets[key]))
		comp.TotalEst += tokens
		comp.Sections = append(comp.Sections, CtxSection{Key: key, Est: tokens})
	}
	if comp.TotalEst > 0 {
		for i := range comp.Sections {
			comp.Sections[i].Share = math.Round(float64(comp.Sections[i].Est)/float64(comp.TotalEst)*10000) / 10000
		}
	}
	sort.SliceStable(items, func(a, b int) bool { return items[a].Tokens > items[b].Tokens })
	comp.Items = items
	return comp
}

// AllocatePromptTokens rescales the local estimates onto the upstream-reported
// prompt total (proportional allocation, remainder to the largest bucket).
// Returns the sections with Allocated set; upstream <= 0 leaves them nil.
func AllocatePromptTokens(comp CtxComposition, upstream int64) []CtxSection {
	out := make([]CtxSection, len(comp.Sections))
	copy(out, comp.Sections)
	if upstream <= 0 || comp.TotalEst <= 0 {
		return out
	}
	allocated := 0
	largest := 0
	for i := range out {
		value := int(math.Round(float64(upstream) * float64(out[i].Est) / float64(comp.TotalEst)))
		out[i].Allocated = &value
		allocated += value
		if out[i].Est > out[largest].Est {
			largest = i
		}
	}
	// The rounding remainder lands on the largest bucket so the allocated sum
	// reproduces the upstream prompt total exactly.
	if remainder := int(upstream) - allocated; remainder != 0 {
		fixed := *out[largest].Allocated + remainder
		out[largest].Allocated = &fixed
	}
	return out
}

// ctxLabel builds a drill-down label: a role/prefix plus the first runes of the
// human-readable text (content spellings first, then the JSON itself).
func ctxLabel(prefix string, value any) string {
	var text string
	switch v := value.(type) {
	case string:
		text = v
	case map[string]any:
		text = jsonStringValue(v["text"])
		if text == "" {
			text = jsonStringValue(v["name"])
		}
	}
	text = strings.TrimSpace(text)
	if text == "" {
		encoded, err := json.Marshal(value)
		if err != nil {
			return prefix
		}
		text = string(encoded)
	}
	runes := []rune(text)
	if len(runes) > ctxItemLabelCap {
		text = string(runes[:ctxItemLabelCap]) + "…"
	}
	return prefix + ": " + text
}

// itoa is a tiny int formatter avoiding strconv imports at every call site.
func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	negative := v < 0
	if negative {
		v = -v
	}
	var digits [20]byte
	i := len(digits)
	for v > 0 {
		i--
		digits[i] = byte('0' + v%10)
		v /= 10
	}
	if negative {
		i--
		digits[i] = '-'
	}
	return string(digits[i:])
}
