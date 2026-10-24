package gateway

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// This file lets plugin-backed models answer the native Anthropic Messages API
// (/v1/messages). The request is translated into chat/completions and sent down
// the normal scheduling path; the reply is translated back into a Messages
// object or into Anthropic Server-Sent Events. Bare model names keep the plain
// A passthrough, so A's own Anthropic handling is never involved here.

// defaultAnthropicMaxTokens matches Anthropic's requirement that max_tokens be
// present: clients that omit it get a usable default instead of a plugin error.
const defaultAnthropicMaxTokens = 4096

// thinkingSigningKey is minted once per process. Anthropic clients expect an
// opaque signature on every thinking block (and a signature_delta before the
// block closes); the OpenAI-compatible upstream has no such concept, so the
// gateway signs the reasoning text itself. This is a well-formed placeholder,
// not an Anthropic-issued signature: nothing here validates it, and thinking
// blocks are dropped when a turn is replayed upstream.
var thinkingSigningKey = func() []byte {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return []byte("aiworkgate-thinking-signature")
	}
	return key
}()

func thinkingSignature(text string) string {
	mac := hmac.New(sha256.New, thinkingSigningKey)
	mac.Write([]byte(text))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// ---------------------------------------------------------------- requests

// chatBodyFromAnthropic translates an Anthropic Messages request into chat.
// It returns the number of dropped image blocks so the caller can log them.
// Ported from TraeWorkAssistant src-tauri/src/api_server/payload.rs
// (anthropic_to_openai).
func chatBodyFromAnthropic(body []byte) (out []byte, images int, err error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, 0, &envelopeError{"request body must be a JSON object"}
	}
	modelID := jsonString(top["model"])
	if strings.TrimSpace(modelID) == "" {
		return nil, 0, &envelopeError{"model is required"}
	}

	chat := map[string]json.RawMessage{"model": mustJSON(modelID)}
	copyTop(chat, top, "stream", "temperature", "top_p")
	if raw, ok := top["max_tokens"]; ok && string(raw) != "null" {
		chat["max_tokens"] = raw
	} else {
		chat["max_tokens"] = mustJSON(defaultAnthropicMaxTokens)
	}
	if raw, ok := top["stop_sequences"]; ok {
		var stops []string
		if json.Unmarshal(raw, &stops) == nil && len(stops) > 0 {
			chat["stop"] = raw
		}
	}

	messages, images, err := anthropicMessages(top)
	if err != nil {
		return nil, 0, err
	}
	chat["messages"] = mustJSON(messages)
	if tools, ok := anthropicTools(top["tools"]); ok {
		chat["tools"] = tools
	}
	if choice, ok := anthropicToolChoice(top["tool_choice"]); ok {
		chat["tool_choice"] = choice
	}
	// Unknown top-level fields (thinking, metadata, top_k, ...) are not copied:
	// they have no chat equivalent and the request must not fail because of them.
	encoded, err := json.Marshal(chat)
	if err != nil {
		return nil, 0, err
	}
	return encoded, images, nil
}

