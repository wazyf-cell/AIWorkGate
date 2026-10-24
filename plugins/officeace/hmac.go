package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	sdkAlgorithm = "SDK-HMAC-SHA256"
	sdkDateFmt   = "20060102T150405Z"
)

// signCredential is the Huawei Cloud SDK-HMAC material from the OfficeAce
// session (model_app_key / model_app_secret / sts_token / project_id).
type signCredential struct {
	AccessKey     string
	SecretKey     string
	SecurityToken string
	ProjectID     string
}

type signInput struct {
	Method      string
	URL         string
	Body        string
	ContentType string
	Credential  signCredential
	Now         time.Time
}

type signedHeaders struct {
	Header        map[string]string
	Authorization string
	SignedNames   []string
	SDKDate       string
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func hmacSHA256Hex(key, data string) string {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte(data))
	return hex.EncodeToString(mac.Sum(nil))
}

// rfc3986Escape escapes every byte except unreserved characters. Go's
// url.QueryEscape uses '+', and PathEscape leaves some sub-delims, both of
// which break Huawei's canonical form.
func rfc3986Escape(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&0x0f])
	}
	return b.String()
}

func canonicalPath(u *url.URL) string {
	p := u.EscapedPath()
	if p == "" {
		p = "/"
	}
	segments := strings.Split(p, "/")
	for i, seg := range segments {
		if seg == "" {
			continue
		}
		decoded, err := url.PathUnescape(seg)
		if err != nil {
			decoded = seg
		}
		segments[i] = rfc3986Escape(decoded)
	}
	out := strings.Join(segments, "/")
	if !strings.HasSuffix(out, "/") {
		out += "/"
	}
	return out
}

func canonicalQuery(u *url.URL) string {
	q := u.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		values := append([]string(nil), q[k]...)
		sort.Strings(values)
		for _, v := range values {
			parts = append(parts, rfc3986Escape(k)+"="+rfc3986Escape(v))
		}
	}
	return strings.Join(parts, "&")
}

func normalizeHeaderValue(v string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(v)), " ")
}

// signSDKHMAC produces Huawei Cloud SDK-HMAC-SHA256 headers. It mirrors the
// implementation OfficeAce ships, including the trailing slash on the
// canonical path and the blank line after canonical headers.
func signSDKHMAC(in signInput) (signedHeaders, error) {
	ak := strings.TrimSpace(in.Credential.AccessKey)
	sk := strings.TrimSpace(in.Credential.SecretKey)
	token := strings.TrimSpace(in.Credential.SecurityToken)
	if ak == "" || sk == "" {
		return signedHeaders{}, errors.New("缺少 MaaS 签名凭据（model_app_key / model_app_secret）")
	}
	u, err := url.Parse(in.URL)
	if err != nil || u.Host == "" {
		return signedHeaders{}, fmt.Errorf("无效的上游 URL %q: %v", in.URL, err)
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	sdkDate := now.UTC().Format(sdkDateFmt)

	raw := map[string]string{
		"host":       u.Host,
		"x-sdk-date": sdkDate,
	}
	if token != "" {
		raw["x-security-token"] = token
	}
	if ct := strings.TrimSpace(in.ContentType); ct != "" {
		raw["content-type"] = ct
	}
	if pid := strings.TrimSpace(in.Credential.ProjectID); pid != "" {
		raw["x-project-id"] = pid
	}
	normalized := make(map[string]string, len(raw))
	names := make([]string, 0, len(raw))
	for k, v := range raw {
		key := strings.ToLower(k)
		normalized[key] = normalizeHeaderValue(v)
		names = append(names, key)
	}
	sort.Strings(names)

	var headerLines []string
	for _, name := range names {
		headerLines = append(headerLines, name+":"+normalized[name])
	}
	signedNames := strings.Join(names, ";")
	canonicalRequest := strings.Join([]string{
		strings.ToUpper(in.Method),
		canonicalPath(u),
		canonicalQuery(u),
		strings.Join(headerLines, "\n") + "\n",
		signedNames,
		sha256Hex(in.Body),
	}, "\n")
	stringToSign := strings.Join([]string{sdkAlgorithm, sdkDate, sha256Hex(canonicalRequest)}, "\n")
	signature := hmacSHA256Hex(sk, stringToSign)
	auth := fmt.Sprintf("%s Access=%s, SignedHeaders=%s, Signature=%s", sdkAlgorithm, ak, signedNames, signature)

	headers := map[string]string{
		"Host":          u.Host,
		"X-Sdk-Date":    sdkDate,
		"Authorization": auth,
	}
	if token != "" {
		headers["X-Security-Token"] = token
	}
	if ct := strings.TrimSpace(in.ContentType); ct != "" {
		headers["Content-Type"] = ct
	}
	if pid := strings.TrimSpace(in.Credential.ProjectID); pid != "" {
		headers["X-Project-ID"] = pid
	}
	return signedHeaders{Header: headers, Authorization: auth, SignedNames: names, SDKDate: sdkDate}, nil
}
