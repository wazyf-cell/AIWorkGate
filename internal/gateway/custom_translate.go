package gateway

// Adapts a native Anthropic/Gemini reply into the chat-shaped response the
// rest of the custom-model pipeline already understands. Non-stream replies are
// converted in memory; SSE replies are converted through a pipe so the client
// still receives frames as they arrive.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

func translateCustomResponse(protocol string, resp *http.Response, clientModel string) (*http.Response, error) {
	upstreamSSE := strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
	if upstreamSSE {
		pr, pw := io.Pipe()
		out := *resp
		out.Body = pr
		out.Header = resp.Header.Clone()
		out.Header.Set("Content-Type", "text/event-stream; charset=utf-8")
		out.ContentLength = -1
		go func() {
			defer resp.Body.Close()
			var err error
			switch protocol {
			case "anthropic":
				err = anthropicStreamToChat(resp.Body, pw, clientModel)
			case "gemini":
				err = geminiStreamToChat(resp.Body, pw, clientModel)
			}
			if err != nil {
				writeTranslatedStreamError(pw, err)
			}
			_ = pw.Close()
		}()
		return &out, nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, customResponseBodyBytes+1))
	_ = resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("读取上游响应失败")
	}
	if len(body) > customResponseBodyBytes {
		return nil, fmt.Errorf("上游响应过大")
	}
	var converted []byte
	switch protocol {
	case "anthropic":
		converted, err = anthropicResponseToChat(body, clientModel)
	case "gemini":
		converted, err = geminiResponseToChat(body, clientModel)
	default:
		converted = body
	}
	if err != nil {
		return nil, err
	}
	out := *resp
	out.Body = io.NopCloser(bytes.NewReader(converted))
	out.Header = resp.Header.Clone()
	out.Header.Set("Content-Type", "application/json; charset=utf-8")
	out.ContentLength = int64(len(converted))
	return &out, nil
}

// writeTranslatedStreamError ends a stream with an error frame instead of a
// silent EOF, so an SSE client can explain the failure.
func writeTranslatedStreamError(dst io.Writer, err error) {
	payload, marshalErr := json.Marshal(map[string]any{
		"error": map[string]string{
			"message": "转换上游响应失败：" + err.Error(),
			"type":    "upstream_error",
		},
	})
	if marshalErr != nil {
		return
	}
	_, _ = dst.Write([]byte("data: " + string(payload) + "\n\ndata: [DONE]\n\n"))
}