// anthropicMessages maps system plus the message list onto chat roles. Images
// are counted and dropped because chat plugins here do not accept vision input.
func anthropicMessages(top map[string]json.RawMessage) ([]map[string]any, int, error) {
	messages := make([]map[string]any, 0, 8)
	if text := anthropicSystemText(top["system"]); text != "" {
		messages = append(messages, map[string]any{"role": "system", "content": text})
	}

	raw, ok := top["messages"]
	if !ok {
		return nil, 0, &envelopeError{"messages is required"}
	}
	var src []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &src); err != nil {
		return nil, 0, &envelopeError{"messages must be an array"}
	}

	images := 0
	for _, msg := range src {
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			role = "user"
		}
		if len(msg.Content) == 0 || string(msg.Content) == "null" {
			continue
		}
		var text string
		if err := json.Unmarshal(msg.Content, &text); err == nil {
			messages = append(messages, map[string]any{"role": role, "content": text})
			continue
		}
		var blocks []map[string]json.RawMessage
		if err := json.Unmarshal(msg.Content, &blocks); err != nil {
			continue
		}

		var (
			texts      []string
			parts      []any
			hasImage   bool
			calls      []any
			results    []map[string]any
			toolImages []any
		)
		for _, block := range blocks {
			switch jsonString(block["type"]) {
			case "text":
				if t := jsonString(block["text"]); t != "" {
					texts = append(texts, t)
					parts = append(parts, map[string]any{"type": "text", "text": t})
				}
			case "tool_use":
				input := block["input"]
				if len(input) == 0 || string(input) == "null" {
					input = mustJSON(map[string]any{})
				}
				calls = append(calls, map[string]any{
					"id":   firstNonEmpty(jsonString(block["id"]), "toolu_"+strconv.Itoa(len(calls))),
					"type": "function",
					"function": map[string]any{
						"name": jsonString(block["name"]),
						// arguments carries the JSON text of input, i.e. JSON.stringify(input).
						"arguments": string(input),
					},
				})
			case "tool_result":
				contentText, imageParts, dropped := anthropicToolResultText(block["content"])
				images += dropped
				toolImages = append(toolImages, imageParts...)
				if jsonBool(block["is_error"]) {
					if contentText == "" {
						contentText = "Error: tool execution failed"
					} else {
						contentText = "Error: " + contentText
					}
				}
				results = append(results, map[string]any{
					"role":         "tool",
					"tool_call_id": jsonString(block["tool_use_id"]),
					"content":      contentText,
				})
			case "image":
				// Chat upstreams accept image_url parts (see plugins/officeace),
				// so a picture is translated instead of dropped. A's payload.rs:467
				// drops it; this gateway does better.
				if part, ok := anthropicImagePart(block); ok {
					parts = append(parts, part)
					hasImage = true
					continue
				}
				images++
			}
		}

		content := anthropicChatContent(texts, parts, hasImage)
		// Chat tool messages are text-only, so images returned by a tool are
		// carried on the user turn that accompanies the tool results. Without
		// this they were counted and dropped (screenshot/tool-vision workflows
		// lost every picture).
		if len(toolImages) > 0 {
			switch c := content.(type) {
			case nil:
				content = toolImages
			case string:
				content = append([]any{map[string]any{"type": "text", "text": c}}, toolImages...)
			case []any:
				content = append(c, toolImages...)
			}
		}
		switch {
		case len(results) > 0:
			// tool_result belongs to the user turn: tool messages first, then text.
			messages = append(messages, results...)
			if content != nil {
				messages = append(messages, map[string]any{"role": role, "content": content})
			}
		case len(calls) > 0:
			entry := map[string]any{"role": "assistant", "content": nil, "tool_calls": calls}
			if content != nil {
				entry["content"] = content
			}
			messages = append(messages, entry)
		case content != nil:
			messages = append(messages, map[string]any{"role": role, "content": content})
		}
	}
	// Anthropic clients may send two adjacent turns with the same role; strict
	// chat upstreams answer that with a 400. Ported from A (TraeWorkAssistant,
	// MIT, 朱天伟) wb_payload.rs:14,597 (merge_message).
	return mergeAdjacentMessages(messages), images, nil
}

// anthropicChatContent returns nil when the turn carried no text and no image.
// Text-only turns keep chat's plain string form; a turn with an image becomes
// an ordered part array so the picture survives.
func anthropicChatContent(texts []string, parts []any, hasImage bool) any {
	if len(texts) == 0 && !hasImage {
		return nil
	}
	if !hasImage {
		return strings.Join(texts, "")
	}
	return parts
}

// anthropicImagePart maps an Anthropic image block onto a chat image_url part.
// A base64 source becomes a data: URL, a url source is passed through.
func anthropicImagePart(block map[string]json.RawMessage) (map[string]any, bool) {
	var source struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	}
	if err := json.Unmarshal(block["source"], &source); err != nil {
		return nil, false
	}
	var url string
	switch source.Type {
	case "base64":
		if source.Data == "" {
			return nil, false
		}
		mediaType := source.MediaType
		if mediaType == "" {
			mediaType = "image/png"
		}
		url = "data:" + mediaType + ";base64," + source.Data
	case "url":
		url = source.URL
	default:
		return nil, false
	}
	if url == "" {
		return nil, false
	}
	return chatImagePart(url), true
}

// anthropicSystemText accepts either the plain string form or the block array.
func anthropicSystemText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var b strings.Builder
	for _, block := range blocks {
		b.WriteString(jsonString(block["text"]))
	}
	return b.String()
}

// anthropicToolResultText flattens the string or block-array result content.
// Supported images are returned as chat image parts so the caller can attach
// them to the user turn; unsupported sources are counted as dropped.
func anthropicToolResultText(raw json.RawMessage) (string, []any, int) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil, 0
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil, 0
	}
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", nil, 0
	}
	var (
		b       strings.Builder
		parts   []any
		dropped int
	)
	for _, block := range blocks {
		if jsonString(block["type"]) == "image" {
			if part, ok := anthropicImagePart(block); ok {
				parts = append(parts, part)
			} else {
				dropped++
			}
			continue
		}
		b.WriteString(jsonString(block["text"]))
	}
	return b.String(), parts, dropped
}

