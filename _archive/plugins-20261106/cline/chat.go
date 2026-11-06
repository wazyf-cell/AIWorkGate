package main

// The chat surface: forward an OpenAI request to the Cline gateway and either
// relay the SSE stream or fold it into one chat.completion.
//
// Cline speaks OpenAI natively, so there is no protocol conversion — only the
// envelope work in protocol.go (workos: prefix, client headers, schema
// sanitizing, max_tokens clamp) plus the relay in sse.go.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

func (s *server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "cannot read request body")
		return
	}
	var meta struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(body, &meta)

	creds, ok := s.store.get()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "not_configured",
			"Cline 插件尚未登录：请在账号页发起登录，或调用 /admin/credentials 写入 access_token")
		return
	}
	if err := s.egressError(); err != nil {
		writeError(w, http.StatusForbidden, "egress_missing", err.Error())
		return
	}
	// Renew once before sending when the recorded expiry has already passed. A
	// failure here is not fatal: the chat call below reports the real error, and
	// a wrong local clock must not block a working token.
	if creds.expired(time.Now().UnixMilli()) && creds.refreshable() {
		if renewed, err := s.refreshCredential(creds.RefreshToken); err == nil {
			if renewed.RefreshToken == "" {
				renewed.RefreshToken = creds.RefreshToken
			}
			if err := s.store.set(renewed); err == nil {
				creds = *renewed
				s.log("info", "credential_refreshed", map[string]any{"fingerprint": fingerprint(creds.AccessToken)})
			}
		} else {
			s.log("warn", "preflight_refresh_failed", map[string]any{"error": err.Error()})
		}
	}

	shaped, err := BuildBody(body, true)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "cannot convert body: "+err.Error())
		return
	}
	resp, err := s.client.chat(r.Context(), creds.AccessToken, shaped)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "upstream request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()

	// A 401/403 usually means the access token died: renew once and retry, which is
	// exactly what the reference does before giving up.
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if isRegionForbidden(resp.StatusCode, string(raw)) {
			status, kind, message := classifyUpstreamError(resp.StatusCode, string(raw))
			writeJSON(w, status, map[string]any{"error": map[string]any{"message": message, "type": kind, "code": kind}})
			return
		}
		if !creds.refreshable() {
			status, kind, message := classifyUpstreamError(resp.StatusCode, string(raw))
			writeJSON(w, status, map[string]any{"error": map[string]any{"message": message, "type": kind, "code": kind}})
			return
		}
		renewed, refreshErr := s.refreshCredential(creds.RefreshToken)
		if refreshErr != nil {
			writeError(w, http.StatusUnauthorized, "auth",
				"凭据已过期且续期失败："+refreshErr.Error())
			return
		}
		if renewed.RefreshToken == "" {
			renewed.RefreshToken = creds.RefreshToken
		}
		if err := s.store.set(renewed); err != nil {
			s.log("warn", "refresh_persist_failed", map[string]any{"error": err.Error()})
		}
		creds = *renewed
		resp.Body.Close()
		resp, err = s.client.chat(r.Context(), creds.AccessToken, shaped)
		if err != nil {
			writeError(w, http.StatusBadGateway, "upstream_error", "upstream request failed: "+err.Error())
			return
		}
		defer resp.Body.Close()
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		status, kind, message := classifyUpstreamError(resp.StatusCode, string(raw))
		s.log("warn", "upstream_rejected", map[string]any{
			"status": resp.StatusCode, "kind": kind, "detail": truncate(message, 256),
		})
		if kind == "rate_limit" || kind == "quota" {
			// Cooldown hints so the host can park the account without parsing us.
			switch resp.StatusCode {
			case http.StatusTooManyRequests:
				w.Header().Set("Retry-After", "60")
			}
			w.Header().Set("X-Aiworkgate-Account-Status", "rate_limited")
		}
		writeJSON(w, status, map[string]any{
			"error": map[string]any{"message": message, "type": kind, "code": kind},
		})
		return
	}

	if meta.Stream {
		s.relayStream(w, resp.Body)
		return
	}
	completion, err := consumeStream(resp.Body)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "cannot read upstream stream: "+err.Error())
		return
	}
	out, err := completion.json(meta.Model)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "cannot encode completion: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// isRegionForbidden reports whether a 403 is the channel's region lock, which no
// amount of token refreshing can fix.
func isRegionForbidden(status int, body string) bool {
	if status != http.StatusForbidden {
		return false
	}
	lower := strings.ToLower(body)
	for _, marker := range []string{
		"not available in your region", "access forbidden",
		"region not supported", "not available in your country",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// classifyUpstreamError maps an upstream rejection onto the contract's error kinds.
func classifyUpstreamError(status int, body string) (int, string, string) {
	detail := readableDetail(body)
	lower := strings.ToLower(body)
	switch {
	case status == http.StatusPaymentRequired || strings.Contains(lower, "insufficient") ||
		strings.Contains(lower, "credit") || strings.Contains(lower, "quota") && strings.Contains(lower, "exceeded"):
		return status, "quota", "Cline 额度或积分不足（等待不会恢复，请到 app.cline.bot 充值）：" + detail
	case status == http.StatusTooManyRequests:
		return status, "rate_limit", "Cline 限流或当日免费额度已用尽（额度按天、按模型单独结算）：" + detail
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return status, "auth", "Cline 鉴权失败（token 失效或地区受限）：" + detail
	case status >= 500:
		return status, "server", "Cline 上游服务错误：" + detail
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
		if e, ok := payload["error"].(string); ok && e != "" {
			parts = append(parts, e)
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
