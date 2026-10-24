package gateway

// User-defined custom models: direct routing to an OpenAI-compatible upstream.
//
// Ported from A (TraeWorkAssistant, MIT, 朱天伟):
//   - src-tauri/src/api_server/custom_models.rs:23-54 (entry shape and the
//     "name is the routing key" rule), 118-181 (canonical match on enabled
//     entries, chat URL normalisation);
//   - src-tauri/src/api_server/dispatch.rs:405-425 (step ⓪: a custom hit
//     bypasses the scheduler and plugins; a single upstream, no cross-pool
//     fallback) and routes.rs:876-893 (execution path re-reads the entry);
//   - src-tauri/src/api_server/custom_route.rs:23-50 (JSON content type,
//     Bearer auth from the stored key, stream forwarded as SSE).
//
// Differences from A on purpose:
//   - A forces `stream:true` upstream and locally aggregates for non-stream
//     clients. This host keeps the client's own stream flag and forwards the
//     bytes (rewriting only the `model` field), because the gateway already
//     has a byte-for-byte SSE pipeline and usage tap (tap.go);
//   - the API key lives in the DPAPI secret store, not in a data file, and is
//     looked up per request through Options.CustomModelKey;
//   - /v1/messages, /v1/responses and /v1/completions are served as well: the
//     request goes through the same converters the plugin path uses
//     (chatBodyFromAnthropic, chatBodyFromResponses, chatBodyFromCompletions)
//     and the chat reply is written into the same anthropicWriter /
//     envelopeWriter, via the chatSender seam (gateway.go). One converter per
//     protocol, two transports. The passthrough endpoints (embeddings, images)
//     remain A-only.
//
// Logging and key usage are not duplicated: the handler fills the same
// reqRecord the rest of the gateway uses, so ServeHTTP's deferred emit() and
// recordKeyUsage() write the api_logs row (source "custom:<row id>") and count
// the request once.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// customUpstreamTimeout bounds one custom upstream call, stream included
	// (the local request context still cancels it earlier).
	customUpstreamTimeout = 120 * time.Second
	// customErrorBodyBytes caps the upstream error body read for the envelope.
	customErrorBodyBytes = 8 << 10
	// customResponseBodyBytes caps a non-stream upstream response.
	customResponseBodyBytes = 32 << 20
	// customErrorSnippetBytes bounds the upstream message echoed to the client.
	customErrorSnippetBytes = 300
)

// CustomModel is the gateway's read-only view of one user-defined upstream.
// The API key never travels in this struct.
type CustomModel struct {
	ID               string
	ConnectionID     string
	Name             string
	Protocol         string
	BaseURL          string
	UpstreamModel     string
	Enabled           bool
	ConnectionEnabled bool
	ContextWindow     int
	MaxOutputTokens  int
	InputTypes       []string
	ReasoningTiers   []string
	DefaultReasoning string
}

// CustomChatURL normalises a custom base URL to its chat endpoint:
// base ending in "/v1" -> "<base>/chat/completions", otherwise
// "<base>/v1/chat/completions". Ported from A custom_models.rs:185-193.
func CustomChatURL(baseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if strings.HasSuffix(base, "/v1") {
		return base + "/chat/completions"
	}
	return base + "/v1/chat/completions"
}

// customModelList loads the provider list. A nil provider or a failing store
// degrades to "no custom models" so a database hiccup never blocks traffic.
func (g *Gateway) customModelList() []CustomModel {
	if g == nil || g.customModels == nil {
		return nil
	}
	list, err := g.customModels()
	if err != nil {
		return nil
	}
	return list
}

// customModelFor returns the enabled entry whose name canonicalises to model.
// Exact canonical match only (A custom_models.rs:173-181); the first match
// wins if an abnormal database holds duplicates.
func (g *Gateway) customModelFor(model string) (CustomModel, bool) {
	key := CanonicalModelID(model)
	if key == "" {
		return CustomModel{}, false
	}
	for _, cm := range g.customModelList() {
		if !cm.Enabled || (cm.ConnectionID != "" && !cm.ConnectionEnabled) {
			continue
		}
		if CanonicalModelID(cm.Name) == key {
			return cm, true
		}
	}
	return CustomModel{}, false
}

// customKey returns the stored upstream key of one entry.
func (g *Gateway) customKey(id string) string {
	if g == nil || g.customModelKey == nil || id == "" {
		return ""
	}
	key, _ := g.customModelKey(id)
	return strings.TrimSpace(key)
}

