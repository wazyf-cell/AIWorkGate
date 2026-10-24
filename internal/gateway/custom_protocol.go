package gateway

// Endpoint and header rules for the three upstream protocols a custom
// connection can speak. Body translation lives in upstream_*.go; this file is
// shared with the admin API (model discovery and the explicit probe) so a URL
// is built in exactly one place.

import (
	"fmt"
	"net/url"
	"strings"

	"aiworkgate/internal/model"
)

// CustomProtocolChatURL returns the upstream chat endpoint for one protocol.
// The model is part of the Gemini path; OpenAI and Anthropic send it in the
// body instead.
func CustomProtocolChatURL(protocol, baseURL, upstreamModel string, stream bool) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	switch NormalizeCustomProtocolName(protocol) {
	case "anthropic":
		base = ensureSuffixPath(base, "/v1")
		return base + "/messages", nil
	case "gemini":
		base = ensureGeminiBase(base)
		model := strings.TrimPrefix(strings.TrimSpace(upstreamModel), "models/")
		if model == "" {
			return "", fmt.Errorf("gemini upstream model is empty")
		}
		action := ":generateContent"
		if stream {
			action = ":streamGenerateContent?alt=sse"
		}
		return base + "/models/" + url.PathEscape(model) + action, nil
	default:
		return CustomChatURL(base), nil
	}
}

// CustomProtocolModelsURL returns the model-list endpoint used by connection
// discovery. OpenAI and Anthropic both expose GET /models under /v1; Gemini
// uses /v1beta/models.
func CustomProtocolModelsURL(protocol, baseURL string) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	switch NormalizeCustomProtocolName(protocol) {
	case "anthropic":
		return ensureSuffixPath(base, "/v1") + "/models", nil
	case "gemini":
		return ensureGeminiBase(base) + "/models?pageSize=1000", nil
	default:
		return ensureSuffixPath(base, "/v1") + "/models", nil
	}
}

// CustomProtocolHeaders returns the authentication and version headers for one
// protocol. The caller sets Content-Type/Accept separately.
func CustomProtocolHeaders(protocol, key string) map[string]string {
	headers := map[string]string{}
	key = strings.TrimSpace(key)
	switch NormalizeCustomProtocolName(protocol) {
	case "anthropic":
		if key != "" {
			headers["x-api-key"] = key
		}
		headers["anthropic-version"] = "2023-06-01"
	case "gemini":
		if key != "" {
			headers["x-goog-api-key"] = key
		}
	default:
		if key != "" {
			headers["Authorization"] = "Bearer " + key
		}
	}
	return headers
}

// NormalizeCustomProtocolName folds the stored protocol value. Unknown values
// fall back to the OpenAI-compatible protocol, matching the pre-connection
// behaviour for rows written by older builds. The admin API validates input
// with model.NormalizeProtocol first, so this fallback is only for stored rows.
func NormalizeCustomProtocolName(raw string) string {
	if protocol, ok := model.NormalizeProtocol(raw); ok {
		return protocol
	}
	return model.ProtocolOpenAI
}

func ensureSuffixPath(base, suffix string) string {
	if strings.HasSuffix(base, suffix) {
		return base
	}
	// Some users paste https://host/v1/ already; avoid /v1/v1.
	if strings.HasSuffix(base, "/v1") && suffix == "/v1" {
		return base
	}
	return base + suffix
}

// ensureGeminiBase accepts https://generativelanguage.googleapis.com,
// .../v1beta and .../v1 without duplicating the version segment.
func ensureGeminiBase(base string) string {
	lower := strings.ToLower(base)
	for _, suffix := range []string{"/v1beta", "/v1"} {
		if strings.HasSuffix(lower, suffix) {
			return base
		}
	}
	return base + "/v1beta"
}
