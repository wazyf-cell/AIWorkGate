package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// decodeOptionalJSON decodes a request body that the caller may legitimately
// send empty (an empty POST means "use your default method"). A malformed body
// is not fatal here either: the caller reads only optional hints out of it and
// validates what it needs itself.
func decodeOptionalJSON(r *http.Request, dst any) error {
	if r.Body == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(dst)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, kind, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": message, "type": kind, "code": kind},
	})
}

// openAIErrorBody renders an OpenAI-shaped error for a non-2xx upstream result.
func openAIErrorBody(status int, message string) []byte {
	if strings.TrimSpace(message) == "" {
		message = "upstream returned a non-2xx status"
	}
	b, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "upstream_error",
			"code":    status,
		},
	})
	return b
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// fingerprint is a short, non-reversible tag so two tokens can be told apart in
// a log without exposing either.
func fingerprint(secret string) string {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return ""
	}
	sum := sha256Sum(secret)
	return "sha256:" + sum[:8]
}
