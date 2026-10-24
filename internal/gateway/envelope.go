package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// This file lets plugin-backed models answer two other OpenAI wire formats:
// /v1/completions (legacy text completions) and /v1/responses (Responses API).
// Each request is translated into chat/completions and dispatched through the
// normal scheduling path; the upstream reply is translated back, including
// Server-Sent Events, because agent clients such as Codex only speak the
// Responses API and they stream.

type envelopeKind int

const (
	envelopeCompletions envelopeKind = iota
	envelopeResponses
)

// maxEnvelopeBuffer bounds how much upstream JSON is held while a non-streaming
// reply is converted.
const maxEnvelopeBuffer = 32 << 20

type envelopeError struct{ msg string }

func (e *envelopeError) Error() string { return e.msg }

// ---------------------------------------------------------------- wire types

type wireUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// PromptTokensDetails carries the cached prefix (OpenAI dialect).
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	// CompletionTokensDetails carries the reasoning token count.
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
	// Anthropic reports the same prefix counters under these names.
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

func (u *wireUsage) total() int {
	if u.TotalTokens > 0 {
		return u.TotalTokens
	}
	return u.PromptTokens + u.CompletionTokens
}

// cachedTokens returns the cached prompt prefix in either dialect. Both fields
// describe the same tokens; when a proxy emits both the larger value wins
// (union semantics) instead of summing, which would double count. Same rule as
// bodyTap.cachedPrompt. Ported from A (TraeWorkAssistant, MIT, 朱天伟)
// sse.rs:826-829,922-927.
func (u *wireUsage) cachedTokens() int {
	if u == nil {
		return 0
	}
	cached := 0
	if u.PromptTokensDetails != nil {
		cached = u.PromptTokensDetails.CachedTokens
	}
	if u.CacheReadInputTokens > cached {
		cached = u.CacheReadInputTokens
	}
	return cached
}

// cacheCreationTokens returns the cache-write count when the upstream reports
// one; chat upstreams generally do not, so this is usually 0.
func (u *wireUsage) cacheCreationTokens() int {
	if u == nil {
		return 0
	}
	return u.CacheCreationInputTokens
}

// reasoningTokens reports the reasoning token count and whether the upstream
// sent completion_tokens_details at all.
func (u *wireUsage) reasoningTokens() (int, bool) {
	if u == nil || u.CompletionTokensDetails == nil {
		return 0, false
	}
	return u.CompletionTokensDetails.ReasoningTokens, true
}

func (u *wireUsage) completionValue() map[string]any {
	if u == nil {
		return nil
	}
	return map[string]any{
		"prompt_tokens": u.PromptTokens, "completion_tokens": u.CompletionTokens, "total_tokens": u.total(),
	}
}

// responsesValue keeps the flat input/output/total fields and adds the detail
// objects when the upstream reported them. Ported from A (TraeWorkAssistant,
// MIT, 朱天伟) wb_responses.rs:519 (input_tokens_details.cached_tokens).
func (u *wireUsage) responsesValue() map[string]any {
	if u == nil {
		return nil
	}
	value := map[string]any{
		"input_tokens": u.PromptTokens, "output_tokens": u.CompletionTokens, "total_tokens": u.total(),
	}
	if u.PromptTokensDetails != nil {
		value["input_tokens_details"] = map[string]any{
			"cached_tokens": u.PromptTokensDetails.CachedTokens,
		}
	}
	if reasoning, ok := u.reasoningTokens(); ok {
		value["output_tokens_details"] = map[string]any{"reasoning_tokens": reasoning}
	}
	return value
}

type wireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatReply struct {
	ID      string `json:"id"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int         `json:"index"`
		FinishReason any         `json:"finish_reason"`
		Message      chatMessage `json:"message"`
	} `json:"choices"`
	Usage *wireUsage      `json:"usage"`
	Error json.RawMessage `json:"error"`
}

type chatMessage struct {
	Role             string         `json:"role"`
	Content          string         `json:"content"`
	ReasoningContent string         `json:"reasoning_content"`
	ToolCalls        []wireToolCall `json:"tool_calls"`
}

type chatChunk struct {
	ID      string `json:"id"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Role             string      `json:"role"`
			Content          string      `json:"content"`
			ReasoningContent string      `json:"reasoning_content"`
			ToolCalls        []toolDelta `json:"tool_calls"`
		} `json:"delta"`
		FinishReason json.RawMessage `json:"finish_reason"`
	} `json:"choices"`
	Usage *wireUsage `json:"usage"`
	// Error is set by upstreams that report a failure mid-stream, e.g.
	// data: {"error":{"message":...,"type":...}}. Ported from A
	// (TraeWorkAssistant, MIT, 朱天伟) sse.rs:1055-1077, wb_sse.rs:87.
	Error *wireError `json:"error"`
}

// wireError is the error object carried by a mid-stream failure frame.
type wireError struct {
	Message string          `json:"message"`
	Type    string          `json:"type"`
	Code    json.RawMessage `json:"code"`
}

func (e *wireError) codeString() string {
	if e == nil || len(e.Code) == 0 || string(e.Code) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(e.Code, &s) == nil {
		return s
	}
	return strings.Trim(string(e.Code), `"`)
}

// streamErrorFromFrame resolves the failure carried by an upstream frame, in
// either the {"error":{...}} shape or the bare payload of an `event: error`
// frame. Ported from A (TraeWorkAssistant, MIT, 朱天伟) sse.rs:1055-1077.
func streamErrorFromFrame(chunk chatChunk, payload string) (typ, code, msg string) {
	if chunk.Error != nil {
		code = chunk.Error.codeString()
		return chunk.Error.Type, code, firstNonEmpty(chunk.Error.Message, code)
	}
	var plain wireError
	if err := json.Unmarshal([]byte(payload), &plain); err == nil {
		code = plain.codeString()
		return plain.Type, code, firstNonEmpty(plain.Message, code)
	}
	return "", "", ""
}

type toolDelta struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ---------------------------------------------------------------- requests

// relayEnvelope translates a /v1/completions or /v1/responses request into a
// chat body and hands it to send, which must write the chat reply into ew. The
// conversion lives here once; the transport is the chatSender seam, so the
// plugin/scheduler pipeline (handleEnvelope) and custom upstreams
// (custom_models.go) share it. rec keeps the client-facing model name; the
// sender rewrites the body to the physical one, so scheduling, logs and headers
// stay comparable.
func (g *Gateway) relayEnvelope(ew http.ResponseWriter, rec *reqRecord, body []byte, kind envelopeKind, send chatSender) {
	var (
		chatBody []byte
		err      error
	)
	if kind == envelopeResponses {
		chatBody, err = chatBodyFromResponses(body)
	} else {
		chatBody, err = chatBodyFromCompletions(body)
	}
	if err != nil {
		g.fail(ew, rec, http.StatusBadRequest, "invalid_request_error", "unsupported_request", err.Error())
		return
	}
	send(ew, chatBody)
}

// chatBodyFromCompletions translates a text-completions request into chat.
func chatBodyFromCompletions(body []byte) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, &envelopeError{"request body must be a JSON object"}
	}
	model := jsonString(top["model"])
	if strings.TrimSpace(model) == "" {
		return nil, &envelopeError{"model is required"}
	}
	prompt, err := singlePromptText(top["prompt"])
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"suffix", "echo", "logit_bias", "best_of"} {
		if completionsParamUnsupported(name, top[name]) {
			return nil, &envelopeError{"completions 参数 " + name + " 无法在 chat 后端复现，请改用 /v1/chat/completions"}
		}
	}
	if n := jsonNumber(top["n"], 1); n != 1 {
		return nil, &envelopeError{"completions 参数 n 目前只支持 1"}
	}
	messages, err := json.Marshal([]map[string]any{{"role": "user", "content": prompt}})
	if err != nil {
		return nil, err
	}
	out := map[string]json.RawMessage{"model": mustJSON(model), "messages": messages}
	copyTop(out, top, "stream", "max_tokens", "temperature", "top_p", "presence_penalty",
		"frequency_penalty", "user", "seed", "stop")
	return json.Marshal(out)
}