// anthropicTools unwraps {name, description, input_schema} into chat functions.
func anthropicTools(raw json.RawMessage) (json.RawMessage, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, false
	}
	out := make([]any, 0, len(tools))
	for _, tool := range tools {
		name := jsonString(tool["name"])
		if strings.TrimSpace(name) == "" {
			continue
		}
		params := tool["input_schema"]
		if len(params) == 0 || string(params) == "null" {
			params = mustJSON(map[string]any{"type": "object"})
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": jsonString(tool["description"]),
				"parameters":  params,
			},
		})
	}
	if len(out) == 0 {
		return nil, false
	}
	return mustJSON(out), true
}

// anthropicToolChoice maps auto/any/tool/none onto chat's tool_choice values.
func anthropicToolChoice(raw json.RawMessage) (json.RawMessage, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "any" {
			s = "required"
		}
		switch s {
		case "auto", "none", "required":
			return mustJSON(s), true
		}
		return nil, false
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, false
	}
	switch obj.Type {
	case "any":
		return mustJSON("required"), true
	case "tool":
		return mustJSON(map[string]any{
			"type":     "function",
			"function": map[string]any{"name": obj.Name},
		}), true
	case "none":
		return mustJSON("none"), true
	default:
		return mustJSON("auto"), true
	}
}

func jsonBool(raw json.RawMessage) bool {
	var b bool
	return json.Unmarshal(raw, &b) == nil && b
}

// ---------------------------------------------------------------- response bodies

// Ported from TraeWorkAssistant src-tauri/src/api_server/wb_sse.rs
// (completion_to_anthropic).
func anthropicMessageBody(reply chatReply, clientModel, msgID string) map[string]any {
	content := make([]any, 0, 4)
	var (
		message chatMessage
		finish  string
	)
	if len(reply.Choices) > 0 {
		message = reply.Choices[0].Message
		finish = finishReasonString(reply.Choices[0].FinishReason)
	}
	if strings.TrimSpace(message.ReasoningContent) != "" {
		content = append(content, map[string]any{
			"type":      "thinking",
			"thinking":  message.ReasoningContent,
			"signature": thinkingSignature(message.ReasoningContent),
		})
	}
	if message.Content != "" {
		content = append(content, map[string]any{"type": "text", "text": message.Content})
	}
	for _, call := range message.ToolCalls {
		content = append(content, map[string]any{
			"type":  "tool_use",
			"id":    firstNonEmpty(call.ID, newWireID("toolu")),
			"name":  call.Function.Name,
			"input": anthropicToolInput(call.Function.Arguments),
		})
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}
	return map[string]any{
		"id":            msgID,
		"type":          "message",
		"role":          "assistant",
		"model":         firstNonEmpty(clientModel, reply.Model),
		"content":       content,
		"stop_reason":   anthropicStopReason(finish, len(message.ToolCalls) > 0),
		"stop_sequence": nil,
		"usage":         anthropicUsage(reply.Usage),
	}
}

// anthropicStopReason maps chat finish reasons onto the Anthropic vocabulary.
// Truncation wins over tool calls: a "length" turn that also carries tool_use
// must tell the client it was cut off, not invite it to run another tool.
func anthropicStopReason(finish string, hasTools bool) string {
	switch finish {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "refusal"
	default:
		if hasTools {
			return "tool_use"
		}
		return "end_turn"
	}
}

// anthropicToolInput turns the chat arguments string back into an object. The
// Messages API types input as an object, so anything unparsable becomes {}.
func anthropicToolInput(args string) map[string]any {
	args = strings.TrimSpace(args)
	if args == "" {
		return map[string]any{}
	}
	var input map[string]any
	if err := json.Unmarshal([]byte(args), &input); err != nil || input == nil {
		return map[string]any{}
	}
	return input
}

// anthropicUsage builds the Messages usage object. Cache counters follow
// Anthropic's integer shape (0 when unknown) and carry the upstream's cached
// prefix, which tap.go already collects for logging. Ported from A
// (TraeWorkAssistant, MIT, 朱天伟) sse.rs:826-829,922-927.
func anthropicUsage(u *wireUsage) map[string]any {
	in, out := 0, 0
	if u != nil {
		in, out = u.PromptTokens, u.CompletionTokens
	}
	return map[string]any{
		"input_tokens":                in,
		"output_tokens":               out,
		"cache_read_input_tokens":     u.cachedTokens(),
		"cache_creation_input_tokens": u.cacheCreationTokens(),
	}
}

// ---------------------------------------------------------------- errors

