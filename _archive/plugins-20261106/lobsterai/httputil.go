package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

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

func fingerprint(secret string) string {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return ""
	}
	sum := sha256Sum(secret)
	return "sha256:" + sum[:8]
}

// newUUIDHex returns 32 hex characters of randomness, matching the reference's
// `randomUUID()` usage for the login session id (it uses the same value as both
// `state` and `uuid`, dashes normalised away by the flow).
func newUUIDHex() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000000000000000000000000000"
	}
	return hex.EncodeToString(b[:])
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
