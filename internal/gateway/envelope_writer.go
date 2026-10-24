package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// envelopeWriter adapts the chat/completions reply produced by a plugin into
// the /v1/completions or /v1/responses envelope the client asked for. It is an
// http.ResponseWriter so it can be dropped into the existing forward/scheduling
// path without touching them.

type envelopeWriter struct {
	dst    http.ResponseWriter
	kind   envelopeKind
	model  string
	status int
	header http.Header

	// wantStream is what the client asked for; it only breaks ties when the
	// upstream gives no usable Content-Type.
	wantStream bool
	started    bool
	stream     bool
	pending    []byte
	capped     bool
	body       bytes.Buffer

	respID        string
	msgID         string
	created       int64
	seq           int
	idSeen        bool
	preamble      bool
	msgOpened     bool
	reasoningDone bool
	messageIndex  int
	outputItems   []any
	finished      bool
	text          strings.Builder
	reasoning     strings.Builder
	calls         []*callState
	usage         *wireUsage
	finishReason  string
	// event is the "event:" name of the frame being parsed; failed records
	// that an upstream error frame already ended the stream.
	event  string
	failed bool
}

type callState struct {
	id   string
	name string
	args strings.Builder
}

func newEnvelopeWriter(dst http.ResponseWriter, kind envelopeKind, model string, wantStream bool) *envelopeWriter {
	return &envelopeWriter{
		dst:        dst,
		kind:       kind,
		model:      model,
		status:     http.StatusOK,
		header:     http.Header{},
		wantStream: wantStream,
		respID:     newWireID(envelopeIDPrefix(kind)),
		created:    time.Now().Unix(),
	}
}

func envelopeIDPrefix(kind envelopeKind) string {
	if kind == envelopeResponses {
		return "resp"
	}
	return "cmpl"
}

func (e *envelopeWriter) Header() http.Header { return e.header }

// WriteHeader records the upstream status and decides how to adapt the body.
// Only the first call counts: the gateway calls it before streaming, so the
// header map is already populated by then.
func (e *envelopeWriter) WriteHeader(code int) {
	if e.started {
		return
	}
	e.status = code
	contentType := strings.ToLower(e.header.Get("Content-Type"))
	switch {
	case strings.Contains(contentType, "text/event-stream"):
		e.stream = true
	case strings.Contains(contentType, "application/json"):
		e.stream = false
	default:
		// Upstreams that omit the media type are believed only when the client
		// itself asked for a stream.
		e.stream = e.wantStream
	}
}

// Write passes non-200 replies through untouched (an error is not our business
// to re-encode), adapts SSE as it arrives and buffers everything else.
func (e *envelopeWriter) Write(p []byte) (int, error) {
	if e.status != http.StatusOK {
		e.emitHeaders(e.passthroughContentType())
		return e.dst.Write(p)
	}
	if e.stream {
		return e.writeStream(p)
	}
	if e.capped {
		return len(p), nil
	}
	if e.body.Len()+len(p) > maxEnvelopeBuffer {
		e.capped = true
		return len(p), nil
	}
	return e.body.Write(p)
}

// passthroughContentType keeps the media type the upstream chose for a reply we
// are not converting, so an error body stays byte-for-byte identifiable.
func (e *envelopeWriter) passthroughContentType() string {
	if ct := e.header.Get("Content-Type"); ct != "" {
		return ct
	}
	return "application/json; charset=utf-8"
}

// writeStream splits the upstream SSE stream into whole lines as they arrive.
func (e *envelopeWriter) writeStream(p []byte) (int, error) {
	e.pending = append(e.pending, p...)
	if len(e.pending) > maxEnvelopeBuffer {
		// No line break in 32MB is not a stream this adapter can recover.
		e.pending = e.pending[:0]
		e.capped = true
		return len(p), nil
	}
	for {
		idx := bytes.IndexByte(e.pending, '\n')
		if idx < 0 {
			break
		}
		line := append([]byte(nil), e.pending[:idx]...)
		e.pending = e.pending[idx+1:]
		e.handleLine(line)
	}
	return len(p), nil
}

func (e *envelopeWriter) Flush() {
	if f, ok := e.dst.(http.Flusher); ok && e.started {
		f.Flush()
	}
}

