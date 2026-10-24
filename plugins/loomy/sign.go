// Package main — 讯飞 account-API request signing.
//
// Ported from loomy-auth.ts in the reference (buildLoomySigningString /
// loomyAuthHeaders). No secret lives in this file: the accessKeyId and
// accessKeySecret come from the operator's environment
// (LOOMY_XFYUN_ACCESS_KEY_ID / LOOMY_XFYUN_ACCESS_KEY_SECRET) and the reference's
// plaintext pair is deliberately not copied.
//
// Canonical string (nine lines, "\n"-joined):
//
//	METHOD
//	/rfc3986-escaped/path
//	rfc3986-escaped-query
//	base64(md5(body))            <- empty body -> empty string
//	content-type
//	date (UTC, RFC 1123)
//	nonce (uuid)
//	(signed headers, empty)
//	(canonicalized headers, empty)
//
// signature = base64(HMAC-SHA1(accessKeySecret, canonical)); header
// "Authorization: account <accessKeyId>:<signature>" plus Date, Nonce,
// Content-Type and Content-MD5.
package main

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// xfyunSecret is the operator-supplied signing pair.
type xfyunSecret struct {
	AccessKeyID     string
	AccessKeySecret string
}

func (s xfyunSecret) complete() bool {
	return strings.TrimSpace(s.AccessKeyID) != "" && strings.TrimSpace(s.AccessKeySecret) != ""
}

// signingFromEnv reads the only accepted sources of the signing pair. Absent
// values make POST /accounts/login answer 400 naming both variables (never 501:
// the host reads 501 as "this plugin has no login" and hides the entry, leaving
// the operator no way to start the flow and see the hint).
func signingFromEnv() xfyunSecret {
	return xfyunSecret{
		AccessKeyID:     strings.TrimSpace(os.Getenv(envAccessKeyID)),
		AccessKeySecret: strings.TrimSpace(os.Getenv(envAccessKeySecret)),
	}
}

// fingerprint identifies which key is configured without printing it.
func (s xfyunSecret) fingerprint() string {
	if !s.complete() {
		return ""
	}
	sum := sha256Sum(s.AccessKeyID)
	return "sha256:" + sum[:8]
}

// loomyContentMD5 is loomyContentMd5: BASE64 of the body MD5 (not hex).
func loomyContentMD5(body string) string {
	if body == "" {
		return ""
	}
	sum := md5.Sum([]byte(body))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// escapeRFC3986 is encodeURIComponent plus the five characters JavaScript's
// percent-encoder leaves alone but the 讯飞 scheme escapes.
func escapeRFC3986(v string) string {
	r := url.QueryEscape(v)
	r = strings.ReplaceAll(r, "+", "%20")
	r = strings.ReplaceAll(r, "%7E", "~")
	for _, p := range [][2]string{{"!", "%21"}, {"'", "%27"}, {"(", "%28"}, {")", "%29"}, {"*", "%2A"}} {
		r = strings.ReplaceAll(r, p[0], p[1])
	}
	return r
}

// buildEscapedPath is buildEscapedPath: leading "/", no trailing "/", every
// segment escaped independently.
func buildEscapedPath(raw string) string {
	clean := raw
	if !strings.HasPrefix(clean, "/") {
		clean = "/" + clean
	}
	if len(clean) > 1 && strings.HasSuffix(clean, "/") {
		clean = clean[:len(clean)-1]
	}
	segs := strings.Split(clean, "/")
	for i, s := range segs {
		if s != "" {
			segs[i] = escapeRFC3986(s)
		}
	}
	return strings.Join(segs, "/")
}

// buildEscapedQuery is buildEscapedQueryString: sorted keys (Object.entries
// order of a plain object is insertion order, and the reference inserts nothing
// here for the endpoints this plugin calls), values RFC3986-escaped.
func buildEscapedQueryString(query map[string]string) string {
	if len(query) == 0 {
		return ""
	}
	keys := make([]string, 0, len(query))
	for k := range query {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, escapeRFC3986(k)+"="+escapeRFC3986(query[k]))
	}
	return strings.Join(parts, "&")
}

// buildLoomySigningString is buildLoomySigningString verbatim.
func buildLoomySigningString(method, path string, query map[string]string, body, contentType, date, nonce string) string {
	return strings.Join([]string{
		strings.ToUpper(method),
		buildEscapedPath(path),
		buildEscapedQueryString(query),
		loomyContentMD5(body),
		contentType,
		date,
		nonce,
		"",
		"",
	}, "\n")
}

// signLoomy returns the headers loomyAuthHeaders produces.
func signLoomy(s xfyunSecret, method, path string, query map[string]string, body string, now time.Time) http.Header {
	date := now.UTC().Format(http.TimeFormat)
	nonce := randomHex(16)
	contentType := "application/json"
	canonical := buildLoomySigningString(method, path, query, body, contentType, date, nonce)
	mac := hmac.New(sha1.New, []byte(s.AccessKeySecret))
	mac.Write([]byte(canonical))
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	h := http.Header{}
	h.Set("Authorization", "account "+s.AccessKeyID+":"+sig)
	h.Set("Date", date)
	h.Set("Nonce", nonce)
	h.Set("Content-Type", contentType)
	if md5b64 := loomyContentMD5(body); md5b64 != "" {
		h.Set("Content-MD5", md5b64)
	}
	return h
}

// randomHex returns n random bytes as hex.
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", buf)
}
