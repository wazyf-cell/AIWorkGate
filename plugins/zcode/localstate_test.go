package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var errTestRejected = errors.New("stub: upstream rejected the credential")

// sealForTest produces a real enc:v1 envelope with the same scheme the official
// client uses, so the decryptor is exercised against real AES-256-GCM output.
func sealForTest(t *testing.T, plain, secret string) string {
	t.Helper()
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("nonce: %v", err)
	}
	sum := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("gcm: %v", err)
	}
	out := gcm.Seal(nil, nonce, []byte(plain), nil)
	ct, tag := out[:len(out)-gcm.Overhead()], out[len(out)-gcm.Overhead():]
	enc := base64.RawURLEncoding.EncodeToString
	return envelopePrefix + enc(nonce) + "." + enc(tag) + "." + enc(ct)
}

func TestDecryptCredentialRoundTrip(t *testing.T) {
	secret := "test-secret"
	env := sealForTest(t, "hello-world", secret)
	got, err := decryptCredential(env, secret)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if got != "hello-world" {
		t.Fatalf("decrypt = %q", got)
	}
	if _, err := decryptCredential(env, "wrong-secret"); err == nil {
		t.Fatal("decrypt with the wrong key must fail")
	}
	if plain, err := decryptCredential("plain-value", secret); err != nil || plain != "plain-value" {
		t.Fatalf("a plain value must pass through, got %q %v", plain, err)
	}
}

// readLocalCredentials must reject a truncated envelope, never return a partial
// map that a caller could mistake for a login.
func TestReadLocalCredentialsRejectsBadEnvelope(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	body, _ := json.Marshal(map[string]string{"zcodejwttoken": "enc:v1:only-one-part"})
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("ZCODE_CREDENTIALS_FILE", path)
	t.Setenv(credEnvName, "test-secret")
	_, err := readLocalCredentials()
	var lse *localStateError
	if !errors.As(err, &lse) || lse.Code != "local_state_unreadable" {
		t.Fatalf("err = %v, want local_state_unreadable", err)
	}
}

// The api-key entry wins over the desktop JWT: it is the route that can serve
// chat unattended.
func TestPickLocalCredentialPrefersAPIKey(t *testing.T) {
	env := map[string]string{
		"zcodejwttoken": "jwt-value",
		"account-provider:coding-plan:account:bigmodel-individual-coding-plan:account:42:api-key": "k.s",
		"oauth:bigmodel:user_info": `{"displayName":"Tester"}`,
	}
	imp, err := pickLocalCredential(env)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if imp.Kind != credentialKindAPIKey || imp.Secret != "k.s" {
		t.Fatalf("picked %+v", imp)
	}
	if imp.Label != "Tester" {
		t.Fatalf("label = %q, want the user display name", imp.Label)
	}
	if !strings.Contains(imp.Source, "coding-plan") {
		t.Fatalf("source = %q", imp.Source)
	}
}

func TestIdentityResetRotatesDeviceMid(t *testing.T) {
	s := newLoginServer(t)
	if err := s.store.set(credentials{JWT: "tok", Label: "x"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	before, _ := s.store.get()

	req := httptest.NewRequest(http.MethodPost, "/accounts/default/identity/reset", strings.NewReader(`{"mode":"request"}`))
	rec := httptest.NewRecorder()
	s.handleAccountSub(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	after, _ := s.store.get()
	if after.DeviceMid == before.DeviceMid {
		t.Fatal("device id was not rotated")
	}
	if after.JWT != before.JWT {
		t.Fatal("identity reset must not touch the credential")
	}

	req = httptest.NewRequest(http.MethodPost, "/accounts/default/identity/reset", strings.NewReader(`{"mode":"os"}`))
	rec = httptest.NewRecorder()
	s.handleAccountSub(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unsupported_identity_mode") {
		t.Fatalf("os mode status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestAccountsExposeIdentity(t *testing.T) {
	s := newLoginServer(t)
	if err := s.store.set(credentials{JWT: "tok"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/accounts", nil)
	rec := httptest.NewRecorder()
	s.handleAccounts(rec, req)
	var body struct {
		Accounts []struct {
			Identity *struct {
				DeviceID string `json:"device_id"`
			} `json:"identity"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Accounts) != 1 || body.Accounts[0].Identity == nil || body.Accounts[0].Identity.DeviceID == "" {
		t.Fatalf("identity missing: %s", rec.Body.String())
	}
}

// Strict egress without a proxy binding must refuse the request instead of
// silently going direct.
func TestStrictEgressBlocksDirect(t *testing.T) {
	s := newLoginServer(t)
	s.egress.file = egressFile{Version: 1, Strict: true,
		Default: egressEntry{Mode: egressDirect}, Accounts: map[string]egressEntry{}}
	_, err := s.proxyFor("default")
	var em *egressMissingError
	if !errors.As(err, &em) {
		t.Fatalf("err = %v, want egressMissingError", err)
	}

	s.egress.file = egressFile{Version: 1,
		Default: egressEntry{Mode: egressProxy, URL: "http://127.0.0.1:9"}, Accounts: map[string]egressEntry{}}
	proxy, err := s.proxyFor("default")
	if err != nil || proxy != "http://127.0.0.1:9" {
		t.Fatalf("proxy = %q err = %v", proxy, err)
	}
}

func TestReloadEgressReadsHostFile(t *testing.T) {
	s := newLoginServer(t)
	path := filepath.Join(t.TempDir(), "egress.json")
	body := `{"version":1,"strict":false,"default":{"mode":"proxy","url":"http://127.0.0.1:7"},"accounts":{"default":{"mode":"direct"}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	s.cfg.Egress = path
	if err := s.reloadEgress(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if proxy, _ := s.proxyFor("default"); proxy != "" {
		t.Fatalf("account override must win, got %q", proxy)
	}
	if proxy, _ := s.proxyFor("other"); proxy != "http://127.0.0.1:7" {
		t.Fatalf("default binding = %q", proxy)
	}
	s.now = func() time.Time { return time.Unix(0, 0) }
}