// restoreBody hands a buffered body back to the next handler unchanged.
func restoreBody(r *http.Request, body []byte) {
	r.Body = io.NopCloser(bytes.NewReader(body))
}

// handleCustomModel claims POST /v1/chat/completions, /v1/messages,
// /v1/responses and /v1/completions when the requested model is an enabled
// custom model. It reports false (body restored) when the request belongs to
// the normal scheduler/plugin/A pipeline.
func (g *Gateway) handleCustomModel(w http.ResponseWriter, r *http.Request, rec *reqRecord) bool {
	path := r.URL.Path
	switch path {
	case "/v1/chat/completions", "/v1/messages", "/v1/responses", "/v1/completions":
	default:
		return false
	}

	var (
		body       []byte
		failStatus int
	)
	if path == "/v1/messages" {
		// Anthropic clients get an Anthropic shaped read error.
		body, failStatus = readAnthropicBody(w, r)
	} else {
		body, failStatus = readRequestBody(w, r)
	}
	if failStatus != 0 {
		rec.status = failStatus
		rec.errText = "failed to read request body"
		return true
	}
	meta := parseRequestMeta(body)
	if !meta.valid {
		// Let the normal handler answer invalid_json exactly as before.
		restoreBody(r, body)
		return false
	}
	cm, ok := g.customModelFor(meta.model)
	if !ok {
		restoreBody(r, body)
		return false
	}

	// A key bound to one plugin keeps its binding semantics: its bare model
	// names are rewritten to "<pool>/<model>" and therefore never reach a
	// custom upstream.
	if kr, found := keyRecordFrom(r.Context()); found && strings.TrimSpace(kr.BindPool) != "" {
		restoreBody(r, body)
		return false
	}

	g.prepareRecord(rec, r, body, meta)
	rec.model = strings.TrimSpace(meta.model)
	rec.source = "custom:" + cm.ID

	// The key's model scope applies to custom ids like any other model.
	if kr, found := keyRecordFrom(r.Context()); found && !modelInScope(kr.ModelScope, rec.model) {
		g.failKeyScope(w, rec, rec.model, "该模型不在该 Key 的模型范围内")
		return true
	}

	switch path {
	case "/v1/messages":
		// The converter is shared with the plugin path; the sender is the only
		// custom-specific part (see customSender).
		aw := newAnthropicWriter(w, rec.model, rec.stream)
		aw.Header().Set(sourceHeader, rec.source)
		defer aw.finish()
		g.relayMessages(aw, rec, body, g.customSender(r, rec, cm))
	case "/v1/responses":
		g.forwardCustomEnvelope(w, r, rec, cm, body, envelopeResponses)
	case "/v1/completions":
		g.forwardCustomEnvelope(w, r, rec, cm, body, envelopeCompletions)
	default:
		g.forwardCustom(w, r, rec, cm, body)
	}
	return true
}

// forwardCustomEnvelope relays a completions/Responses request through the
// shared envelope converter. The adapter parses the chat reply (JSON or SSE)
// and writes the protocol the client asked for, incomplete status and usage
// details included.
func (g *Gateway) forwardCustomEnvelope(w http.ResponseWriter, r *http.Request, rec *reqRecord, cm CustomModel, body []byte, kind envelopeKind) {
	ew := newEnvelopeWriter(w, kind, rec.model, rec.stream)
	ew.Header().Set(sourceHeader, rec.source)
	defer ew.finish()
	g.relayEnvelope(ew, rec, body, kind, g.customSender(r, rec, cm))
}

// customSender is the custom-upstream side of the chatSender seam: one
// converted chat body in, the raw upstream reply copied into the adapter.
func (g *Gateway) customSender(r *http.Request, rec *reqRecord, cm CustomModel) chatSender {
	return func(dst http.ResponseWriter, chatBody []byte) {
		g.customCall(dst, r, rec, cm, chatBody, func(resp *http.Response) {
			g.copyCustomConverted(dst, rec, resp)
		})
	}
}

// forwardCustom proxies one chat request to its custom upstream.
func (g *Gateway) forwardCustom(w http.ResponseWriter, r *http.Request, rec *reqRecord, cm CustomModel, body []byte) {
	g.customCall(w, r, rec, cm, body, func(resp *http.Response) {
		upstreamSSE := strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
		switch {
		case upstreamSSE:
			g.copyCustomStream(w, resp, rec, cm.Name)
		case rec.stream:
			g.copyCustomAsStream(w, resp, rec, cm.Name)
		default:
			g.copyCustomBody(w, resp, rec, cm.Name)
		}
	})
}