type anthropicErrorEnvelope struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		// Code mirrors the OpenAI paths' error.code (model_not_found et al.) so a
		// client can branch on the same value whichever endpoint it called. It is
		// omitted when no code was reported, leaving the shape unchanged.
		Code string `json:"code,omitempty"`
	} `json:"error"`
}

func anthropicErrorBody(typ, msg string) anthropicErrorEnvelope {
	return anthropicErrorBodyCode(typ, msg, "")
}

func anthropicErrorBodyCode(typ, msg, code string) anthropicErrorEnvelope {
	var body anthropicErrorEnvelope
	body.Type = "error"
	body.Error.Type = typ
	body.Error.Message = msg
	body.Error.Code = code
	return body
}

func writeAnthropicError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(anthropicErrorBody(typ, msg))
}

// anthropicErrorType maps an upstream OpenAI error type and HTTP status onto
// the Anthropic error taxonomy.
func anthropicErrorType(status int, upstreamType, code string) string {
	if code == "model_not_found" {
		return "not_found_error"
	}
	switch upstreamType {
	case "invalid_request_error", "not_found_error", "authentication_error",
		"permission_error", "rate_limit_error", "overloaded_error", "api_error":
		return upstreamType
	case "rate_limit_exceeded", "insufficient_quota", "concurrency_limit":
		return "rate_limit_error"
	}
	switch status {
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 429:
		return "rate_limit_error"
	case 529:
		return "overloaded_error"
	}
	if status >= 500 {
		return "api_error"
	}
	return "invalid_request_error"
}

// anthropicErrorFromBody extracts a usable type, code and message from whatever
// the upstream wrote: an OpenAI error object, an Anthropic error object, or text.
// The code is carried through so gateway-originated failures keep reporting
// model_not_found the way the OpenAI paths do.
func anthropicErrorFromBody(body []byte, status int) (typ, code, msg string) {
	var env struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &env) == nil && (env.Error.Message != "" || env.Error.Type != "") {
		typ = anthropicErrorType(status, env.Error.Type, env.Error.Code)
		code = env.Error.Code
		msg = firstNonEmpty(env.Error.Message, env.Error.Code)
		if msg == "" {
			msg = http.StatusText(status)
		}
		return typ, code, msg
	}
	var plain struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &plain) == nil && plain.Message != "" {
		return anthropicErrorType(status, "", ""), "", plain.Message
	}
	msg = truncateUTF8(strings.TrimSpace(string(body)), 512)
	if msg == "" {
		msg = http.StatusText(status)
	}
	return anthropicErrorType(status, "", ""), "", msg
}

// readAnthropicBody buffers /v1/messages request bodies, writing an Anthropic
// shaped error when the body cannot be read.
func readAnthropicBody(w http.ResponseWriter, r *http.Request) ([]byte, int) {
	if r.Body == nil {
		return nil, 0
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes+1))
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return nil, http.StatusBadRequest
	}
	if len(body) > maxRequestBodyBytes {
		writeAnthropicError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body exceeds 32MB")
		return nil, http.StatusRequestEntityTooLarge
	}
	return body, 0
}

// ---------------------------------------------------------------- writer

// anthropicWriter adapts the chat/completions reply produced by a plugin into
// a Messages object or the Anthropic SSE event stream. It is an
// http.ResponseWriter so the existing forwarding and scheduling paths can use
// it unchanged. Ported from TraeWorkAssistant wb_sse.rs (finish_stream).
type anthropicWriter struct {
	dst    http.ResponseWriter
	model  string
	status int
	header http.Header

	wantStream bool
	started    bool
	stream     bool
	pending    []byte
	capped     bool
	body       bytes.Buffer

	msgID string
	// Block order is fixed: thinking, then text, then tool_use.
	blockIndex     int
	messageStarted bool
	thinkingOpen   bool
	textOpen       bool
	reasoning      strings.Builder
	text           strings.Builder
	calls          []*callState
	usage          *wireUsage
	finishReason   string
	// event is the "event:" name of the frame being parsed; failed records that
	// an upstream error frame already ended the stream.
	event  string
	failed bool

	finished bool
}

func newAnthropicWriter(dst http.ResponseWriter, clientModel string, wantStream bool) *anthropicWriter {
	return &anthropicWriter{
		dst:        dst,
		model:      clientModel,
		status:     http.StatusOK,
		header:     http.Header{},
		wantStream: wantStream,
		blockIndex: -1,
		msgID:      newWireID("msg"),
	}
}

func (a *anthropicWriter) Header() http.Header { return a.header }

