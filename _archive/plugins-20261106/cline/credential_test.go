package main

// Credential projection and store guards: what a register/refresh response may
// look like, how the expiry is interpreted, and what lands in the sealed file.

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildCredentialProjection(t *testing.T) {
	// The envelope may be the payload itself or {"data":{}}; accessToken may be
	// spelled either way. All four combinations must land on the same result.
	payloads := []map[string]any{
		{"accessToken": "at", "refreshToken": "rt", "accountId": "acc", "email": "a@b.c"},
		{"data": map[string]any{"accessToken": "at", "refreshToken": "rt", "accountId": "acc", "email": "a@b.c"}},
		{"access_token": "at", "refresh_token": "rt", "account_id": "acc", "email": "a@b.c"},
		{"data": map[string]any{"access_token": "at", "refresh_token": "rt", "account_id": "acc", "email": "a@b.c"}},
	}
	for i, payload := range payloads {
		cred := buildCredential(payload)
		if cred.AccessToken != "at" {
			t.Fatalf("case %d: access token = %q", i, cred.AccessToken)
		}
		if cred.RefreshToken != "rt" || cred.AccountID != "acc" || cred.Email != "a@b.c" {
			t.Fatalf("case %d: %+v", i, cred)
		}
	}
}

func TestBuildCredentialStoresTokenBare(t *testing.T) {
	// A register response that already carries the prefix must not be stored with
	// it, or the next bearerValue call would produce workos:workos:...
	cred := buildCredential(map[string]any{"accessToken": "workos:at", "refreshToken": "rt"})
	if cred.AccessToken != "at" {
		t.Fatalf("stored token = %q, want bare", cred.AccessToken)
	}
	if bearerValue(cred.AccessToken) != "workos:at" {
		t.Fatalf("re-rendered bearer = %q", bearerValue(cred.AccessToken))
	}
}

func TestBuildCredentialReadsUserInfoAndNames(t *testing.T) {
	cred := buildCredential(map[string]any{
		"accessToken": "at",
		"userInfo": map[string]any{
			"clineUserId": "user_1", "email": "x@y.z",
			"firstName": "Ada", "lastName": "Lovelace",
		},
	})
	if cred.AccountID != "user_1" || cred.Email != "x@y.z" {
		t.Fatalf("userInfo not projected: %+v", cred)
	}
	if cred.label() != "Ada Lovelace" {
		t.Fatalf("label = %q", cred.label())
	}
}

func TestTimestampMsHandlesSecondsAndMillis(t *testing.T) {
	// Seconds (below year 2001 in ms) are scaled up.
	if got := timestampMs(map[string]any{"expiresAt": float64(1700000000)}, "expiresAt"); got != 1700000000000 {
		t.Fatalf("seconds -> %d", got)
	}
	// Milliseconds pass through.
	if got := timestampMs(map[string]any{"expires_at": float64(1700000000000)}, "expiresAt", "expires_at"); got != 1700000000000 {
		t.Fatalf("millis -> %d", got)
	}
	// An RFC3339 string parses.
	if got := timestampMs(map[string]any{"expire_time": "2026-10-22T00:00:00Z"}, "expire_time"); got != 1792627200000 {
		t.Fatalf("rfc3339 -> %d", got)
	}
	if got := timestampMs(map[string]any{}, "expiresAt"); got != 0 {
		t.Fatalf("absent -> %d, want 0", got)
	}
}

func TestCredentialExpiryTreatsUnknownAsLive(t *testing.T) {
	c := clineCredential{AccessToken: "at"}
	// An unknown expiry must never be reported as expired: that would force a
	// pointless refresh and could strand a good token behind a failing endpoint.
	if c.expired(9_999_999_999_999) {
		t.Fatal("an unknown expiry must not count as expired")
	}
	c.ExpireTime = 1000
	if !c.expired(2000) {
		t.Fatal("a passed expiry must count as expired")
	}
	if c.expired(500) {
		t.Fatal("a future expiry must not count as expired")
	}
}

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, err := newStore(dir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if st.configured() {
		t.Fatal("a fresh store must not claim to be configured")
	}
	cred := &clineCredential{
		AccessToken:  "workos:token-abc",
		RefreshToken: "rt-1",
		ExpireTime:   1792627200000,
		AccountID:    "acc_1",
		Nickname:     "Ada",
	}
	if err := st.set(cred); err != nil {
		t.Fatalf("set: %v", err)
	}
	stored, ok := st.get()
	if !ok {
		t.Fatal("credential not readable after set")
	}
	// The prefix must be stripped on the way in, so the wire value is built once.
	if stored.AccessToken != "token-abc" {
		t.Fatalf("stored token = %q, want bare", stored.AccessToken)
	}
	if !stored.refreshable() {
		t.Fatal("a stored refresh token must report refreshable")
	}
	// Reopening must read the sealed file back.
	again, err := newStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	restored, ok := again.get()
	if !ok || restored.AccessToken != "token-abc" || restored.RefreshToken != "rt-1" {
		t.Fatalf("restored = %+v/%v", restored, ok)
	}
	if restored.ExpireTime != 1792627200000 {
		t.Fatalf("expiry lost: %d", restored.ExpireTime)
	}
}

func TestStoreRejectsEmptyAccessToken(t *testing.T) {
	st, err := newStore(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	// Writing an empty token would silently wipe a working login.
	if err := st.set(&clineCredential{AccessToken: "  "}); err == nil {
		t.Fatal("expected an error for an empty access token")
	}
}

func TestStoreFileIsSealed(t *testing.T) {
	dir := t.TempDir()
	st, err := newStore(dir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	const token = "super-secret-token-value-123"
	if err := st.set(&clineCredential{AccessToken: token, RefreshToken: "rt-secret"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	raw, err := readFileLimited(filepath.Join(dir, accountsFile), 1<<20)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(raw), token) || strings.Contains(string(raw), "rt-secret") {
		t.Fatal("the credential file contains a secret in plaintext")
	}
	if st.sealKind != "dpapi-user" {
		t.Fatalf("sealKind = %q, want dpapi-user on Windows", st.sealKind)
	}
}

func TestFingerprintNeverEchoesTheToken(t *testing.T) {
	tag := fingerprint("workos:abcdef123456")
	if strings.Contains(tag, "abcdef123456") {
		t.Fatalf("fingerprint leaks the token: %q", tag)
	}
	if !strings.HasPrefix(tag, "sha256:") {
		t.Fatalf("fingerprint = %q", tag)
	}
	// The prefix must not affect the fingerprint, or re-login looks like a
	// different account.
	if fingerprint("workos:abc") != fingerprint("abc") {
		t.Fatal("the workos: prefix changed the fingerprint")
	}
}
