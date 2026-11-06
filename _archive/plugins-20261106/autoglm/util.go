package main

// Small shared helpers: type coercion for the loosely typed upstream envelopes,
// trimming/truncating, and the JSON response writers.
//
// Coercion is deliberately forgiving (toInt64 accepts float64/int64/int/string/
// bool). The reference does the same, because the AutoGLM envelope mixes types
// between regions — a balance arrives as a number, a user_id as a string.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var routePrefixRe = regexp.MustCompile(`^[a-z]+_`)

// marshalReader encodes v for a request body; nil stays nil.
func marshalReader(v any) io.Reader {
	if v == nil {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return bytes.NewReader(raw)
}

// toInt64 coerces the envelope's loosely typed numeric fields.
func toInt64(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	case int:
		return int64(t)
	case json.Number:
		n, _ := t.Int64()
		return n
	case string:
		var n int64
		fmt.Sscanf(strings.TrimSpace(t), "%d", &n)
		return n
	case bool:
		if t {
			return 1
		}
	}
	return 0
}

// toString renders an envelope field as text.
func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(raw)
}

// orStr returns the first non-empty rendering among vals.
func orStr(vals ...any) string {
	for _, v := range vals {
		if s := toString(v); s != "" {
			return s
		}
	}
	return ""
}

// firstNonEmpty returns the first non-blank string.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func minLen(s string, n int) int {
	if len(s) < n {
		return len(s)
	}
	return n
}

// sanitizeRegion restricts a user-supplied region to the known values.
func sanitizeRegion(v, def string) string {
	switch v {
	case regionCN, regionGlobal, "all":
		return v
	}
	return def
}

// nowUnix is the single clock read for handlers.
func nowUnix() int64 { return time.Now().Unix() }

// parseURL is a thin shim so login.go does not import net/url directly.
func parseURL(raw string) (*url.URL, error) { return url.Parse(raw) }

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, kind, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": message, "type": kind},
	})
}

// fingerprint is a short, non-reversible tag so two tokens can be told apart in
// a log without exposing either. It is a digest, not the secret.
func fingerprint(secret string) string {
	secret = strings.TrimSpace(stripBearer(secret))
	if secret == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(secret))
	return "sha256:" + hex.EncodeToString(sum[:])[:8]
}