func (a *anthropicWriter) WriteHeader(code int) {
	if a.started {
		return
	}
	a.status = code
	contentType := strings.ToLower(a.header.Get("Content-Type"))
	switch {
	case strings.Contains(contentType, "text/event-stream"):
		a.stream = true
	case strings.Contains(contentType, "application/json"):
		a.stream = false
	default:
		a.stream = a.wantStream
	}
}

func (a *anthropicWriter) Write(p []byte) (int, error) {
	switch {
	case a.status >= 400:
		// Error replies are re-encoded at finish; the upstream body only has to
		// survive long enough to recover its message.
		if !a.capped {
			if a.body.Len()+len(p) > maxEnvelopeBuffer {
				a.capped = true
				a.body.Reset()
			} else {
				a.body.Write(p)
			}
		}
		return len(p), nil
	case a.status != http.StatusOK:
		a.emitHeaders(a.passthroughContentType())
		return a.dst.Write(p)
	}
	if a.stream {
		return a.writeStream(p)
	}
	if a.capped {
		return len(p), nil
	}
	if a.body.Len()+len(p) > maxEnvelopeBuffer {
		a.capped = true
		return len(p), nil
	}
	return a.body.Write(p)
}

func (a *anthropicWriter) passthroughContentType() string {
	if ct := a.header.Get("Content-Type"); ct != "" {
		return ct
	}
	return "application/json; charset=utf-8"
}

func (a *anthropicWriter) writeStream(p []byte) (int, error) {
	a.pending = append(a.pending, p...)
	if len(a.pending) > maxEnvelopeBuffer {
		a.pending = a.pending[:0]
		a.capped = true
		return len(p), nil
	}
	for {
		idx := bytes.IndexByte(a.pending, '\n')
		if idx < 0 {
			break
		}
		line := append([]byte(nil), a.pending[:idx]...)
		a.pending = a.pending[idx+1:]
		a.handleLine(line)
	}
	return len(p), nil
}

func (a *anthropicWriter) Flush() {
	if f, ok := a.dst.(http.Flusher); ok && a.started {
		f.Flush()
	}
}

func (a *anthropicWriter) emitHeaders(contentType string) {
	if a.started {
		return
	}
	dst := a.dst.Header()
	for key, values := range a.header {
		if strings.EqualFold(key, "Content-Type") || strings.EqualFold(key, "Content-Length") {
			continue
		}
		for _, v := range values {
			dst.Add(key, v)
		}
	}
	dst.Set("Content-Type", contentType)
	a.started = true
	a.dst.WriteHeader(a.status)
}

// finish is idempotent; the handler defers it after the upstream call returns.
func (a *anthropicWriter) finish() {
	if a.finished {
		return
	}
	a.finished = true
	switch {
	case a.status >= 400:
		typ, code, msg := anthropicErrorFromBody(a.body.Bytes(), a.status)
		a.emitJSONErrorWithCode(a.status, typ, msg, code)
	case a.status != http.StatusOK:
		a.emitHeaders(a.passthroughContentType())
	case a.stream:
		a.finishStream()
	default:
		a.bufferedReply()
	}
}

// ------------------------------------------------------------------ buffered

func (a *anthropicWriter) bufferedReply() {
	if a.capped {
		a.emitJSONError(http.StatusBadGateway, "api_error", "upstream reply too large to convert")
		return
	}
	raw := bytes.TrimSpace(a.body.Bytes())
	if len(raw) == 0 {
		a.emitJSONError(http.StatusBadGateway, "api_error", "empty upstream reply")
		return
	}
	var reply chatReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		a.emitJSONError(http.StatusBadGateway, "api_error", "upstream reply is not valid JSON")
		return
	}
	if len(reply.Error) > 0 && string(reply.Error) != "null" {
		typ, code, msg := anthropicErrorFromBody(raw, http.StatusBadGateway)
		a.emitJSONErrorWithCode(http.StatusBadGateway, typ, msg, code)
		return
	}
	data, err := json.Marshal(anthropicMessageBody(reply, a.model, a.msgID))
	if err != nil {
		a.emitJSONError(http.StatusBadGateway, "api_error", "failed to encode response")
		return
	}
	a.emitJSON(data)
}

func (a *anthropicWriter) emitJSON(data []byte) {
	a.emitHeaders("application/json; charset=utf-8")
	a.writeBody(data)
}

func (a *anthropicWriter) emitJSONError(status int, typ, msg string) {
	a.emitJSONErrorWithCode(status, typ, msg, "")
}

// emitJSONErrorWithCode adds the upstream error code (e.g. model_not_found) to
// the Anthropic error object; an empty code keeps the two-field shape.
func (a *anthropicWriter) emitJSONErrorWithCode(status int, typ, msg, code string) {
	if a.started {
		return
	}
	a.status = status
	data, err := json.Marshal(anthropicErrorBodyCode(typ, msg, code))
	if err != nil {
		return
	}
	a.emitJSON(data)
}

