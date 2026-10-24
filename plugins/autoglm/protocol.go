// Package main implements the AutoGLM channel for AIWorkGate.
//
// The upstream contract is ported field by field from the reference
// implementation in workbuddy2api-panel/internal/autoclaw (package `autoclaw`,
// which is the internal name of the AutoGLM client). Nothing here was guessed:
// every path, header and signature formula below comes from that package.
//
// Two regions exist and they do not share an origin:
//
//	cn      https://autoglm-acceleration-api.zhipuai.cn
//	global  https://autoglm-api.autoglm.ai
//
// Both speak the same "userapi" envelope {code,msg,data}; chat additionally has
// two narrower channels (a proxy path on the userapi origin, and a per-account
// sandbox origin handed out by the sandbox API).
package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Client identity constants. These are the official AutoGLM client's own
// appid/version, embedded in the reference source (auth.go):
//
//	appID     = "100003"
//	appSecret = "38d2391985e2369a5fb8227d8e6cd5e5"
//	clientVer = "1.18.5"
//
// appSecret signs every userapi/checkin request as X-Auth-Sign; it is the
// product's shipped client credential, not a user credential. User tokens live
// only in the DPAPI-sealed accounts.dat and are never printed.
const (
	appID      = "100003"
	appSecret  = "38d2391985e2369a5fb8227d8e6cd5e5"
	clientVer  = "1.18.5"
	webVersion = "1.12.1"
)

// userapiBase is the per-region origin (auth.go userapiBase).
var userapiBase = map[string]string{
	"cn":     "https://autoglm-acceleration-api.zhipuai.cn",
	"global": "https://autoglm-api.autoglm.ai",
}

// Paths, copied from auth.go's const block.
const (
	proxyChatPath    = "/autoclaw-proxy/proxy/autoclaw/v1/chat/completions"
	configModelsPath = "/autoclaw-proxy/proxy/autoclaw-model-config"
	checkinTaskPath  = "/autoclaw-proxy/proxy/autoclaw-task-complete"
	taskListPath     = "/autoclaw-proxy/proxy/autoclaw-task-list"

	// relaySuffix is appended to whatever sandbox endpoint the API returns
	// (upstream.go relaySuffix).
	relaySuffix = "/autoclaw-cloud"
)

// validateChatBody mirrors the reference's validateChatBody: the body must be
// JSON, messages must be a non-empty array, and at least one message must be a
// user turn.
func validateChatBody(body []byte) string {
	var req struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil {
		return "invalid JSON body"
	}
	if len(req.Messages) == 0 {
		return "messages must be a non-empty array"
	}
	for _, m := range req.Messages {
		var mm struct {
			Role string `json:"role"`
		}
		if json.Unmarshal(m, &mm) == nil && mm.Role == "user" {
			return ""
		}
	}
	return "messages must contain at least one user message"
}

// peekModel returns the top-level model of a chat body, or "".
func peekModel(body []byte) string {
	var obj struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &obj) != nil {
		return ""
	}
	return strings.TrimSpace(obj.Model)
}

// isAgentTarget reports whether model names an agent route rather than a
// backend model (chat.go isAgentTarget).
func isAgentTarget(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "openclaw" || m == "openclaw/default" {
		return true
	}
	return strings.HasPrefix(m, "openclaw/") ||
		strings.HasPrefix(m, "openclaw:") ||
		strings.HasPrefix(m, "agent:")
}

// normalizeModel splits a requested model into (agent target, backend model).
func normalizeModel(model string) (string, string) {
	m := strings.TrimSpace(model)
	if m == "" {
		return "openclaw", ""
	}
	if isAgentTarget(m) {
		return m, ""
	}
	return "openclaw", m
}

// prepareModelBody rewrites model to "openclaw" for agent targets and passes
// everything else through untouched.
func prepareModelBody(body []byte) []byte {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return body
	}
	_, xm := normalizeModel(toString(obj["model"]))
	if xm == "" {
		return body
	}
	obj["model"] = "openclaw"
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// prepareProxyBody is the proxy channel's body: force the model and drop the
// OpenAI "user" field, which the proxy rejects.
func prepareProxyBody(body []byte, model string) []byte {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return body
	}
	if model != "" {
		obj["model"] = model
	}
	delete(obj, "user")
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// stripRoutePrefix removes a leading global/ or cn/ from a model name.
func stripRoutePrefix(model string) string {
	for _, p := range []string{"global/", "cn/"} {
		if strings.HasPrefix(model, p) {
			return model[len(p):]
		}
	}
	return model
}

// stripRegionPrefix removes global/ cn/ global: cn: (chat.go stripRegionPrefix).
func stripRegionPrefix(model string) string {
	low := strings.ToLower(model)
	for _, p := range []string{"global/", "cn/", "global:", "cn:"} {
		if strings.HasPrefix(low, p) {
			return model[len(p):]
		}
	}
	return model
}

// requestModelHeader is the x-openclaw-model header for relay sends. Agent
// targets send nothing, because the sandbox picks the agent itself.
func requestModelHeader(body []byte) string {
	m := peekModel(body)
	if m == "" || isAgentTarget(m) {
		return ""
	}
	return stripRoutePrefix(m)
}

// notEnabled reports the upstream answering "this endpoint is not enabled here",
// which makes the auto dispatcher fall through to the next channel.
func notEnabled(status int, body string) bool {
	if status == 404 || status == 501 {
		return true
	}
	if status == 400 {
		lower := strings.ToLower(body)
		for _, k := range []string{"chatcompletions", "endpoint", "not enabled", "disabled"} {
			if strings.Contains(lower, k) {
				return true
			}
		}
	}
	return false
}

// contentPartsText flattens an OpenAI multimodal parts array to its text.
func contentPartsText(parts []any) string {
	sb := strings.Builder{}
	for _, p := range parts {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if toString(pm["type"]) == "text" {
			if t, ok := pm["text"].(string); ok {
				sb.WriteString(t)
			}
		}
	}
	return strings.TrimSpace(sb.String())
}

// buildRelayChatReq turns an OpenAI chat body into the relay request shape:
// {sessionKey, message, thinking}.
func buildRelayChatReq(body []byte) (map[string]any, error) {
	var obj struct {
		User     string `json:"user"`
		Thinking string `json:"reasoning_effort"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	req := map[string]any{"sessionKey": strings.TrimSpace(obj.User)}
	if req["sessionKey"] == "" {
		req["sessionKey"] = "main"
	}
	message := ""
	for i := len(obj.Messages) - 1; i >= 0; i-- {
		m := obj.Messages[i]
		if m.Role != "user" {
			continue
		}
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			message = strings.TrimSpace(s)
			if message != "" {
				break
			}
			continue
		}
		var parts []any
		if json.Unmarshal(m.Content, &parts) == nil {
			message = contentPartsText(parts)
			if message != "" {
				break
			}
		}
	}
	if message == "" {
		return nil, fmt.Errorf("no user text message found")
	}
	req["message"] = message
	if t := strings.TrimSpace(obj.Thinking); t != "" {
		req["thinking"] = t
	}
	return req, nil
}

// extractRunID pulls a run id out of a relay send response.
func extractRunID(data any) string {
	switch v := data.(type) {
	case string:
		return strings.TrimSpace(v)
	case map[string]any:
		for _, k := range []string{"runId", "run_id"} {
			if s, ok := v[k].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}
