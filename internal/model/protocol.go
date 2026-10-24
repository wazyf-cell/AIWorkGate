package model

import "strings"

// Upstream protocols a custom connection can speak. The canonical strings are
// stored in SQLite and also used to build upstream URLs, so both the admin API
// and the gateway must agree on the alias table below.
const (
	ProtocolOpenAI    = "openai"
	ProtocolAnthropic = "anthropic"
	ProtocolGemini    = "gemini"
)

// NormalizeProtocol folds a protocol name and reports whether it is supported.
// An empty value means the historical OpenAI-compatible protocol. Callers that
// need to accept legacy rows may fall back to ProtocolOpenAI when ok is false;
// the admin API must not.
func NormalizeProtocol(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", ProtocolOpenAI, "openai-compatible", "openai_compatible":
		return ProtocolOpenAI, true
	case ProtocolAnthropic, "claude":
		return ProtocolAnthropic, true
	case ProtocolGemini, "google":
		return ProtocolGemini, true
	default:
		return "", false
	}
}
