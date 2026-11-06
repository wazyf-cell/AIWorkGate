package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// unknownSystemField reports whether an upstream rejection complains about the
// top-level "system" field rather than about the model or the messages.
func unknownSystemField(raw string) bool {
	lower := strings.ToLower(raw)
	return strings.Contains(lower, "unknown field") && strings.Contains(lower, "system")
}

// checkImageSupport rejects an image request the model cannot serve.
func checkImageSupport(model string, body []byte, caps []Capability) error {
	if !containsImagePart(body) {
		return nil
	}
	for _, c := range caps {
		if c.ID != model {
			continue
		}
		if c.SupportsImage() {
			return nil
		}
		return fmt.Errorf("模型 %q 不支持图片输入（能力以远端 models.dev 为准）", model)
	}
	// Unknown model: do not block. The upstream decides, and blocking on a
	// missing catalog entry would break every request during a models.dev outage.
	return nil
}

// containsImagePart reports whether the body carries an image_url content part.
func containsImagePart(body []byte) bool {
	var probe struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return false
	}
	for _, m := range probe.Messages {
		if strings.Contains(string(m.Content), `"image_url"`) {
			return true
		}
	}
	return false
}

// classifyUpstreamError maps an upstream rejection onto the plugin contract's
// error kinds. FreeTierError is called out separately because it means the
// request shape was wrong, not that the account is out of credit.
func classifyUpstreamError(status int, body string) (int, string, string) {
	detail := readableDetail(body)
	lower := strings.ToLower(body)
	switch {
	case strings.Contains(body, "FreeTierError"):
		return status, "free_tier",
			"opencode 免费通道被上游按请求形状拒绝（FreeTierError）：必须带 v2 形状 UA 与四个同值 session 头，且 payload 必须 stream=true 并含官方 read/shell 工具定义。换 key 或换出口都无效。原始详情：" + detail
	case strings.Contains(body, "FreeUsageLimitError"):
		return http.StatusTooManyRequests, "free_usage_limit", "opencode 免费额度已用尽：" + detail
	case strings.Contains(body, "GoUsageLimitError"):
		return http.StatusTooManyRequests, "go_usage_limit", "opencode Go 额度已用尽：" + detail
	case status == 429 || strings.Contains(lower, "too many requests") || strings.Contains(lower, "rate limit"):
		return http.StatusTooManyRequests, "rate_limit", "上游限流：" + detail
	case status == 402 || strings.Contains(lower, "insufficient"):
		return status, "quota", "上游额度不足：" + detail
	case status == 401 || status == 403:
		return status, "auth", "上游鉴权失败：" + detail
	case status >= 500:
		return status, "server", "上游服务错误：" + detail
	}
	return status, "auth", detail
}

// readableDetail pulls a human message out of an upstream error envelope.
func readableDetail(body string) string {
	text := strings.TrimSpace(body)
	if text == "" {
		return "（上游未返回错误详情）"
	}
	var payload map[string]any
	if json.Unmarshal([]byte(text), &payload) == nil {
		var parts []string
		if e, ok := payload["error"].(map[string]any); ok {
			if m, ok := e["message"].(string); ok && m != "" {
				parts = append(parts, m)
			}
		}
		for _, key := range []string{"message", "msg"} {
			if m, ok := payload[key].(string); ok && m != "" {
				parts = append(parts, m)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, " ")
		}
	}
	if len(text) > 500 {
		return text[:500] + "…"
	}
	return text
}

// errorBody renders the contract's OpenAI-shaped error envelope.
func errorBody(kind, message string) []byte {
	out, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": message, "type": kind, "code": kind},
	})
	return out
}
