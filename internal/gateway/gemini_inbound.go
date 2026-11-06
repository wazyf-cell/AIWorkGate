package gateway

// P6-1: Gemini generateContent inbound. The request is translated into the
// internal OpenAI chat shape, routed through the existing scheduler/plugin
// pipeline, and the answer is translated back (JSON or SSE). Text, inline
// images and function calls are mapped in both directions.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type geminiInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type geminiInboundFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type geminiInboundFunctionResponse struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response,omitempty"`
}

type geminiInboundPart struct {
	Text             string                         `json:"text"`
	InlineData       *geminiInlineData              `json:"inlineData,omitempty"`
	FunctionCall     *geminiInboundFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiInboundFunctionResponse `json:"functionResponse,omitempty"`
}

type geminiFunctionDeclaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type geminiTool struct {
	FunctionDeclarations []geminiFunctionDeclaration `json:"functionDeclarations,omitempty"`
}

type geminiInboundContent struct {
	Role  string              `json:"role"`
	Parts []geminiInboundPart `json:"parts"`
}

type geminiInboundRequest struct {
	Contents          []geminiInboundContent `json:"contents"`
	SystemInstruction *struct {
		Parts []geminiInboundPart `json:"parts"`
	} `json:"systemInstruction"`
	GenerationConfig *struct {
		Temperature     *float64 `json:"temperature"`
		TopP            *float64 `json:"topP"`
		MaxOutputTokens *int     `json:"maxOutputTokens"`
	} `json:"generationConfig"`
	Tools []geminiTool `json:"tools"`
}

// geminiAction splits /v1beta/models/<model>:generateContent style paths.
func geminiAction(path string) (model string, stream bool, ok bool) {
	rest := ""
	for _, prefix := range []string{"/v1beta/models/", "/v1/models/"} {
		if strings.HasPrefix(path, prefix) {
			rest = strings.TrimPrefix(path, prefix)
			break
		}
	}
	if rest == "" {
		return "", false, false
	}
	idx := strings.LastIndex(rest, ":")
	if idx <= 0 {
		return "", false, false
	}
	model = strings.TrimPrefix(strings.TrimSpace(rest[:idx]), "models/")
	switch rest[idx+1:] {
	case "generateContent":
		return model, false, true
	case "streamGenerateContent":
		return model, true, true
	}
	return "", false, false
}

func isGeminiPath(path string) bool {
	_, _, ok := geminiAction(path)
	return ok
}

func geminiToChatBody(req geminiInboundRequest, model string, stream bool) ([]byte, error) {
	messages := make([]map[string]any, 0, len(req.Contents)+1)
	if req.SystemInstruction != nil {
		if text := joinGeminiParts(req.SystemInstruction.Parts); text != "" {
			messages = append(messages, map[string]any{"role": "system", "content": text})
		}
	}
	// Gemini has no call ids; pair each functionResponse with the oldest call of
	// the same name so the OpenAI tool message carries a matching id.
	callIDs := map[string][]string{}
	callSeq := 0
	nextCallID := func(name string) string {
		callSeq++
		id := fmt.Sprintf("call_%s_%d", sanitizeCallName(name), callSeq)
		callIDs[name] = append(callIDs[name], id)
		return id
	}
	takeCallID := func(name string) string {
		if ids := callIDs[name]; len(ids) > 0 {
			callIDs[name] = ids[1:]
			return ids[0]
		}
		callSeq++
		return fmt.Sprintf("call_%s_%d", sanitizeCallName(name), callSeq)
	}

	for _, content := range req.Contents {
		role := "user"
		if strings.EqualFold(strings.TrimSpace(content.Role), "model") {
			role = "assistant"
		}
		var (
			textParts []string
			images    []map[string]any
			toolCalls []map[string]any
			toolMsgs  []map[string]any
		)
		for _, part := range content.Parts {
			if part.Text != "" {
				textParts = append(textParts, part.Text)
			}
			if part.InlineData != nil && part.InlineData.Data != "" {
				images = append(images, map[string]any{
					"type": "image_url",
					"image_url": map[string]any{
						"url": "data:" + part.InlineData.MimeType + ";base64," + part.InlineData.Data,
					},
				})
			}
			if part.FunctionCall != nil {
				name := strings.TrimSpace(part.FunctionCall.Name)
				if name == "" {
					continue
				}
				toolCalls = append(toolCalls, map[string]any{
					"id":   nextCallID(name),
					"type": "function",
					"function": map[string]any{
						"name":      name,
						"arguments": geminiArguments(part.FunctionCall.Args),
					},
				})
			}
			if part.FunctionResponse != nil {
				name := strings.TrimSpace(part.FunctionResponse.Name)
				if name == "" {
					continue
				}
				toolMsgs = append(toolMsgs, map[string]any{
					"role":         "tool",
					"tool_call_id": takeCallID(name),
					"content":      geminiArguments(part.FunctionResponse.Response),
				})
			}
		}
		text := strings.Join(textParts, "")
		if role == "assistant" {
			message := map[string]any{"role": role, "content": text}
			if len(toolCalls) > 0 {
				message["tool_calls"] = toolCalls
			}
			messages = append(messages, message)
			continue
		}
		// Tool results must precede any text/image part of the same user turn.
		messages = append(messages, toolMsgs...)
		if text == "" && len(images) == 0 {
			continue
		}
		if len(images) == 0 {
			messages = append(messages, map[string]any{"role": role, "content": text})
			continue
		}
		parts := make([]map[string]any, 0, len(images)+1)
		if text != "" {
			parts = append(parts, map[string]any{"type": "text", "text": text})
		}
		parts = append(parts, images...)
		messages = append(messages, map[string]any{"role": role, "content": parts})
	}
	if len(messages) == 0 || (len(messages) == 1 && messages[0]["role"] == "system") {
		return nil, fmt.Errorf("contents contain no user or model turns")
	}
	body := map[string]any{"model": model, "messages": messages}
	if tools := geminiToolsToChat(req.Tools); len(tools) > 0 {
		body["tools"] = tools
	}
	if stream {
		body["stream"] = true
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	if cfg := req.GenerationConfig; cfg != nil {
		if cfg.Temperature != nil {
			body["temperature"] = *cfg.Temperature
		}
		if cfg.TopP != nil {
			body["top_p"] = *cfg.TopP
		}
		if cfg.MaxOutputTokens != nil && *cfg.MaxOutputTokens > 0 {
			body["max_tokens"] = *cfg.MaxOutputTokens
		}
	}
	return json.Marshal(body)
}

// geminiArguments turns a Gemini args/response object into the JSON string
// OpenAI expects, defaulting to an empty object.
func geminiArguments(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "{}"
	}
	return trimmed
}