func (e *envelopeWriter) emitHeaders(contentType string) {
	if e.started {
		return
	}
	dst := e.dst.Header()
	for key, values := range e.header {
		if strings.EqualFold(key, "Content-Type") || strings.EqualFold(key, "Content-Length") {
			continue
		}
		for _, v := range values {
			dst.Add(key, v)
		}
	}
	dst.Set("Content-Type", contentType)
	e.started = true
	e.dst.WriteHeader(e.status)
}

// finish converts whatever was collected into the final envelope. Safe to call
// twice; the gateway handler defers it after the upstream stream ends.
func (e *envelopeWriter) finish() {
	if e.finished {
		return
	}
	e.finished = true
	if e.status != http.StatusOK {
		// A pass-through reply still needs its headers flushed.
		e.emitHeaders(e.passthroughContentType())
		return
	}
	if e.stream {
		e.flushPending()
		if e.failed {
			// An upstream error frame already terminated the stream; a [DONE]
			// after an error would look like a successful finish.
			return
		}
		if e.kind == envelopeResponses {
			e.finishResponses()
		} else {
			e.finishCompletions()
		}
		// The upstream terminator is dropped in handleLine; this one is ours.
		e.emitRaw("data: [DONE]\n\n")
		return
	}
	e.bufferedReply()
}

// ------------------------------------------------------------------ buffered

func (e *envelopeWriter) bufferedReply() {
	if e.capped {
		e.status = http.StatusBadGateway
		e.emitJSON([]byte(`{"error":{"message":"upstream reply too large to convert","type":"upstream_error","code":"response_too_large"}}`))
		return
	}
	raw := bytes.TrimSpace(e.body.Bytes())
	if len(raw) == 0 {
		e.status = http.StatusBadGateway
		e.emitJSON([]byte(`{"error":{"message":"empty upstream reply","type":"upstream_error","code":"empty_response"}}`))
		return
	}
	var reply chatReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		e.emitUnconverted(raw)
		return
	}
	if len(reply.Error) > 0 && string(reply.Error) != "null" {
		e.emitJSON([]byte(`{"error":` + string(reply.Error) + `}`))
		return
	}
	if reply.ID != "" && e.kind != envelopeResponses {
		// /v1/completions keeps the upstream chat id. /v1/responses must use
		// its own resp_ id: the upstream only ever speaks chatcmpl-…, and a
		// Responses client treats the id as an opaque response handle.
		e.respID = reply.ID
	}
	if e.kind == envelopeResponses {
		// responseBody prefers reply.ID when set; give it the gateway-owned id
		// so the body matches the stream preamble and the upstream chat id
		// never leaks.
		reply.ID = e.respID
	}
	body := completionBody(reply, e.model, e.created)
	if e.kind == envelopeResponses {
		body = responseBody(reply, e.model, e.created)
	}
	data, err := json.Marshal(body)
	if err != nil {
		e.emitUnconverted(raw)
		return
	}
	e.emitJSON(data)
}

func (e *envelopeWriter) emitJSON(data []byte) {
	e.emitHeaders("application/json; charset=utf-8")
	e.writeBody(data)
}

// emitUnconverted relays bytes the translation could not read, keeping the
// media type the upstream chose so the client sees what the plugin really sent.
func (e *envelopeWriter) emitUnconverted(data []byte) {
	e.emitHeaders(e.passthroughContentType())
	e.writeBody(data)
}

func (e *envelopeWriter) writeBody(data []byte) {
	_, _ = e.dst.Write(data)
	if f, ok := e.dst.(http.Flusher); ok {
		f.Flush()
	}
}

// ------------------------------------------------------------------ streaming

func (e *envelopeWriter) flushPending() {
	if len(bytes.TrimSpace(e.pending)) == 0 {
		return
	}
	e.handleLine(e.pending)
	e.pending = e.pending[:0]
}

