package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"sort"
	"strings"
	"time"
)

// signRequestHuawei implements the SDK-HMAC-SHA256 request signing used by the
// CodeArts SNAP APIs. It is a faithful port of `sign.ts` from the reference
// implementation:
//
//   - the canonical URI always carries a trailing slash;
//   - the query string is taken verbatim (url.search.slice(1));
//   - signed headers are host, x-sdk-date, x-sdk-content-sha256,
//     x-security-token, optional extras, and content-type for non-GET;
//   - Authorization is added after signing and is not itself signed.
func signRequestHuawei(method, rawURL string, body []byte, ak, sk, securityToken string, extra map[string]string, now time.Time) (map[string]string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	uri := u.Path
	if uri == "" {
		uri = "/"
	}
	if !strings.HasSuffix(uri, "/") {
		uri += "/"
	}
	query := u.RawQuery
	dateStamp := now.UTC().Format("20060102T150405Z")
	payloadHash := sha256HexBytes(body)

	headers := map[string]string{
		"host":                 u.Host,
		"x-sdk-date":           dateStamp,
		"x-sdk-content-sha256": payloadHash,
		"x-security-token":     securityToken,
	}
	for k, v := range extra {
		headers[strings.ToLower(k)] = v
	}
	if strings.ToUpper(method) != "GET" {
		headers["content-type"] = "application/json"
	}

	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)

	var headerLines strings.Builder
	for i, k := range names {
		if i > 0 {
			headerLines.WriteByte('\n')
		}
		headerLines.WriteString(k)
		headerLines.WriteByte(':')
		headerLines.WriteString(headers[k])
	}
	canonicalRequest := strings.Join([]string{
		strings.ToUpper(method),
		uri,
		query,
		headerLines.String(),
		"",
		strings.Join(names, ";"),
		payloadHash,
	}, "\n")
	canonicalHash := sha256HexBytes([]byte(canonicalRequest))
	stringToSign := "SDK-HMAC-SHA256\n" + dateStamp + "\n" + canonicalHash
	mac := hmac.New(sha256.New, []byte(sk))
	mac.Write([]byte(stringToSign))
	signature := hex.EncodeToString(mac.Sum(nil))

	out := make(map[string]string, len(headers)+1)
	for k, v := range headers {
		out[k] = v
	}
	out["Authorization"] = "SDK-HMAC-SHA256 Access=" + ak + ",SignedHeaders=" + strings.Join(names, ";") + ",Signature=" + signature
	return out, nil
}

func sha256HexBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