// completionsParamUnsupported reports whether a legacy completions parameter asks
// for behaviour a chat backend cannot reproduce. Only a truthy value counts:
// SDKs send the OpenAI defaults (echo:false, suffix:"", best_of:1) on every call,
// so rejecting on mere presence turned a normal request into a 400.
func completionsParamUnsupported(name string, raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	switch name {
	case "suffix":
		var suffix string
		if json.Unmarshal(raw, &suffix) != nil {
			return true
		}
		return suffix != ""
	case "echo":
		var echo bool
		if json.Unmarshal(raw, &echo) != nil {
			return true
		}
		return echo
	case "logit_bias":
		var bias map[string]json.RawMessage
		if json.Unmarshal(raw, &bias) != nil {
			return true
		}
		return len(bias) > 0
	case "best_of":
		var best float64
		if json.Unmarshal(raw, &best) != nil {
			return true
		}
		// best_of is capped by n, which this gateway serves only as 1.
		return best > 1
	}
	return false
}

func singlePromptText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", &envelopeError{"prompt is required"}
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil {
		if len(arr) != 1 {
			return "", &envelopeError{"prompt 数组只支持单条提示；多条请改用 /v1/chat/completions"}
		}
		var one string
		if err := json.Unmarshal(arr[0], &one); err != nil {
			return "", &envelopeError{"prompt 元素必须是字符串"}
		}
		return one, nil
	}
	return "", &envelopeError{"prompt 必须是字符串或单元素字符串数组"}
}

// chatBodyFromResponses translates a Responses API request into chat.
func chatBodyFromResponses(body []byte) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, &envelopeError{"request body must be a JSON object"}
	}
	model := jsonString(top["model"])
	if strings.TrimSpace(model) == "" {
		return nil, &envelopeError{"model is required"}
	}
	if v, ok := top["background"]; ok && strings.TrimSpace(string(v)) == "true" {
		return nil, &envelopeError{"Responses 的 background 模式暂不支持"}
	}
	// previous_response_id is a server-side pointer. This gateway keeps no
	// conversation state, so it is accepted and ignored as long as the client
	// resends the full input (which Codex-style clients do).
	prevID := ""
	if v, ok := top["previous_response_id"]; ok && string(v) != "null" && string(v) != `""` {
		prevID = strings.TrimSpace(jsonString(v))
	}
	messages := make([]map[string]any, 0, 8)
	if instructions := strings.TrimSpace(jsonString(top["instructions"])); instructions != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
	}
	input, err := responsesInputToMessages(top["input"])
	if err != nil {
		return nil, err
	}
	messages = append(messages, input...)
	if len(messages) == 0 {
		if prevID != "" {
			return nil, &envelopeError{"previous_response_id 需要同时提供 input：本网关无状态，不保存上一轮上下文"}
		}
		return nil, &envelopeError{"input 为空，至少需要一条消息"}
	}
	b, err := json.Marshal(messages)
	if err != nil {
		return nil, err
	}
	out := map[string]json.RawMessage{"model": mustJSON(model), "messages": b}
	copyTop(out, top, "stream", "temperature", "top_p", "user", "presence_penalty",
		"frequency_penalty", "stop", "seed", "stream_options")
	if v, ok := top["max_output_tokens"]; ok {
		out["max_tokens"] = v
	}
	if effort, ok := reasoningEffort(top["reasoning"]); ok {
		out["reasoning_effort"] = mustJSON(effort)
	}
	if tools, ok := responsesToolsToChat(top["tools"]); ok {
		out["tools"] = tools
	}
	if v, ok := top["tool_choice"]; ok {
		out["tool_choice"] = v
	}
	return json.Marshal(out)
}