func (e *envelopeWriter) handleLine(raw []byte) {
	line := strings.TrimRight(string(raw), "\r")
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		e.event = ""
		return
	}
	if strings.HasPrefix(trimmed, ":") || strings.HasPrefix(trimmed, "id:") ||
		strings.HasPrefix(trimmed, "retry:") {
		return
	}
	if strings.HasPrefix(trimmed, "event:") {
		e.event = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
		return
	}
	if !strings.HasPrefix(trimmed, "data:") {
		trimmed = "data: " + trimmed
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	event := e.event
	e.event = ""
	if payload == "[DONE]" || e.failed {
		return
	}
	var chunk chatChunk
	if event == "error" {
		// Some upstreams send a non-JSON payload after "event: error"; the frame
		// still ends the stream as a failure.
		_ = json.Unmarshal([]byte(payload), &chunk)
		e.failStream(chunk, payload)
		return
	}
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return
	}
	// A mid-stream failure must end the stream as a failure, not as a fake
	// successful finish. Ported from A (TraeWorkAssistant, MIT, 朱天伟)
	// sse.rs:1055-1077, wb_sse.rs:87.
	if chunk.Error != nil {
		e.failStream(chunk, payload)
		return
	}
	// The upstream echoes the same chat id on every frame; the first one wins.
	// /v1/responses keeps its own resp_ id instead (see bufferedReply).
	if chunk.ID != "" && !e.idSeen && e.kind != envelopeResponses {
		e.idSeen = true
		e.respID = chunk.ID
	}
	if chunk.Created != 0 {
		e.created = chunk.Created
	}
	if chunk.Usage != nil {
		e.usage = chunk.Usage
	}
	if len(chunk.Choices) == 0 {
		return
	}
	delta := chunk.Choices[0].Delta
	if delta.ReasoningContent != "" {
		e.reasoning.WriteString(delta.ReasoningContent)
	}
	if delta.Content != "" {
		e.text.WriteString(delta.Content)
		if e.kind == envelopeResponses {
			e.openMessage()
			e.emitResponseDelta(delta.Content)
		} else {
			e.emitCompletionDelta(delta.Content)
		}
	}
	for _, call := range delta.ToolCalls {
		e.mergeCall(call)
	}
	if reason := finishReasonFromRaw(chunk.Choices[0].FinishReason); reason != "" {
		e.finishReason = reason
	}
}

// failStream ends the converted stream with the protocol-correct failure: a
// response.failed event for Responses (code upstream_error) or a plain
// {"error":{...}} frame for completions, and no [DONE] afterwards.
// Ported from A (TraeWorkAssistant, MIT, 朱天伟) sse.rs:1055-1077.
func (e *envelopeWriter) failStream(chunk chatChunk, payload string) {
	if e.failed {
		return
	}
	e.failed = true
	typ, code, msg := streamErrorFromFrame(chunk, payload)
	if msg == "" {
		msg = "upstream stream error"
	}
	if e.kind == envelopeResponses {
		e.openPreamble()
		e.emitEvent("response.failed", map[string]any{
			"type": "response.failed", "sequence_number": e.next(),
			"response": map[string]any{
				"id": e.respID, "object": "response", "created_at": e.created,
				"status": "failed", "model": e.model, "output": e.itemsSoFar(),
				"error": map[string]any{
					"code":    "upstream_error",
					"message": msg,
				},
			},
		})
		return
	}
	e.emitEvent("", map[string]any{"error": map[string]any{
		"message": msg,
		"type":    firstNonEmpty(typ, "upstream_error"),
		"code":    firstNonEmpty(code, "upstream_error"),
	}})
}

// mergeCall folds one tool-call fragment into its slot. Providers that omit
// index (or restart numbering) are handled by treating a fragment that carries
// an id or a name as the start of a new call.
func (e *envelopeWriter) mergeCall(call toolDelta) {
	index := -1
	if call.Index != nil {
		index = *call.Index
	} else if call.ID != "" || call.Function.Name != "" {
		index = len(e.calls)
	} else if len(e.calls) > 0 {
		index = len(e.calls) - 1
	} else {
		index = 0
	}
	if index < 0 {
		index = 0
	}
	for len(e.calls) <= index {
		e.calls = append(e.calls, &callState{})
	}
	state := e.calls[index]
	if call.ID != "" {
		state.id = call.ID
	}
	if call.Function.Name != "" {
		state.name = call.Function.Name
	}
	state.args.WriteString(call.Function.Arguments)
}