// customCall performs one chat call against the custom upstream and hands a
// successful reply to consume. Every failure (invalid URL, timeout,
// unreachable, non-2xx) is written to dst with a redacted message; dst may be a
// protocol adapter, which then re-encodes the error for its own client.
// Request construction, auth and error mapping live here exactly once.
func (g *Gateway) customCall(dst http.ResponseWriter, r *http.Request, rec *reqRecord, cm CustomModel, chatBody []byte, consume func(*http.Response)) {
	key := g.customKey(cm.ConnectionID)
	if key == "" {
		// Legacy rows created before connections stored the key under the model id.
		key = g.customKey(cm.ID)
	}
	prepared, tier, settingsErr := applyCustomModelSettings(cm, chatBody)
	if settingsErr != nil {
		g.fail(dst, rec, settingsErr.status, settingsErr.typ, settingsErr.code, settingsErr.msg)
		return
	}
	protocol := NormalizeCustomProtocolName(cm.Protocol)
	endpoint, err := CustomProtocolChatURL(protocol, cm.BaseURL, cm.UpstreamModel, rec.stream)
	if err != nil {
		rec.errText = "custom model base_url is invalid"
		g.fail(dst, rec, http.StatusBadGateway, "upstream_unavailable", "upstream_unavailable",
			fmt.Sprintf("自定义模型 %s 的 API 地址无效", cm.Name))
		return
	}
	var payload []byte
	switch protocol {
	case "anthropic":
		payload, err = chatToAnthropicRequest(prepared, cm.UpstreamModel, tier)
	case "gemini":
		payload, err = chatToGeminiRequest(prepared, cm.UpstreamModel, tier)
	default:
		payload, err = rewriteModel(prepared, cm.UpstreamModel)
	}
	if err != nil {
		g.fail(dst, rec, http.StatusBadRequest, "invalid_request_error", "unsupported_request",
			"无法把请求转换成上游协议："+err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), customUpstreamTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		// The URL is only ever echoed through net/url errors, so the message
		// stays generic: neither the query nor the key can leak.
		rec.errText = "custom model base_url is invalid"
		g.fail(dst, rec, http.StatusBadGateway, "upstream_unavailable", "upstream_unavailable",
			fmt.Sprintf("自定义模型 %s 的 API 地址无效", cm.Name))
		return
	}
	// Only the gateway-derived headers: the AIWorkGate client key is never
	// forwarded upstream.
	req.Header.Set("Content-Type", "application/json")
	if rec.stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	for name, value := range CustomProtocolHeaders(protocol, key) {
		req.Header.Set(name, value)
	}

	resp, err := g.client.Do(req)
	if err != nil {
		switch {
		case r.Context().Err() != nil:
			rec.status = 499
			rec.errText = "client canceled"
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			rec.errText = "custom upstream timeout"
			g.fail(dst, rec, http.StatusBadGateway, "upstream_unavailable", "upstream_unavailable",
				fmt.Sprintf("自定义模型 %s 上游超时（%d 秒）", cm.Name, int(customUpstreamTimeout/time.Second)))
		default:
			rec.errText = "custom upstream unreachable"
			g.fail(dst, rec, http.StatusBadGateway, "upstream_unavailable", "upstream_unavailable",
				fmt.Sprintf("自定义模型 %s 上游不可达（请检查 API 地址与网络）", cm.Name))
		}
		return
	}
	defer resp.Body.Close()

	rec.status = resp.StatusCode
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		rec.errText = customUpstreamError(resp, key)
		status := resp.StatusCode
		if status < 400 {
			status = http.StatusBadGateway
		}
		g.fail(dst, rec, status, "upstream_error", "custom_upstream_error", rec.errText)
		return
	}
	if protocol != "openai" {
		translated, terr := translateCustomResponse(protocol, resp, rec.model)
		if terr != nil {
			rec.errText = "custom upstream response translation failed"
			g.fail(dst, rec, http.StatusBadGateway, "upstream_error", "custom_upstream_error",
				"转换上游响应失败："+terr.Error())
			return
		}
		resp = translated
	}
	consume(resp)
}

