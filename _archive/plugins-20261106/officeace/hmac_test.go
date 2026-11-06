package main

import (
	"strings"
	"testing"
	"time"
)

func TestSDKHMACKnownVector(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 34, 56, 0, time.UTC)
	signed, err := signSDKHMAC(signInput{
		Method:      "POST",
		URL:         "https://maas.example.com/v2/chat/completions",
		Body:        `{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}],"stream":true}`,
		ContentType: "application/json",
		Credential: signCredential{
			AccessKey:     "test-ak",
			SecretKey:     "test-sk",
			SecurityToken: "test-token",
			ProjectID:     "test-project",
		},
		Now: now,
	})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	const want = "SDK-HMAC-SHA256 Access=test-ak, SignedHeaders=content-type;host;x-project-id;x-sdk-date;x-security-token, Signature=ce64cd048a6f22fb979c0dcaf66c3246b06fbfff6bb965b2bb2b5cd6b4aaaf04"
	if signed.Authorization != want {
		t.Fatalf("authorization mismatch\n got: %s\nwant: %s", signed.Authorization, want)
	}
	if signed.Header["X-Sdk-Date"] != "20261020T123456Z" {
		t.Fatalf("sdk date = %q", signed.Header["X-Sdk-Date"])
	}
	if signed.Header["X-Security-Token"] != "test-token" || signed.Header["X-Project-ID"] != "test-project" {
		t.Fatalf("headers = %+v", signed.Header)
	}
	if signed.Header["Host"] != "maas.example.com" {
		t.Fatalf("host = %q", signed.Header["Host"])
	}
}

func TestSDKHMACRequiresAKSK(t *testing.T) {
	_, err := signSDKHMAC(signInput{
		Method: "POST",
		URL:    "https://maas.example.com/v2/chat/completions",
		Credential: signCredential{
			SecurityToken: "t",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "model_app_key") {
		t.Fatalf("err = %v", err)
	}
}

func TestSDKHMACCanonicalPathAndQuery(t *testing.T) {
	signed, err := signSDKHMAC(signInput{
		Method: "GET",
		URL:    "https://example.com/a%20b/c?z=1&a=2",
		Credential: signCredential{
			AccessKey:     "ak",
			SecretKey:     "sk",
			SecurityToken: "tok",
		},
		Now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if !strings.Contains(signed.Authorization, "SignedHeaders=host;x-sdk-date;x-security-token") {
		t.Fatalf("auth = %s", signed.Authorization)
	}
}