// openPreamble emits the Responses start events exactly once.
func (e *envelopeWriter) openPreamble() {
	if e.preamble || e.kind != envelopeResponses {
		return
	}
	e.preamble = true
	e.msgID = e.respID + "_msg"
	inProgress := map[string]any{
		"id": e.respID, "object": "response", "created_at": e.created,
		"status": "in_progress", "model": e.model, "output": e.itemsSoFar(),
	}
	e.emitEvent("response.created", map[string]any{
		"type": "response.created", "sequence_number": e.next(), "response": inProgress,
	})
	e.emitEvent("response.in_progress", map[string]any{
		"type": "response.in_progress", "sequence_number": e.next(), "response": inProgress,
	})
}

// openMessage announces the assistant message item before its first text delta.
func (e *envelopeWriter) openMessage() {
	if e.msgOpened || e.kind != envelopeResponses {
		return
	}
	e.openPreamble()
	e.flushReasoning()
	e.msgOpened = true
	item := map[string]any{"type": "message", "id": e.msgID, "role": "assistant",
		"status": "in_progress", "content": []any{}}
	e.messageIndex = e.addItem(item)
	e.emitEvent("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "sequence_number": e.next(),
		"output_index": e.messageIndex, "item": item,
	})
	e.emitEvent("response.content_part.added", map[string]any{
		"type": "response.content_part.added", "sequence_number": e.next(),
		"item_id": e.msgID, "output_index": e.messageIndex, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})
}

// addItem appends an output item and returns its output_index. Keeping the
// slice as the only index source prevents the events and the final response
// object from disagreeing about item order.
func (e *envelopeWriter) addItem(item map[string]any) int {
	e.outputItems = append(e.outputItems, item)
	return len(e.outputItems) - 1
}

// flushReasoning emits the reasoning item once, ahead of whatever follows it.
func (e *envelopeWriter) flushReasoning() {
	if e.reasoningDone || e.reasoning.Len() == 0 {
		return
	}
	e.reasoningDone = true
	e.openPreamble()
	item := e.reasoningItem()
	index := e.addItem(item)
	e.emitEvent("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "sequence_number": e.next(),
		"output_index": index, "item": item,
	})
	e.emitEvent("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "sequence_number": e.next(),
		"output_index": index, "item": item,
	})
}

func (e *envelopeWriter) reasoningItem() map[string]any {
	return map[string]any{"type": "reasoning", "id": newWireID("rs"),
		"summary": []any{map[string]any{"type": "summary_text", "text": e.reasoning.String()}}}
}

func (e *envelopeWriter) messageItem() map[string]any {
	return map[string]any{"type": "message", "id": e.msgID, "role": "assistant", "status": "completed",
		"content": []any{map[string]any{
			"type": "output_text", "text": e.text.String(), "annotations": []any{}}}}
}

func (e *envelopeWriter) itemsSoFar() []any {
	if len(e.outputItems) == 0 {
		return []any{}
	}
	return append([]any{}, e.outputItems...)
}

func (e *envelopeWriter) emitCompletionDelta(text string) {
	chunk := map[string]any{
		"id": e.respID, "object": "text_completion", "created": e.created, "model": e.model,
		"choices": []any{map[string]any{"index": 0, "text": text, "logprobs": nil, "finish_reason": nil}},
	}
	e.emitEvent("", chunk)
}

func (e *envelopeWriter) emitResponseDelta(text string) {
	e.emitEvent("response.output_text.delta", map[string]any{
		"type": "response.output_text.delta", "sequence_number": e.next(),
		"item_id": e.msgID, "output_index": e.messageIndex, "content_index": 0, "delta": text,
	})
}

// hasOutput reports whether the upstream produced anything the client can use.
// An empty stream is an upstream failure, not a successful blank answer.
func (e *envelopeWriter) hasOutput() bool {
	return e.text.Len() > 0 || e.reasoning.Len() > 0 || len(e.calls) > 0
}

func (e *envelopeWriter) finishCompletions() {
	if !e.hasOutput() {
		e.emitEvent("", map[string]any{"error": map[string]any{
			"message": "upstream stream ended without content",
			"type":    "upstream_error",
			"code":    "empty_response",
		}})
		return
	}
	finish := e.finishReason
	if finish == "" {
		finish = "stop"
	}
	chunk := map[string]any{
		"id": e.respID, "object": "text_completion", "created": e.created, "model": e.model,
		"choices": []any{map[string]any{"index": 0, "text": "", "logprobs": nil, "finish_reason": finish}},
	}
	if usage := e.usage.completionValue(); usage != nil {
		chunk["usage"] = usage
	}
	e.emitEvent("", chunk)
}

