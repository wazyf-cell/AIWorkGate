package logging

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Body capture limits (D97): with Debug on, full bodies up to 256KB are kept
// for retention.body_preview_days; with Debug off only a 1KB preview is kept
// for the API log retention window.
const (
	BodyReqLimit     = 256 << 10
	BodyRespLimit    = 256 << 10
	BodyPreviewLimit = 1024
)

// bearerRe collapses credentials carried in an Authorization header value.
var bearerRe = regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9._\-]+`)

// bodyKeyRe redacts key/value pairs in non-JSON bodies (form data, headers,
// query strings, partially written JSON). Values may contain spaces so a
// header-style "Authorization: Bearer xyz" collapses as a whole.
var bodyKeyRe = regexp.MustCompile(`(?i)("?)(authorization|api[_-]?key|apikey|access[_-]?token|refresh[_-]?token|token|secret|cookie|password)("?)(\s*[:=]\s*)("?)([^"',;\r\n}&]+)`)

// sensitiveKeySuffixes are matched against the normalised key (lowercased,
// separators removed) so x-api-key, access_token and client-secret are caught
// while max_tokens / total_tokens are not.
var sensitiveKeySuffixes = []string{
	"authorization", "apikey", "token", "secret", "cookie", "password",
}

func sensitiveKey(key string) bool {
	n := strings.ToLower(strings.Map(func(r rune) rune {
		switch r {
		case '_', '-', ' ':
			return -1
		}
		return r
	}, key))
	for _, s := range sensitiveKeySuffixes {
		if strings.HasSuffix(n, s) {
			return true
		}
	}
	return false
}

// Truncate cuts s to at most n bytes, backing off to a valid UTF-8 boundary so
// a multi-byte character is never split.
func Truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	b := s[:n]
	for len(b) > 0 && !utf8.ValidString(b) {
		b = b[:len(b)-1]
	}
	return b
}

// RedactBody masks credentials in a request/response body. Valid JSON is
// redacted structurally (values of sensitive keys become "***"); anything
// else falls back to a key/value regex. Both paths also collapse
// "Bearer <token>" to "Bearer ***".
func RedactBody(body string) string {
	if body == "" {
		return ""
	}
	trimmed := strings.TrimSpace(body)
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		var v any
		if err := json.Unmarshal([]byte(trimmed), &v); err == nil {
			v = redactJSONValue(v)
			if b, err := json.Marshal(v); err == nil {
				return bearerRe.ReplaceAllString(string(b), "Bearer ***")
			}
		}
	}
	out := bearerRe.ReplaceAllString(body, "Bearer ***")
	out = bodyKeyRe.ReplaceAllString(out, "${1}${2}${3}${4}${5}***")
	return out
}

func redactJSONValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if sensitiveKey(k) {
				t[k] = "***"
				continue
			}
			t[k] = redactJSONValue(val)
		}
		return t
	case []any:
		for i := range t {
			t[i] = redactJSONValue(t[i])
		}
		return t
	default:
		return v
	}
}
