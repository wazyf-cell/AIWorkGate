package gateway

// D-51 (plans/2026-11-05 §D-51): POST /v1/messages/count_tokens.
//
// 默认口径（定稿并写进实现）：**本地估算**。Anthropic 客户端（SDK、CLI）在发
// 送前会用这个端点估算上下文体积；网关侧没有上游真实 tokenizer，而插件契约
// （docs/插件开发指南.md）目前没有 count_tokens 的能力声明机制——插件既不能
// 声明"我能算真实 token"，网关也没有按能力分派的路径。因此选择本地估算作为
// 唯一默认口径：
//   - 复用 estimatePromptTokens（gateway.go，P11 规则输入的同一套字符加权估
//     算：ASCII 0.3 / 非 ASCII 1），对 messages + system + tools 求和；
//   - 该端点是"读"不是推理：不进 isInferencePath（不消耗客户端 Key 的每日配
//     额）、不路由到任何上游、不计费、不写 source 头；
//   - 估算值与真实值可以偏差较大（尤其缓存命中与图片块），Anthropic 官方语义
//     里它本来就是"预算参考值"；客户端拿它做截断决策足够。
//
// 插件真实值留给以后：若将来插件清单增加 count_tokens 能力声明，这里在同一
// 个 handler 里按 model → plugin 能力表短路成一次上游调用即可，路由与鉴权
// 保持不变（取舍记录于 2026-11-06 D-51 定稿）。

import (
	"encoding/json"
	"net/http"
)

// handleCountTokens implements POST /v1/messages/count_tokens with the local
// char-weighted estimate. Authentication, CORS, rate limits and logging are
// shared with every other /v1 endpoint (ServeHTTP); the endpoint itself never
// schedules, never forwards and never bills.
func (g *Gateway) handleCountTokens(w http.ResponseWriter, r *http.Request, rec *reqRecord) {
	body, failStatus := readAnthropicBody(w, r)
	if failStatus != 0 {
		rec.status = failStatus
		return
	}
	meta := parseRequestMeta(body)
	if !meta.valid {
		// Same semantics as /v1/messages' invalid_json: a malformed body is a
		// 400 with the Anthropic error shape, never a fabricated 200 of 0.
		rec.errText = "invalid JSON body"
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "request body must be a JSON object")
		rec.status = http.StatusBadRequest
		return
	}
	// prepareRecord fills model/promptChars/estTokens for the api_logs row, so
	// the estimate is auditable from the log even though nothing is billed.
	g.prepareRecord(rec, r, body, meta)

	tokens := meta.estTokens
	if tokens < 0 {
		tokens = 0
	}
	rec.status = http.StatusOK
	data, err := json.Marshal(map[string]int{"input_tokens": tokens})
	if err != nil {
		g.fail(w, rec, http.StatusInternalServerError, "api_error", "encode_failed",
			"failed to encode count_tokens response")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