func (e *envelopeWriter) finishResponses() {
	// A stream that carried only tool calls, only reasoning, or nothing at all
	// still has to open with response.created.
	e.openPreamble()
	if !e.hasOutput() {
		e.emitEvent("response.failed", map[string]any{
			"type": "response.failed", "sequence_number": e.next(),
			"response": map[string]any{
				"id": e.respID, "object": "response", "created_at": e.created,
				"status": "failed", "model": e.model, "output": e.itemsSoFar(),
				"error": map[string]any{
					"code":    "empty_response",
					"message": "upstream stream ended without content",
				},
			},
		})
		return
	}
	if e.msgOpened {
		e.emitEvent("response.output_text.done", map[string]any{
			"type": "response.output_text.done", "sequence_number": e.next(),
			"item_id": e.msgID, "output_index": e.messageIndex, "content_index": 0,
			"text": e.text.String(),
		})
		e.emitEvent("response.content_part.done", map[string]any{
			"type": "response.content_part.done", "sequence_number": e.next(),
			"item_id": e.msgID, "output_index": e.messageIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": e.text.String(), "annotations": []any{}},
		})
		completed := e.messageItem()
		e.outputItems[e.messageIndex] = completed
		e.emitEvent("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "sequence_number": e.next(),
			"output_index": e.messageIndex, "item": completed,
		})
	}
	e.flushReasoning()

	for _, call := range e.calls {
		if call == nil || strings.TrimSpace(call.name) == "" {
			continue
		}
		id := firstNonEmpty(call.id, newWireID("fc"))
		added := map[string]any{"type": "function_call", "id": id,
			"call_id": call.id, "name": call.name, "arguments": "", "status": "in_progress"}
		item := map[string]any{"type": "function_call", "id": id, "call_id": call.id,
			"name": call.name, "arguments": call.args.String(), "status": "completed"}
		index := e.addItem(item)
		e.emitEvent("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "sequence_number": e.next(),
			"output_index": index, "item": added,
		})
		if call.args.Len() > 0 {
			e.emitEvent("response.function_call_arguments.delta", map[string]any{
				"type": "response.function_call_arguments.delta", "sequence_number": e.next(),
				"item_id": id, "output_index": index, "delta": call.args.String(),
			})
			e.emitEvent("response.function_call_arguments.done", map[string]any{
				"type": "response.function_call_arguments.done", "sequence_number": e.next(),
				"item_id": id, "output_index": index, "arguments": call.args.String(),
			})
		}
		e.emitEvent("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "sequence_number": e.next(),
			"output_index": index, "item": item,
		})
	}

	status, incomplete := responseStatus(e.finishReason)
	final := map[string]any{
		"id": e.respID, "object": "response", "created_at": e.created,
		"status": status, "model": e.model,
		"output": e.itemsSoFar(), "output_text": e.text.String(),
	}
	if incomplete != nil {
		final["incomplete_details"] = incomplete
	}
	if usage := e.usage.responsesValue(); usage != nil {
		final["usage"] = usage
	}
	// A truncated answer is "incomplete", not "completed". Ported from A
	// (TraeWorkAssistant, MIT, 朱天伟) wb_responses.rs:519.
	event := "response.completed"
	if status == "incomplete" {
		event = "response.incomplete"
	}
	e.emitEvent(event, map[string]any{
		"type": event, "sequence_number": e.next(), "response": final,
	})
}

func (e *envelopeWriter) next() int {
	e.seq++
	return e.seq
}

func (e *envelopeWriter) emitEvent(event string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	if event == "" {
		e.emitRaw("data: " + string(data) + "\n\n")
		return
	}
	e.emitRaw("event: " + event + "\ndata: " + string(data) + "\n\n")
}

func (e *envelopeWriter) emitRaw(s string) {
	e.emitHeaders("text/event-stream")
	_, _ = e.dst.Write([]byte(s))
	if f, ok := e.dst.(http.Flusher); ok {
		f.Flush()
	}
}

func finishReasonFromRaw(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return ""
}
