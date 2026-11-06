package gateway

// P6-1: Gemini generateContent inbound. The request is translated into the
// internal OpenAI chat shape, routed through the existing scheduler/plugin/A
// pipeline, and the answer is translated back (JSON or SSE). Only text and
// inline images are mapped today; tool calls are not translated.

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

type geminiInboundPart struct {
	Text       string            `json:"text"`
	InlineData *geminiInlineData `json:"inlineData,omitempty"`
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
	for _, content := range req.Contents {
		role := "user"
		if strings.EqualFold(strings.TrimSpace(content.Role), "model") {
			role = "assistant"
		}
		var textParts []string
		var images []map[string]any
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
		}
		text := strings.Join(textParts, "")
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
			Content string `json:"content"`
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
	if len(answer.Choices) > 0 {
		text = answer.Choices[0].Message.Content
		if answer.Choices[0].FinishReason == "length" {
			finish = "MAX_TOKENS"
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"parts": []any{map[string]any{"text": text}}, "role": "model"},
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
type geminiStreamWriter struct {
	dst          http.ResponseWriter
	status       int
	pending      []byte
	started      bool
	finishReason string
	usage        map[string]any
	model        string
	errorBody    []byte
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
				Content string `json:"content"`
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
	final := map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"parts": []any{map[string]any{"text": ""}}, "role": "model"},
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
