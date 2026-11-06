package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

func readLimited(r io.Reader, n int64) string {
	b, _ := io.ReadAll(io.LimitReader(r, n))
	return string(b)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func errorDetail(body string) string {
	var data map[string]any
	if err := json.Unmarshal([]byte(body), &data); err == nil {
		parts := make([]string, 0, 6)
		if e, ok := data["error"].(map[string]any); ok {
			for _, k := range []string{"code", "type", "message"} {
				if s, ok := e[k].(string); ok && s != "" {
					parts = append(parts, s)
				}
			}
		}
		for _, k := range []string{"error_code", "error_msg", "message"} {
			if s, ok := data[k].(string); ok && s != "" {
				parts = append(parts, s)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, " ")
		}
	}
	if strings.TrimSpace(body) == "" {
		return "empty upstream response"
	}
	return truncate(strings.TrimSpace(body), 500)
}

func isAuthError(status int, body string) bool {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return true
	}
	lower := strings.ToLower(body)
	return strings.Contains(body, "APIG.0602") ||
		strings.Contains(lower, "invalid token") ||
		strings.Contains(lower, "token expired") ||
		strings.Contains(lower, "token is invalid")
}

func isQuotaFailure(status int, body string) bool {
	lower := strings.ToLower(body)
	return strings.Contains(body, "4291") || strings.Contains(lower, "insufficient quota") || strings.Contains(lower, "insufficient_quota")
}

func isRateLimitFailure(status int, body string) bool {
	if status == http.StatusTooManyRequests {
		return true
	}
	for _, code := range []string{"6004", "4291", "81111.429", "APIG.0602", "14003"} {
		if strings.Contains(body, code) {
			return true
		}
	}
	lower := strings.ToLower(body)
	return strings.Contains(lower, "rate limit") || strings.Contains(lower, "too many requests") ||
		strings.Contains(lower, "frequency limit") || strings.Contains(lower, "frequency_limit")
}

func isQueueFailure(status int, body string) bool {
	if status != http.StatusBadRequest {
		return false
	}
	if strings.Contains(body, "TM.00001041") {
		return true
	}
	lower := strings.ToLower(body)
	return strings.Contains(lower, "peak usage") || strings.Contains(lower, "try again after") ||
		strings.Contains(lower, "peak hours") || strings.Contains(lower, "high demand") ||
		strings.Contains(lower, "too many requests")
}

func isQuotaCode(code, message string) bool {
	return strings.Contains(code, "4291") || strings.Contains(strings.ToLower(message), "insufficient")
}

func isQueueCode(code string) bool {
	if code == "TM.00001041" {
		return true
	}
	lower := strings.ToLower(code)
	for _, needle := range []string{"81111", "tpm", "429", "rate", "limit", "排队", "限流", "too many request"} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}