func sanitizeCallName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "fn"
	}
	return b.String()
}

// geminiToolsToChat maps functionDeclarations to OpenAI function tools.
func geminiToolsToChat(tools []geminiTool) []map[string]any {
	var out []map[string]any
	for _, tool := range tools {
		for _, decl := range tool.FunctionDeclarations {
			name := strings.TrimSpace(decl.Name)
			if name == "" {
				continue
			}
			fn := map[string]any{"name": name}
			if desc := strings.TrimSpace(decl.Description); desc != "" {
				fn["description"] = desc
			}
			if len(decl.Parameters) > 0 {
				var params any
				if json.Unmarshal(decl.Parameters, &params) == nil && params != nil {
					fn["parameters"] = params
				}
			}
			out = append(out, map[string]any{"type": "function", "function": fn})
		}
	}
	return out
}
func joinGeminiParts(parts []geminiInboundPart) string {
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part.Text != "" {
			out = append(out, part.Text)
		}
	}
	return strings.Join(out, "")
}

// handleGeminiGenerate serves POST /v1beta/models/{model}:generateContent and
// its streaming variant.
func (g *Gateway) handleGeminiGenerate(w http.ResponseWriter, r *http.Request, rec *reqRecord, model string, stream bool) {
	body, failStatus := readRequestBody(w, r)
	if failStatus != 0 {
		rec.status = failStatus
		return
	}
	var req geminiInboundRequest
	if err := json.Unmarshal(body, &req); err != nil {
		g.failGemini(w, rec, http.StatusBadRequest, "请求体不是合法的 JSON")
		return
	}
	chatBody, err := geminiToChatBody(req, model, stream)
	if err != nil {
		g.failGemini(w, rec, http.StatusBadRequest, err.Error())
		return
	}

	sub := r.Clone(r.Context())
	sub.Method = http.MethodPost
	sub.URL.Path = "/v1/chat/completions"
	sub.RequestURI = "/v1/chat/completions"
	sub.Body = io.NopCloser(bytes.NewReader(chatBody))
	sub.ContentLength = int64(len(chatBody))
	sub.Header = r.Header.Clone()
	sub.Header.Set("Content-Type", "application/json")

	if stream {
		writer := newGeminiStreamWriter(w)
		g.handleChat(writer, sub, rec)
		writer.finish()
		return
	}
	capture := &captureWriter{header: make(http.Header)}
	g.handleChat(capture, sub, rec)
	writeGeminiJSON(w, capture)
}