func (a *anthropicWriter) writeBody(data []byte) {
	_, _ = a.dst.Write(data)
	if f, ok := a.dst.(http.Flusher); ok {
		f.Flush()
	}
}

// ------------------------------------------------------------------ streaming

func (a *anthropicWriter) flushPending() {
	if len(bytes.TrimSpace(a.pending)) == 0 {
		return
	}
	a.handleLine(a.pending)
	a.pending = a.pending[:0]
}

func (a *anthropicWriter) handleLine(raw []byte) {
	line := strings.TrimRight(string(raw), "\r")
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		a.event = ""
		return
	}
	if strings.HasPrefix(trimmed, ":") || strings.HasPrefix(trimmed, "id:") ||
		strings.HasPrefix(trimmed, "retry:") {
		return
	}
	if strings.HasPrefix(trimmed, "event:") {
		a.event = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
		return
	}
	if !strings.HasPrefix(trimmed, "data:") {
		trimmed = "data: " + trimmed
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	event := a.event
	a.event = ""
	if payload == "[DONE]" || a.failed {
		return
	}
	var chunk chatChunk
	if event == "error" {
		// Some upstreams send a non-JSON payload after "event: error"; the frame
		// still ends the stream as a failure.
		_ = json.Unmarshal([]byte(payload), &chunk)
		a.failStream(chunk, payload)
		return
	}
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return
	}
	// A mid-stream failure must close the open block and end as event: error;
	// silently dropping it would fake a successful finish. Ported from A
	// (TraeWorkAssistant, MIT, 朱天伟) sse.rs:1055-1077, wb_sse.rs:87.
	if chunk.Error != nil {
		a.failStream(chunk, payload)
		return
	}
	if chunk.Usage != nil {
		a.usage = chunk.Usage
	}
	if len(chunk.Choices) == 0 {
		return
	}
	delta := chunk.Choices[0].Delta
	if delta.ReasoningContent != "" {
		a.reasoning.WriteString(delta.ReasoningContent)
		a.emitThinking(delta.ReasoningContent)
	}
	if delta.Content != "" {
		a.text.WriteString(delta.Content)
		a.emitText(delta.Content)
	}
	for _, call := range delta.ToolCalls {
		a.calls = appendToolDelta(a.calls, call)
	}
	if reason := finishReasonFromRaw(chunk.Choices[0].FinishReason); reason != "" {
		a.finishReason = reason
	}
}

// appendToolDelta folds one tool-call fragment into its slot, treating a
// fragment that carries an id or name as the start of a new call.
func appendToolDelta(calls []*callState, call toolDelta) []*callState {
	index := -1
	if call.Index != nil {
		index = *call.Index
	} else if call.ID != "" || call.Function.Name != "" {
		index = len(calls)
	} else if len(calls) > 0 {
		index = len(calls) - 1
	} else {
		index = 0
	}
	if index < 0 {
		index = 0
	}
	for len(calls) <= index {
		calls = append(calls, &callState{})
	}
	state := calls[index]
	if call.ID != "" {
		state.id = call.ID
	}
	if call.Function.Name != "" {
		state.name = call.Function.Name
	}
	state.args.WriteString(call.Function.Arguments)
	return calls
}

func (a *anthropicWriter) ensureMessage() {
	if a.messageStarted {
		return
	}
	a.messageStarted = true
	a.emitEvent("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": a.msgID, "type": "message", "role": "assistant", "model": a.model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			// Usage seen before the first delta (some upstreams send it first) is
			// carried here, cache counters included.
			"usage": anthropicUsage(a.usage),
		},
	})
}

func (a *anthropicWriter) openBlock() int {
	a.blockIndex++
	return a.blockIndex
}

func (a *anthropicWriter) emitThinking(text string) {
	if !a.thinkingOpen {
		a.closeText()
		a.ensureMessage()
		a.thinkingOpen = true
		index := a.openBlock()
		a.emitEvent("content_block_start", map[string]any{
			"type": "content_block_start", "index": index,
			"content_block": map[string]any{"type": "thinking", "thinking": ""},
		})
	}
	a.emitEvent("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": a.blockIndex,
		"delta": map[string]any{"type": "thinking_delta", "thinking": text},
	})
}

