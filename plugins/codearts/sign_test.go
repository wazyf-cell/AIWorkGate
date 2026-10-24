package main

import (
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"sort"
	"strings"
	"testing"
	"time"
)

// expectedSign is an independent implementation used to pin the wire format.
func expectedSign(t *testing.T, method, rawURL string, body []byte, ak, sk, st string, extra map[string]string, now time.Time) map[string]string {
	t.Helper()
	protocolHost := strings.SplitN(strings.TrimPrefix(strings.TrimPrefix(rawURL, "https://"), "http://"), "/", 2)[0]
	pathAndQuery := ""
	if idx := strings.Index(rawURL, "://"); idx >= 0 {
		rest := rawURL[idx+3:]
		if slash := strings.Index(rest, "/"); slash >= 0 {
			pathAndQuery = rest[slash:]
		} else {
			pathAndQuery = "/"
		}
	}
	path, query := pathAndQuery, ""
	if q := strings.Index(pathAndQuery, "?"); q >= 0 {
		path, query = pathAndQuery[:q], pathAndQuery[q+1:]
	}
	if !strings.HasSuffix(path, "/") {
		path += "/"
	}
	payloadHash := func(b []byte) string {
		sum := sha256.Sum256(b)
		return hex.EncodeToString(sum[:])
	}
	date := now.UTC().Format("20060102T150405Z")
	headers := map[string]string{
		"host":                 protocolHost,
		"x-sdk-date":           date,
		"x-sdk-content-sha256": payloadHash(body),
		"x-security-token":     st,
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
	var lines []string
	for _, k := range names {
		lines = append(lines, k+":"+headers[k])
	}
	canonical := strings.Join([]string{strings.ToUpper(method), path, query, strings.Join(lines, "\n"), "", strings.Join(names, ";"), payloadHash(body)}, "\n")
	canonicalHash := payloadHash([]byte(canonical))
	toSign := "SDK-HMAC-SHA256\n" + date + "\n" + canonicalHash
	mac := hmac.New(sha256.New, []byte(sk))
	mac.Write([]byte(toSign))
	headers["Authorization"] = "SDK-HMAC-SHA256 Access=" + ak + ",SignedHeaders=" + strings.Join(names, ";") + ",Signature=" + hex.EncodeToString(mac.Sum(nil))
	return headers
}

func TestSigningMatchesIndependentImplementation(t *testing.T) {
	now := time.Date(2026, 10, 20, 10, 20, 30, 0, time.UTC)
	rawURL := "https://snap-access.cn-north-4.myhuaweicloud.com/api/v2/chat/completions?model=GLM-5.2&x=1"
	body := []byte(`{"model":"GLM-5.2"}`)
	extra := map[string]string{"maas_type": "benefit"}
	got, err := signRequestHuawei("POST", rawURL, body, "AKID", "SECRET", "TOKEN", extra, now)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	want := expectedSign(t, "POST", rawURL, body, "AKID", "SECRET", "TOKEN", extra, now)
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("header %q = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("header count = %d, want %d: %v", len(got), len(want), got)
	}
	if got["x-sdk-date"] != "20261020T102030Z" {
		t.Fatalf("date stamp = %q", got["x-sdk-date"])
	}
	wantAuthPrefix := "SDK-HMAC-SHA256 Access=AKID,SignedHeaders=content-type;host;maas_type;x-sdk-content-sha256;x-sdk-date;x-security-token,Signature="
	if !strings.HasPrefix(got["Authorization"], wantAuthPrefix) {
		t.Fatalf("authorization = %q", got["Authorization"])
	}
	if strings.Contains(got["Authorization"], "Authorization=") {
		t.Fatal("Authorization must not be part of the signed headers")
	}

	getHeaders, err := signRequestHuawei("GET", rawURL, nil, "AKID", "SECRET", "TOKEN", nil, now)
	if err != nil {
		t.Fatalf("sign GET: %v", err)
	}
	if _, ok := getHeaders["content-type"]; ok {
		t.Fatal("GET must not sign content-type")
	}
}

func TestDPoPProofStructureAndSignature(t *testing.T) {
	key, err := generateDPoPKey()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	now := time.Unix(1760000000, 0)
	jws, err := key.signDPoP("POST", "https://sts.cn-north-4.myhuaweicloud.com/v1/oauth2/tokens", now)
	if err != nil {
		t.Fatalf("sign DPoP: %v", err)
	}
	parts := strings.Split(jws, ".")
	if len(parts) != 3 {
		t.Fatalf("JWS parts = %d", len(parts))
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	var header struct {
		Alg string            `json:"alg"`
		Typ string            `json:"typ"`
		JWK map[string]string `json:"jwk"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		t.Fatalf("parse header: %v", err)
	}
	if header.Alg != "ES256" || header.Typ != "dpop+jwt" || header.JWK["kty"] != "EC" || header.JWK["crv"] != "P-256" {
		t.Fatalf("header = %+v", header)
	}
	if _, hasD := header.JWK["d"]; hasD {
		t.Fatal("public JWK must not include the private scalar")
	}
	payloadJSON, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var payload struct {
		HTM string `json:"htm"`
		HTU string `json:"htu"`
		IAT int64  `json:"iat"`
		JTI string `json:"jti"`
	}
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload.HTM != "POST" || payload.IAT != now.Unix() || len(payload.JTI) != 64 {
		t.Fatalf("payload = %+v", payload)
	}
	xb, _ := base64.RawURLEncoding.DecodeString(header.JWK["x"])
	yb, _ := base64.RawURLEncoding.DecodeString(header.JWK["y"])
	pub := &ecdsa.PublicKey{Curve: key.private.Curve, X: new(big.Int).SetBytes(xb), Y: new(big.Int).SetBytes(yb)}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		t.Fatalf("signature bytes = %d, err %v", len(sig), err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(pub, digest[:], r, s) {
		t.Fatal("DPoP signature does not verify")
	}

	restored, err := dpopKeyFromJWK(key.marshalJWK())
	if err != nil {
		t.Fatalf("restore JWK: %v", err)
	}
	if restored.publicJWK()["x"] != key.publicJWK()["x"] {
		t.Fatal("restored key mismatch")
	}
}

func TestNormalizeModelID(t *testing.T) {
	cases := map[string]string{
		"GLM-5.2-0001":           "GLM-5.2",
		"deepseek-v4-flash-0012": "deepseek-v4-flash",
		"glm-5.3-flash":          "glm-5.3-flash",
		"model-12ab":             "model-12ab",
		"abc-1234":               "abc",
	}
	for in, want := range cases {
		if got := normalizeModelID(in); got != want {
			t.Fatalf("normalizeModelID(%q) = %q, want %q", in, got, want)
		}
	}
}