func (g *Gateway) failGemini(w http.ResponseWriter, rec *reqRecord, status int, message string) {
	rec.status = status
	rec.errText = message
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"code": status, "message": message, "status": geminiStatus(status)},
	})
}

func geminiStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "INVALID_ARGUMENT"
	case http.StatusUnauthorized:
		return "UNAUTHENTICATED"
	case http.StatusForbidden:
		return "PERMISSION_DENIED"
	case http.StatusNotFound:
		return "NOT_FOUND"
	case http.StatusTooManyRequests:
		return "RESOURCE_EXHAUSTED"
	case http.StatusServiceUnavailable:
		return "UNAVAILABLE"
	}
	return "INTERNAL"
}

// captureWriter buffers a non-streaming chat response for translation.
type captureWriter struct {
	header http.Header
	status int
	buf    bytes.Buffer
}

func (c *captureWriter) Header() http.Header { return c.header }
func (c *captureWriter) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
	}
}
func (c *captureWriter) Write(p []byte) (int, error) { return c.buf.Write(p) }

type openAIChatAnswer struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content   string         `json:"content"`
			ToolCalls []wireToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

func writeGeminiJSON(w http.ResponseWriter, capture *captureWriter) {
	if capture.status == 0 {
		capture.status = http.StatusOK
	}
	if capture.status != http.StatusOK {
		var answer openAIChatAnswer
		_ = json.Unmarshal(capture.buf.Bytes(), &answer)
		msg := answer.Error.Message
		if msg == "" {
			msg = strings.TrimSpace(capture.buf.String())
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(capture.status)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"code": capture.status, "message": msg, "status": geminiStatus(capture.status)},
		})
		return
	}
	var answer openAIChatAnswer
	if err := json.Unmarshal(capture.buf.Bytes(), &answer); err != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"code": http.StatusBadGateway, "message": "上游返回了无法解析的响应", "status": "INTERNAL"},
		})
		return
	}
	text := ""
	finish := "STOP"
	var parts []any
	if len(answer.Choices) > 0 {
		text = answer.Choices[0].Message.Content
		if answer.Choices[0].FinishReason == "length" {
			finish = "MAX_TOKENS"
		}
		for _, call := range answer.Choices[0].Message.ToolCalls {
			name := strings.TrimSpace(call.Function.Name)
			if name == "" {
				continue
			}
			parts = append(parts, map[string]any{
				"functionCall": map[string]any{
					"name": name,
					"args": geminiArgsObject(call.Function.Arguments),
				},
			})
		}
	}
	if text != "" || len(parts) == 0 {
		parts = append([]any{map[string]any{"text": text}}, parts...)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"parts": parts, "role": "model"},
			"finishReason": finish,
			"index":        0,
		}},
		"usageMetadata": map[string]any{
			"promptTokenCount":     answer.Usage.PromptTokens,
			"candidatesTokenCount": answer.Usage.CompletionTokens,
			"totalTokenCount":      answer.Usage.TotalTokens,
		},
		"modelVersion": answer.Model,
	})
}

// geminiStreamWriter translates OpenAI SSE frames into Gemini SSE frames.
// Content deltas are emitted as they arrive; a final frame carries the finish
// reason and usage. Upstream error bodies are buffered and converted.
type geminiStreamToolCall struct {
	id   string
	name string
	args strings.Builder
}

type geminiStreamWriter struct {
	dst          http.ResponseWriter
	status       int
	pending      []byte
	started      bool
	finishReason string
	usage        map[string]any
	model        string
	errorBody    []byte
	toolCalls    map[int]*geminiStreamToolCall
	toolOrder    []int
}

func newGeminiStreamWriter(dst http.ResponseWriter) *geminiStreamWriter {
	return &geminiStreamWriter{dst: dst}
}

func (s *geminiStreamWriter) Header() http.Header { return s.dst.Header() }

func (s *geminiStreamWriter) WriteHeader(status int) {
	if s.status != 0 {
		return
	}
	s.status = status
	if status == http.StatusOK {
		s.start()
	}
}

func (s *geminiStreamWriter) start() {
	if s.started {
		return
	}
	s.started = true
	h := s.dst.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	s.dst.WriteHeader(http.StatusOK)
}

func (s *geminiStreamWriter) Write(p []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
		s.start()
	}
	if s.status != http.StatusOK {
		s.errorBody = append(s.errorBody, p...)
		return len(p), nil
	}
	s.pending = append(s.pending, p...)
	for {
		idx := bytes.Index(s.pending, []byte("\n\n"))
		if idx < 0 {
			break
		}
		frame := s.pending[:idx]
		s.pending = s.pending[idx+2:]
		s.frame(frame)
	}
	return len(p), nil
}

