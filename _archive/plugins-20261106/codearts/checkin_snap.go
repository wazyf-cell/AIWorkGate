package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// snapRequest performs a signed SNAP call and unwraps the {code,data}
// envelope used by the statistics and ops endpoints.
func (p *Plugin) snapRequest(ctx context.Context, method, rawURL string, body []byte, acct *Account, proxyURL string) snapResult {
	// REQUEST_TIMEOUT_MS in the reference implementation.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	extra := map[string]string{"Agent-Type": "PromptCenter", "X-Language": "zh-cn"}
	resp, err := p.signedRequest(ctx, method, rawURL, body, acct, proxyURL, acct.ID, nil, extra)
	if err != nil {
		return snapResult{message: err.Error()}
	}
	defer resp.Body.Close()
	text, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return snapResult{
			message: describeHTTPFailure(resp.StatusCode, text),
			status:  resp.StatusCode,
			resetAt: retryAfterReset(p.now(), resp.Header.Get("Retry-After")),
		}
	}
	var root map[string]any
	if err := json.Unmarshal(text, &root); err != nil {
		return snapResult{message: "请求失败或响应无法解析"}
	}
	if code, ok := numberValue(root["code"]); ok {
		if code != 0 {
			msg, _ := root["message"].(string)
			if msg == "" {
				msg, _ = root["msg"].(string)
			}
			if msg == "" {
				msg = fmt.Sprintf("业务码 %d", int64(code))
			}
			return snapResult{message: msg}
		}
		data, ok := root["data"].(map[string]any)
		if !ok {
			return snapResult{message: "响应缺少 data 字段"}
		}
		return snapResult{ok: true, data: data}
	}
	return snapResult{ok: true, data: root}
}

func describeHTTPFailure(status int, body []byte) string {
	text := strings.TrimSpace(string(body))
	if len(text) > 300 {
		text = text[:300]
	}
	if text == "" {
		return fmt.Sprintf("HTTP %d", status)
	}
	return fmt.Sprintf("HTTP %d: %s", status, text)
}

// snapError wraps a failed SNAP answer, preserving the user-facing prefix while
// carrying the attribution the host understands (plan 2026-10-21 §4).
func snapError(prefix string, res snapResult) error {
	message := res.message
	if prefix != "" {
		message = prefix + "：" + message
	}
	return &checkinFailure{Kind: classifySnapFailure(res), ResetAt: res.resetAt, Message: message}
}

// classifySnapFailure maps a failed SNAP answer to a kind. Only unambiguous
// signals count: HTTP 401/403, 429, 402, and an explicit insufficient-balance
// wording. Everything else stays unattributed instead of guessed.
func classifySnapFailure(res snapResult) string {
	switch res.status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return kindSessionDead
	case http.StatusTooManyRequests:
		return kindRateLimited
	case http.StatusPaymentRequired:
		return kindQuota
	}
	return kindFromMessage(res.message)
}

// quotaHints are upstream wordings that mean "balance/quota exhausted" rather
// than a transient error.
var quotaHints = []string{
	"余额不足", "额度不足", "积分不足", "配额不足",
	"insufficient quota", "insufficient balance", "insufficient credit",
	"quota exceeded", "quota exhausted", "credits exhausted",
}

func kindFromMessage(message string) string {
	lower := strings.ToLower(message)
	for _, hint := range quotaHints {
		if strings.Contains(lower, hint) {
			return kindQuota
		}
	}
	return ""
}

// retryAfterReset turns a Retry-After header (delay-seconds or HTTP date) into
// an absolute Unix reset time. Absent/garbage values mean "not provided".
func retryAfterReset(now time.Time, value string) int64 {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds > 0 {
			return now.Unix() + seconds
		}
		return 0
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) {
		return at.Unix()
	}
	return 0
}

func numberValue(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return f, err == nil
	}
	return 0, false
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

func boolValue(v any) bool {
	b, _ := v.(bool)
	return b
}

func identifierValue(v any) (any, string) {
	switch x := v.(type) {
	case string:
		return x, x
	case float64:
		return x, strconv.FormatInt(int64(x), 10)
	case json.Number:
		return x, x.String()
	case int64:
		return x, strconv.FormatInt(x, 10)
	case int:
		return x, strconv.Itoa(x)
	}
	return nil, ""
}