// copyCustomConverted copies a successful upstream reply into a protocol
// adapter verbatim. The adapter (anthropicWriter / envelopeWriter) parses the
// chat frames and writes its own protocol. Usage is tapped from the upstream
// bytes, so the log row and the key quota see the same numbers as the chat
// path and the request is still counted once by ServeHTTP's recorder.
func (g *Gateway) copyCustomConverted(dst http.ResponseWriter, rec *reqRecord, resp *http.Response) {
	contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if contentType == "" {
		contentType = "application/json"
	}
	isSSE := strings.Contains(strings.ToLower(contentType), "text/event-stream")
	header := dst.Header()
	header.Set("Content-Type", contentType)
	header.Set(sourceHeader, rec.source)
	dst.WriteHeader(resp.StatusCode)
	flusher, _ := dst.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}

	tap := newBodyTap(g.bodyCap, isSSE)
	ttfbSet := false
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if !ttfbSet {
				rec.ttfb = g.now().Sub(rec.start)
				rec.firstTokenAt = g.now()
				ttfbSet = true
			}
			tap.feed(buf[:n])
			if _, werr := dst.Write(buf[:n]); werr != nil {
				rec.errText = "client write failed"
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				rec.errText = "custom upstream read failed"
			}
			break
		}
	}
	tap.finish()
	rec.prompt, rec.completion, rec.cached = tap.prompt, tap.completion, tap.cached
	if g.debugBody.Load() {
		rec.bodyResp = string(tap.preview)
	}
}

// customUpstreamError builds the client-facing message of a non-2xx upstream
// response. The stored key is redacted even if the upstream echoes it, and the
// request URL (which could carry a query) is never included.
func customUpstreamError(resp *http.Response, key string) string {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, customErrorBodyBytes))
	message := ""
	var payload struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if json.Unmarshal(raw, &payload) == nil {
		switch {
		case len(payload.Error) > 0:
			var text string
			if json.Unmarshal(payload.Error, &text) == nil {
				message = text
			} else {
				var object struct {
					Message string `json:"message"`
				}
				if json.Unmarshal(payload.Error, &object) == nil {
					message = object.Message
				}
			}
		case payload.Message != "":
			message = payload.Message
		}
	}
	if strings.TrimSpace(message) == "" {
		message = truncateUTF8(strings.TrimSpace(string(raw)), customErrorSnippetBytes)
	}
	message = redactSecret(message, key)
	if message == "" {
		return fmt.Sprintf("自定义模型上游返回状态 %d", resp.StatusCode)
	}
	return fmt.Sprintf("自定义模型上游返回状态 %d：%s", resp.StatusCode, message)
}

// redactSecret removes every occurrence of the upstream key from text.
func redactSecret(text, secret string) string {
	secret = strings.TrimSpace(secret)
	if secret == "" || text == "" {
		return text
	}
	text = strings.ReplaceAll(text, "Bearer "+secret, "Bearer ***")
	return strings.ReplaceAll(text, secret, "***")
}

// copyCustomBody forwards a non-stream JSON body, rewriting the echoed model.
func (g *Gateway) copyCustomBody(w http.ResponseWriter, resp *http.Response, rec *reqRecord, clientModel string) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, customResponseBodyBytes+1))
	if err != nil {
		rec.errText = "custom upstream read failed"
		g.fail(w, rec, http.StatusBadGateway, "upstream_unavailable", "upstream_unavailable",
			"读取自定义模型上游响应失败")
		return
	}
	if len(body) > customResponseBodyBytes {
		rec.errText = "custom upstream response too large"
		g.fail(w, rec, http.StatusBadGateway, "upstream_unavailable", "upstream_unavailable",
			"自定义模型上游响应过大")
		return
	}

	tap := newBodyTap(g.bodyCap, false)
	tap.feed(body)
	tap.finish()
	rec.prompt, rec.completion, rec.cached = tap.prompt, tap.completion, tap.cached
	if g.debugBody.Load() {
		rec.bodyResp = string(tap.preview)
	}

	out := rewriteModelIfPresent(body, clientModel)
	header := w.Header()
	contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if contentType == "" {
		contentType = "application/json"
	}
	header.Set("Content-Type", contentType)
	header.Set(sourceHeader, rec.source)
	w.WriteHeader(resp.StatusCode)
	rec.ttfb = g.now().Sub(rec.start)
	_, _ = w.Write(out)
}