func (a *anthropicWriter) emitText(text string) {
	if !a.textOpen {
		a.closeThinking()
		a.ensureMessage()
		a.textOpen = true
		index := a.openBlock()
		a.emitEvent("content_block_start", map[string]any{
			"type": "content_block_start", "index": index,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
	}
	a.emitEvent("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": a.blockIndex,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
}

func (a *anthropicWriter) closeThinking() {
	if !a.thinkingOpen {
		return
	}
	a.thinkingOpen = false
	// Anthropic sends the block's signature as the last delta before the stop.
	// Clients that keep thinking blocks in history reject ones without it.
	a.emitEvent("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": a.blockIndex,
		"delta": map[string]any{
			"type":      "signature_delta",
			"signature": thinkingSignature(a.reasoning.String()),
		},
	})
	a.emitEvent("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": a.blockIndex,
	})
}

func (a *anthropicWriter) closeText() {
	if !a.textOpen {
		return
	}
	a.textOpen = false
	a.emitEvent("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": a.blockIndex,
	})
}

// hasOutput reports whether the upstream produced anything a client can use.
// An empty stream is an upstream failure, not a blank successful answer.
func (a *anthropicWriter) hasOutput() bool {
	if a.reasoning.Len() > 0 || a.text.Len() > 0 {
		return true
	}
	for _, call := range a.calls {
		if call != nil && strings.TrimSpace(call.name) != "" {
			return true
		}
	}
	return false
}

func (a *anthropicWriter) finishStream() {
	a.flushPending()
	if a.failed {
		// The upstream error frame already terminated the stream; a message_stop
		// after event: error would claim success.
		return
	}
	if !a.hasOutput() {
		a.emitErrorEvent("api_error", "upstream stream ended without content")
		return
	}
	a.ensureMessage()
	a.closeThinking()
	a.closeText()

	toolCount := 0
	for _, call := range a.calls {
		if call == nil || strings.TrimSpace(call.name) == "" {
			continue
		}
		toolCount++
		index := a.openBlock()
		id := firstNonEmpty(call.id, newWireID("toolu"))
		a.emitEvent("content_block_start", map[string]any{
			"type": "content_block_start", "index": index,
			"content_block": map[string]any{"type": "tool_use", "id": id, "name": call.name, "input": map[string]any{}},
		})
		if args := normalizedToolArgs(call.args.String()); args != "" {
			a.emitEvent("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": index,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
			})
		}
		a.emitEvent("content_block_stop", map[string]any{
			"type": "content_block_stop", "index": index,
		})
	}

	a.emitEvent("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": anthropicStopReason(a.finishReason, toolCount > 0), "stop_sequence": nil},
		"usage": anthropicUsage(a.usage),
	})
	a.emitEvent("message_stop", map[string]any{"type": "message_stop"})
}

// normalizedToolArgs keeps the whole tool input in one delta, but only when it
// parses: the official SDK concatenates partial_json and parses it at block end.
func normalizedToolArgs(args string) string {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" || !json.Valid([]byte(trimmed)) {
		return "{}"
	}
	return trimmed
}

// failStream ends the stream the way the Messages API requires: the open
// content_block is closed, then event: error carries the upstream failure. No
// message_delta/message_stop follows. Ported from A (TraeWorkAssistant, MIT,
// 朱天伟) sse.rs:1055-1077 (close_text_block! before sending event:error).
func (a *anthropicWriter) failStream(chunk chatChunk, payload string) {
	if a.failed {
		return
	}
	a.failed = true
	a.closeThinking()
	a.closeText()
	typ, code, msg := streamErrorFromFrame(chunk, payload)
	if msg == "" {
		msg = "upstream stream error"
	}
	a.emitErrorEvent(anthropicErrorType(http.StatusInternalServerError, typ, code), msg)
}

// emitErrorEvent reports a mid-stream failure the way the Anthropic SDK
// expects. It never fabricates a successful message around the failure.
func (a *anthropicWriter) emitErrorEvent(typ, msg string) {
	data, err := json.Marshal(anthropicErrorBody(typ, msg))
	if err != nil {
		return
	}
	a.emitRaw("event: error\ndata: " + string(data) + "\n\n")
}

func (a *anthropicWriter) emitEvent(event string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	a.emitRaw("event: " + event + "\ndata: " + string(data) + "\n\n")
}

func (a *anthropicWriter) emitRaw(s string) {
	a.emitHeaders("text/event-stream")
	_, _ = a.dst.Write([]byte(s))
	if f, ok := a.dst.(http.Flusher); ok {
		f.Flush()
	}
}

