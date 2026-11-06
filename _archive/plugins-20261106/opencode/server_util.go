package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
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

// fingerprint is a short, non-reversible tag so two keys can be told apart in a
// log without exposing either.
func fingerprint(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return "sha256:" + hex.EncodeToString(sum[:4])
}

// keyHint describes a stored key without revealing it.
func keyHint(key string) string {
	key = strings.TrimSpace(key)
	if key == anonymousKey {
		return "匿名通道（字面量 public）"
	}
	if len(key) <= 6 {
		return "已配置（长度 " + strconv.Itoa(len(key)) + "）"
	}
	return "已配置（" + key[:3] + "…" + key[len(key)-4:] + "）"
}

func loginLabel(key string) string {
	if key == anonymousKey {
		return "匿名通道"
	}
	return "OpenCode " + keyHint(key)
}

// newUUID returns a random RFC-4122 v4 UUID, used as a login session id.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	dst := make([]byte, 36)
	hex.Encode(dst[0:8], b[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], b[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], b[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], b[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], b[10:16])
	return string(dst)
}