// copyCustomAsStream serves a client that asked for stream while the upstream
// ignored stream:true. The JSON body becomes one SSE frame plus [DONE], so an
// SSE client still completes instead of parsing JSON as an event.
func (g *Gateway) copyCustomAsStream(w http.ResponseWriter, resp *http.Response, rec *reqRecord, clientModel string) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, customResponseBodyBytes+1))
	if err != nil || len(body) > customResponseBodyBytes {
		rec.errText = "custom upstream read failed"
		g.fail(w, rec, http.StatusBadGateway, "upstream_unavailable", "upstream_unavailable",
			"读取自定义模型上游响应失败")
		return
	}
	tap := newBodyTap(g.bodyCap, false)
	tap.feed(body)
	tap.finish()
	rec.prompt, rec.completion, rec.cached = tap.prompt, tap.completion, tap.cached
	if g.debugBody.Load() {
		rec.bodyResp = string(tap.preview)
	}

	frame := rewriteModelIfPresent(body, clientModel)
	header := w.Header()
	header.Set("Content-Type", "text/event-stream; charset=utf-8")
	header.Set("Cache-Control", "no-cache")
	header.Set(sourceHeader, rec.source)
	w.WriteHeader(resp.StatusCode)
	rec.ttfb = g.now().Sub(rec.start)

	var out bytes.Buffer
	out.WriteString("data: ")
	out.Write(bytes.TrimSpace(frame))
	out.WriteString("\n\ndata: [DONE]\n\n")
	_, _ = w.Write(out.Bytes())
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// copyCustomStream forwards upstream SSE frames line by line, rewriting the
// "model" field of each JSON data frame and preserving [DONE] and any comment
// or event lines byte for byte.
func (g *Gateway) copyCustomStream(w http.ResponseWriter, resp *http.Response, rec *reqRecord, clientModel string) {
	header := w.Header()
	header.Set("Content-Type", "text/event-stream; charset=utf-8")
	header.Set("Cache-Control", "no-cache")
	header.Set(sourceHeader, rec.source)
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}

	tap := newBodyTap(g.bodyCap, true)
	reader := bufio.NewReaderSize(resp.Body, 64*1024)
	ttfbSet := false
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			if !ttfbSet {
				rec.ttfb = g.now().Sub(rec.start)
				ttfbSet = true
			}
			tap.feed(line)
			out := rewriteSSEModelLine(line, clientModel)
			if _, werr := w.Write(out); werr != nil {
				rec.errText = "client write failed"
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				rec.errText = "custom upstream read failed"
			}
			break
		}
	}
	tap.finish()
	rec.prompt, rec.completion, rec.cached = tap.prompt, tap.completion, tap.cached
	if g.debugBody.Load() {
		rec.bodyResp = string(tap.preview)
	}
}

// rewriteSSEModelLine rewrites "model" inside one "data: {...}" line. Every
// other line and the original prefix/newline bytes are preserved; [DONE] and
// non-JSON data stay untouched.
func rewriteSSEModelLine(line []byte, clientModel string) []byte {
	idx := bytes.Index(line, []byte("data:"))
	if idx < 0 || idx > 1 {
		return line
	}
	prefixEnd := idx + len("data:")
	payload := bytes.TrimSpace(line[prefixEnd:])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return line
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(payload, &top); err != nil {
		return line
	}
	raw, ok := top["model"]
	if !ok {
		return line
	}
	var current string
	if json.Unmarshal(raw, &current) != nil || current == clientModel {
		return line
	}
	encoded, err := json.Marshal(clientModel)
	if err != nil {
		return line
	}
	top["model"] = encoded
	body, err := json.Marshal(top)
	if err != nil {
		return line
	}

	suffix := line[len(bytes.TrimRight(line, "\r\n")):]
	var out bytes.Buffer
	out.Write(line[:prefixEnd])
	out.WriteByte(' ')
	out.Write(body)
	out.Write(suffix)
	return out.Bytes()
}

// rewriteModelIfPresent rewrites the top-level model field of a JSON document
// only when it exists; documents without one are returned byte for byte.
func rewriteModelIfPresent(body []byte, clientModel string) []byte {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return body
	}
	raw, ok := top["model"]
	if !ok {
		return body
	}
	var current string
	if json.Unmarshal(raw, &current) == nil && current == clientModel {
		return body
	}
	encoded, err := json.Marshal(clientModel)
	if err != nil {
		return body
	}
	top["model"] = encoded
	out, err := json.Marshal(top)
	if err != nil {
		return body
	}
	return out
}