// handleMessages implements POST /v1/messages for plugin models. Bare model
// names keep the byte-for-byte A passthrough.
func (g *Gateway) handleMessages(w http.ResponseWriter, r *http.Request, rec *reqRecord) {
	body, failStatus := readAnthropicBody(w, r)
	if failStatus != 0 {
		rec.status = failStatus
		return
	}
	g.prepareRecord(rec, r, body, parseRequestMeta(body))

	scheduled := g.sched != nil && g.snapshot != nil
	tgt, isPlugin := g.matchPlugin(rec.model)
	// A bare name may be served by the configured default plugin; folding the
	// answer into the same tgt/isPlugin pair reuses the translation branch below
	// instead of opening a second, A-shaped path.
	if !isPlugin {
		def, ok, derr := g.bareModelTarget(rec.model)
		switch {
		case derr != nil && errors.Is(derr, errNoDefaultRoute):
			g.writeUnknownModel(w, rec, rec.model, "")
			return
		case derr != nil:
			g.fail(w, rec, http.StatusServiceUnavailable, "plugin_unavailable", "plugin_unavailable", derr.Error())
			return
		case ok:
			tgt, isPlugin = def, true
		}
	}
	bound := scheduled && !isPlugin && g.hasBinding(r.Context(), rec.model)
	if !isPlugin && !bound {
		g.forwardA(w, r, rec, body)
		return
	}

	aw := newAnthropicWriter(w, rec.model, rec.stream)
	defer aw.finish()

	if isPlugin && tgt.inner == "" {
		g.writeUnknownModel(aw, rec, rec.model, tgt.status.Manifest.ID)
		return
	}

	var send chatSender
	if scheduled {
		send = func(dst http.ResponseWriter, chatBody []byte) {
			if isPlugin {
				rec.source = "plugin:" + tgt.status.Manifest.ID
			} else {
				rec.source = scheduledSourceName
			}
			g.forwardScheduled(dst, r, rec, chatBody, "/v1/chat/completions")
		}
		// A plugin that publishes no accounts cannot be scheduled at all: the
		// picker finds no candidate and answers 503. Such a plugin forwards its
		// own upstream directly, so the send seam takes the same shape as the
		// unscheduled plugin branch below.
		if isPlugin && g.pluginWithoutAccounts(r.Context(), tgt.status.Manifest.ID) {
			send = func(dst http.ResponseWriter, chatBody []byte) {
				rec.source = "plugin:" + tgt.status.Manifest.ID
				g.forwardPlugin(dst, r, rec, tgt, chatBody, "/v1/chat/completions")
			}
		}
	} else {
		send = func(dst http.ResponseWriter, chatBody []byte) {
			rec.source = "plugin:" + tgt.status.Manifest.ID
			g.forwardPlugin(dst, r, rec, tgt, chatBody, "/v1/chat/completions")
		}
	}
	g.relayMessages(aw, rec, body, chatSenderForMessages(r, send))
}

// nativeAnthropicHeaders only mean something to a native Anthropic upstream. A
// chat upstream answers them with 400 ("检测到 anthropic-version 头"), which is
// why a Messages request translated into chat must not carry them further.
var nativeAnthropicHeaders = []string{"anthropic-version", "anthropic-beta"}

// chatSenderForMessages wraps the plugin/scheduler sender so the native
// Anthropic headers leave the client request exactly when the translated body is
// handed over as chat: relayMessages calls send only after
// chatBodyFromAnthropic re-encoded the Messages body, and forwardPlugin /
// forwardScheduled clone r.Header from there. The A passthrough of a bare model
// (forwardA) never uses this wrapper, so A keeps receiving the client's headers.
func chatSenderForMessages(r *http.Request, send chatSender) chatSender {
	return func(dst http.ResponseWriter, chatBody []byte) {
		for _, name := range nativeAnthropicHeaders {
			r.Header.Del(name)
		}
		send(dst, chatBody)
	}
}

// relayMessages translates an Anthropic Messages body into a chat request and
// hands it to send, which writes the chat reply into aw. The conversion is kept
// here only; the transport is the chatSender seam, so plugin/scheduler targets
// (handleMessages) and custom upstreams (custom_models.go) share it.
func (g *Gateway) relayMessages(aw http.ResponseWriter, rec *reqRecord, body []byte, send chatSender) {
	chatBody, images, err := chatBodyFromAnthropic(body)
	if err != nil {
		g.fail(aw, rec, http.StatusBadRequest, "invalid_request_error", "unsupported_request", err.Error())
		return
	}
	if images > 0 && g.schedLog != nil {
		// Vision input has no chat equivalent here; dropping it is logged so the
		// operator can explain a reply that ignored the picture.
		g.schedLog.Warn("gateway", "anthropic image blocks dropped", map[string]any{
			"req_id": rec.reqID, "model": rec.model, "count": images,
		})
	}
	send(aw, chatBody)
}