func (s *geminiStreamWriter) frame(frame []byte) {
	line := string(bytes.TrimSpace(frame))
	if !strings.HasPrefix(line, "data:") {
		return
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "" || payload == "[DONE]" {
		return
	}
	var chunk openAIChatAnswer
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return
	}
	if chunk.Model != "" {
		s.model = chunk.Model
	}
	if chunk.Usage.TotalTokens > 0 || chunk.Usage.PromptTokens > 0 || chunk.Usage.CompletionTokens > 0 {
		s.usage = map[string]any{
			"promptTokenCount":     chunk.Usage.PromptTokens,
			"candidatesTokenCount": chunk.Usage.CompletionTokens,
			"totalTokenCount":      chunk.Usage.TotalTokens,
		}
	}
	for _, choice := range chunk.Choices {
		if choice.FinishReason != "" {
			s.finishReason = choice.FinishReason
		}
	}
	// The chat pipeline's chunks share the answer's choices.delta shape; parse
	// the delta independently because openAIChatAnswer models the non-stream form.
	var delta struct {
		Choices []struct {
			Delta struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(payload), &delta); err != nil {
		return
	}
	if delta.Usage.TotalTokens > 0 {
		s.usage = map[string]any{
			"promptTokenCount":     delta.Usage.PromptTokens,
			"candidatesTokenCount": delta.Usage.CompletionTokens,
			"totalTokenCount":      delta.Usage.TotalTokens,
		}
	}
	for _, choice := range delta.Choices {
		if choice.FinishReason != "" {
			s.finishReason = choice.FinishReason
		}
		for _, call := range choice.Delta.ToolCalls {
			if s.toolCalls == nil {
				s.toolCalls = make(map[int]*geminiStreamToolCall)
			}
			acc, ok := s.toolCalls[call.Index]
			if !ok {
				acc = &geminiStreamToolCall{}
				s.toolCalls[call.Index] = acc
				s.toolOrder = append(s.toolOrder, call.Index)
			}
			if call.ID != "" {
				acc.id = call.ID
			}
			if call.Function.Name != "" {
				acc.name = call.Function.Name
			}
			acc.args.WriteString(call.Function.Arguments)
		}
		if choice.Delta.Content == "" {
			continue
		}
		writeGeminiFrame(s.dst, map[string]any{
			"candidates": []any{map[string]any{
				"content": map[string]any{
					"parts": []any{map[string]any{"text": choice.Delta.Content}},
					"role":  "model",
				},
				"index": 0,
			}},
			"modelVersion": s.model,
		})
		if flusher, ok := s.dst.(http.Flusher); ok {
			flusher.Flush()
		}
	}
}

func writeGeminiFrame(dst http.ResponseWriter, payload map[string]any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = dst.Write([]byte("data: "))
	_, _ = dst.Write(raw)
	_, _ = dst.Write([]byte("\n\n"))
}

func (s *geminiStreamWriter) finish() {
	if !s.started {
		status := s.status
		if status == 0 {
			status = http.StatusOK
		}
		if status != http.StatusOK {
			var answer openAIChatAnswer
			_ = json.Unmarshal(s.errorBody, &answer)
			msg := answer.Error.Message
			if msg == "" {
				msg = strings.TrimSpace(string(s.errorBody))
			}
			s.dst.Header().Set("Content-Type", "application/json; charset=utf-8")
			s.dst.WriteHeader(status)
			_ = json.NewEncoder(s.dst).Encode(map[string]any{
				"error": map[string]any{"code": status, "message": msg, "status": geminiStatus(status)},
			})
			return
		}
		s.start()
	}
	var parts []any
	for _, index := range s.toolOrder {
		acc := s.toolCalls[index]
		if acc == nil || strings.TrimSpace(acc.name) == "" {
			continue
		}
		parts = append(parts, map[string]any{
			"functionCall": map[string]any{
				"name": acc.name,
				"args": geminiArgsObject(acc.args.String()),
			},
		})
	}
	if len(parts) == 0 {
		parts = []any{map[string]any{"text": ""}}
	}
	final := map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"parts": parts, "role": "model"},
			"finishReason": geminiInboundFinishReason(s.finishReason),
			"index":        0,
		}},
		"modelVersion": s.model,
	}
	if s.usage != nil {
		final["usageMetadata"] = s.usage
	}
	writeGeminiFrame(s.dst, final)
}

func geminiInboundFinishReason(raw string) string {
	switch raw {
	case "length":
		return "MAX_TOKENS"
	case "content_filter":
		return "SAFETY"
	case "tool_calls", "function_call":
		return "STOP"
	}
	if raw == "" {
		return "STOP"
	}
	return "STOP"
}