// handleModelsWithCustom serves GET /v1/models with custom entries merged in.
// The base list is produced by handleModels (plugins, logical bindings, A) and
// then transformed, so the plugin/A fields are unchanged.
func (g *Gateway) handleModelsWithCustom(w http.ResponseWriter, r *http.Request, rec *reqRecord) {
	buf := &bufferedResponse{header: make(http.Header)}
	g.handleModels(buf, r, rec)
	if buf.status != http.StatusOK {
		buf.respond(w, buf.body.Bytes())
		return
	}
	customs := g.customModelList()
	var list modelsList
	if json.Unmarshal(buf.body.Bytes(), &list) != nil {
		buf.respond(w, buf.body.Bytes())
		return
	}
	entries := list.Data
	if len(customs) > 0 {
		entries = mergeCustomModelEntries(entries, customs)
	}
	kr, restricted := keyRecordFrom(r.Context())
	restricted = restricted && len(kr.ModelScope) > 0
	if restricted {
		filtered := make([]modelEntry, 0, len(entries))
		for _, entry := range entries {
			if modelInScope(kr.ModelScope, entry.ID) {
				filtered = append(filtered, entry)
			}
		}
		entries = filtered
	}
	if len(customs) == 0 && !restricted {
		// Nothing changed: keep the byte-for-byte base list.
		buf.respond(w, buf.body.Bytes())
		return
	}
	payload, err := json.Marshal(modelsList{Object: list.Object, Data: entries})
	if err != nil {
		buf.respond(w, buf.body.Bytes())
		return
	}
	if g.debugBody.Load() {
		rec.bodyResp = string(payload)
	}
	buf.respond(w, payload)
}

// mergeCustomModelEntries inserts enabled custom models before A's entries.
// A plugin model or logical binding with the same canonical id wins (its entry
// comes first and the custom one is skipped); a same-id A model is replaced by
// the custom entry, matching the routing priority (custom before A). The
// result is what GET /v1/models publishes; the admin list flags the skipped
// case through CustomModelConflicts.
func mergeCustomModelEntries(existing []modelEntry, customs []CustomModel) []modelEntry {
	hard := make(map[string]bool, len(existing))
	for _, entry := range existing {
		if entry.OwnedBy != "a" {
			hard[CanonicalModelID(entry.ID)] = true
		}
	}

	inserts := make([]modelEntry, 0, len(customs))
	customKeys := make(map[string]bool, len(customs))
	for _, cm := range customs {
		if !cm.Enabled || (cm.ConnectionID != "" && !cm.ConnectionEnabled) {
			continue
		}
		name := strings.TrimSpace(cm.Name)
		key := CanonicalModelID(name)
		if key == "" || hard[key] || customKeys[key] {
			continue
		}
		customKeys[key] = true
		inserts = append(inserts, modelEntry{
			ID:      name,
			Object:  "model",
			OwnedBy: "custom",
			Name:    name,
			Vendor:  "custom",
		})
	}
	if len(inserts) == 0 {
		return existing
	}

	out := make([]modelEntry, 0, len(existing)+len(inserts))
	inserted := false
	for _, entry := range existing {
		if entry.OwnedBy == "a" && customKeys[CanonicalModelID(entry.ID)] {
			if !inserted {
				out = append(out, inserts...)
				inserted = true
			}
			continue
		}
		out = append(out, entry)
	}
	if !inserted {
		out = append(out, inserts...)
	}
	return out
}

// CustomModelConflicts reports which of names collide with a model id already
// served by an enabled plugin or a logical binding (canonical compare). Those
// custom models never reach /v1/models; the admin list shows them as flagged.
// A's own models are not reported: a custom entry intentionally takes priority
// over A's catalogue.
func (g *Gateway) CustomModelConflicts(ctx context.Context, names []string) map[string]bool {
	out := make(map[string]bool)
	if g == nil || len(names) == 0 {
		return out
	}
	wanted := make(map[string]bool, len(names))
	for _, name := range names {
		if key := CanonicalModelID(name); key != "" {
			wanted[key] = true
		}
	}
	if len(wanted) == 0 {
		return out
	}
	if g.plugins != nil {
		for _, st := range g.plugins.List() {
			if !st.Enabled || st.Manifest.ID == "" {
				continue
			}
			models, _ := g.pluginModelList(ctx, st)
			for _, m := range models {
				key := CanonicalModelID(st.Manifest.ID + "/" + m.ID)
				if wanted[key] {
					out[key] = true
				}
			}
		}
	}
	if g.snapshot != nil {
		if snap, err := g.snapshot(ctx); err == nil && snap != nil {
			for name := range snap.Bindings {
				key := CanonicalModelID(name)
				if wanted[key] {
					out[key] = true
				}
			}
		}
	}
	return out
}