func reasoningEffort(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", false
	}
	var obj struct {
		Effort string `json:"effort"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "", false
	}
	return strings.TrimSpace(obj.Effort), strings.TrimSpace(obj.Effort) != ""
}

// responsesToolsToChat unwraps the flat Responses tool shape into chat's nested
// shape. Entries that are already nested, are not functions or carry no name are
// kept as they are, so a mixed list still loses only the flat entries' shape.
func responsesToolsToChat(raw json.RawMessage) (json.RawMessage, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return raw, true
	}
	out := make([]any, 0, len(tools))
	converted := false
	for _, tool := range tools {
		fn, ok := flatFunctionTool(tool)
		if !ok {
			out = append(out, tool)
			continue
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
		converted = true
	}
	if !converted {
		// Nothing to rewrite: hand the list to the plugin byte for byte.
		return raw, true
	}
	b, err := json.Marshal(out)
	if err != nil {
		return raw, true
	}
	return b, true
}

// flatFunctionTool reports whether one tool uses the flat Responses function
// shape and returns its chat-side payload.
func flatFunctionTool(tool map[string]json.RawMessage) (map[string]json.RawMessage, bool) {
	if jsonString(tool["type"]) != "function" {
		return nil, false
	}
	if _, nested := tool["function"]; nested {
		return nil, false
	}
	if strings.TrimSpace(jsonString(tool["name"])) == "" {
		return nil, false
	}
	fn := map[string]json.RawMessage{}
	for _, key := range []string{"name", "description", "parameters", "strict"} {
		if v, ok := tool[key]; ok {
			fn[key] = v
		}
	}
	return fn, true
}

// responsesInputToMessages accepts a bare string or the item list the SDK sends.
func responsesInputToMessages(raw json.RawMessage) ([]map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return []map[string]any{{"role": "user", "content": text}}, nil
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, &envelopeError{"input 必须是字符串或条目数组"}
	}
	out := make([]map[string]any, 0, len(items))
	// Consecutive function_call items are one assistant turn: chat pairs every
	// tool reply with the message carrying that call, so parallel calls have to
	// share a single tool_calls array instead of becoming separate messages.
	var callMsg map[string]any
	var callList []any
	for _, item := range items {
		if jsonString(item["type"]) == "function_call" {
			callID := firstNonEmpty(jsonString(item["call_id"]), jsonString(item["id"]), newWireID("call"))
			callList = append(callList, map[string]any{
				"id": callID, "type": "function",
				"function": map[string]any{
					"name":      jsonString(item["name"]),
					"arguments": stringOrRaw(item["arguments"]),
				},
			})
			if callMsg == nil {
				callMsg = map[string]any{"role": "assistant", "content": ""}
				out = append(out, callMsg)
			}
			callMsg["tool_calls"] = callList
			continue
		}
		callMsg, callList = nil, nil
		switch jsonString(item["type"]) {
		case "function_call_output":
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": firstNonEmpty(jsonString(item["call_id"]), jsonString(item["id"])),
				"content":      contentPlainText(item["output"]),
			})
		case "reasoning":
			// Replay of a prior reasoning item; chat has no slot for it.
			continue
		default:
			msg := map[string]any{
				"role":    firstNonEmpty(jsonString(item["role"]), "user"),
				"content": contentChatValue(item["content"]),
			}
			if name := jsonString(item["name"]); name != "" {
				msg["name"] = name
			}
			out = append(out, msg)
		}
	}
	// Two adjacent plain-text turns of the same role become one, because strict
	// upstreams reject repeated roles. Ported from A (TraeWorkAssistant, MIT,
	// 朱天伟) wb_payload.rs:14,597 (merge_message).
	return mergeAdjacentMessages(out), nil
}

// contentPlainText flattens content parts into a single chat content value.
// It is the fallback for tool output, where chat has no image slot.
func contentPlainText(raw json.RawMessage) any {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return string(raw)
	}
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		if t := jsonString(part["text"]); t != "" {
			texts = append(texts, t)
			continue
		}
		if img := jsonString(part["image_url"]); img != "" {
			texts = append(texts, "[image_url] "+img)
		}
	}
	return strings.Join(texts, "\n")
}

// contentChatValue flattens Responses content parts, keeping input_image parts
// as chat image_url parts instead of a text placeholder. A string is returned
// when no image is present so callers (and same-role merging) see plain text.
// Ported from A (TraeWorkAssistant, MIT, 朱天伟) payload.rs:467, which drops
// images; here they are passed through because the plugin upstreams accept them.
func contentChatValue(raw json.RawMessage) any {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return string(raw)
	}
	var (
		texts    []string
		ordered  []any
		hasImage bool
	)
	for _, part := range parts {
		if t := jsonString(part["text"]); t != "" {
			texts = append(texts, t)
			ordered = append(ordered, map[string]any{"type": "text", "text": t})
			continue
		}
		if url, ok := responsesImageURL(part); ok {
			hasImage = true
			ordered = append(ordered, chatImagePart(url))
			continue
		}
		// Unsupported shapes keep their placeholder so nothing vanishes silently.
		placeholder := "[image_url]"
		if img := jsonString(part["image_url"]); img != "" {
			placeholder += " " + img
		} else if id := jsonString(part["file_id"]); id != "" {
			placeholder += " file_id:" + id
		}
		texts = append(texts, placeholder)
		ordered = append(ordered, map[string]any{"type": "text", "text": placeholder})
	}
	if !hasImage {
		return strings.Join(texts, "\n")
	}
	return ordered
}

// responsesImageURL extracts a usable image location from an input_image part.
// Both the plain string form and the {url:...} object form are accepted; the
// URL may be an http(s) address or a data: URL.
func responsesImageURL(part map[string]json.RawMessage) (string, bool) {
	if jsonString(part["type"]) != "input_image" {
		return "", false
	}
	if url := jsonString(part["image_url"]); url != "" {
		return url, true
	}
	var obj struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(part["image_url"], &obj) == nil && obj.URL != "" {
		return obj.URL, true
	}
	return "", false
}

// chatImagePart wraps an image URL in the chat content-part shape.
func chatImagePart(url string) map[string]any {
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}
}

// mergeAdjacentMessages folds consecutive plain-text messages of the same role
// into one. Messages carrying tool_calls or tool results are never merged (the
// call ids must stay one per message), and neither are tool/system turns.
// Ported from A (TraeWorkAssistant, MIT, 朱天伟) wb_payload.rs:14,597-724
// (merge_message: string content is joined with a blank line).
func mergeAdjacentMessages(messages []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(messages))
	for _, msg := range messages {
		if n := len(out); n > 0 {
			if merged, ok := mergeChatTextMessage(out[n-1], msg); ok {
				out[n-1] = merged
				continue
			}
		}
		out = append(out, msg)
	}
	return out
}

// mergeChatTextMessage merges extra into base when both are plain text turns of
// the same role, reporting whether it happened.
func mergeChatTextMessage(base, extra map[string]any) (map[string]any, bool) {
	role := jsonTextValue(base["role"])
	if role == "" || role == "tool" || role == "system" {
		return base, false
	}
	if role != jsonTextValue(extra["role"]) {
		return base, false
	}
	if _, ok := base["tool_calls"]; ok {
		return base, false
	}
	if _, ok := extra["tool_calls"]; ok {
		return base, false
	}
	if _, ok := base["tool_call_id"]; ok {
		return base, false
	}
	if _, ok := extra["tool_call_id"]; ok {
		return base, false
	}
	if jsonTextValue(base["name"]) != "" || jsonTextValue(extra["name"]) != "" {
		return base, false
	}
	first, okFirst := base["content"].(string)
	second, okSecond := extra["content"].(string)
	if !okFirst || !okSecond {
		return base, false
	}
	joined := first
	if joined != "" && second != "" {
		joined += "\n\n"
	}
	merged := make(map[string]any, len(base))
	for k, v := range base {
		merged[k] = v
	}
	merged["content"] = joined + second
	return merged, true
}

func jsonTextValue(v any) string {
	s, _ := v.(string)
	return s
}

func jsonString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

func stringOrRaw(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

func jsonNumber(raw json.RawMessage, def float64) float64 {
	if len(raw) == 0 {
		return def
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return def
	}
	return f
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}

func copyTop(dst map[string]json.RawMessage, src map[string]json.RawMessage, keys ...string) {
	for _, key := range keys {
		if v, ok := src[key]; ok && string(v) != "null" {
			dst[key] = v
		}
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func newWireID(prefix string) string {
	return prefix + "_" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// ---------------------------------------------------------------- response bodies

func completionBody(reply chatReply, clientModel string, created int64) map[string]any {
	text, finish := "", "stop"
	if len(reply.Choices) > 0 {
		text = reply.Choices[0].Message.Content
		if r := finishReasonString(reply.Choices[0].FinishReason); r != "" {
			finish = r
		}
	}
	body := map[string]any{
		"id":      firstNonEmpty(reply.ID, newWireID("cmpl")),
		"object":  "text_completion",
		"created": firstNonZero64(reply.Created, created),
		"model":   firstNonEmpty(clientModel, reply.Model),
		"choices": []any{map[string]any{
			"index": 0, "text": text, "logprobs": nil, "finish_reason": finish,
		}},
	}
	if usage := reply.Usage.completionValue(); usage != nil {
		body["usage"] = usage
	}
	return body
}

func responseBody(reply chatReply, clientModel string, created int64) map[string]any {
	// The message item needs its own id; reusing the chat id would make the
	// response and its message indistinguishable.
	output, text := responseOutput(reply, newWireID("msg"))
	finish := ""
	if len(reply.Choices) > 0 {
		finish = finishReasonString(reply.Choices[0].FinishReason)
	}
	status, incomplete := responseStatus(finish)
	body := map[string]any{
		"id":          firstNonEmpty(reply.ID, newWireID("resp")),
		"object":      "response",
		"created_at":  firstNonZero64(reply.Created, created),
		"status":      status,
		"model":       firstNonEmpty(clientModel, reply.Model),
		"output":      output,
		"output_text": text,
	}
	if incomplete != nil {
		body["incomplete_details"] = incomplete
	}
	if usage := reply.Usage.responsesValue(); usage != nil {
		body["usage"] = usage
	}
	return body
}

// responseStatus maps a chat finish reason onto the Responses status pair.
// Ported from A (TraeWorkAssistant, MIT, 朱天伟) wb_responses.rs:519: a length
// stop is "incomplete" with reason max_output_tokens, not a completed answer.
func responseStatus(finish string) (string, map[string]any) {
	if finish == "length" {
		return "incomplete", map[string]any{"reason": "max_output_tokens"}
	}
	return "completed", nil
}

// responseOutput maps one chat message onto Responses output items: reasoning
// first (the upstream only yields raw reasoning text, carried as summary_text),
// then function calls, then the assistant message.
func responseOutput(reply chatReply, msgID string) ([]any, string) {
	output := make([]any, 0, 4)
	if len(reply.Choices) == 0 {
		return output, ""
	}
	msg := reply.Choices[0].Message
	if strings.TrimSpace(msg.ReasoningContent) != "" {
		output = append(output, map[string]any{
			"type": "reasoning", "id": newWireID("rs"),
			"summary": []any{map[string]any{"type": "summary_text", "text": msg.ReasoningContent}},
		})
	}
	for _, call := range msg.ToolCalls {
		output = append(output, map[string]any{
			"type": "function_call", "id": firstNonEmpty(call.ID, newWireID("fc")),
			"call_id": call.ID, "name": call.Function.Name, "arguments": call.Function.Arguments,
			"status": "completed",
		})
	}
	if strings.TrimSpace(msg.Content) == "" {
		return output, ""
	}
	output = append(output, map[string]any{
		"type": "message", "id": firstNonEmpty(msgID, newWireID("msg")),
		"role": "assistant", "status": "completed",
		"content": []any{map[string]any{"type": "output_text", "text": msg.Content, "annotations": []any{}}},
	})
	return output, msg.Content
}

func finishReasonString(v any) string {
	switch typed := v.(type) {
	case string:
		return typed
	case nil:
		return ""
	default:
		return fmt.Sprint(typed)
	}
}

func firstNonZero64(values ...int64) int64 {
	for _, v := range values {
		if v != 0 {
			return v
		}
	}
	return time.Now().Unix()
}
