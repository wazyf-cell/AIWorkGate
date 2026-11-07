package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"aiworkgate/internal/secrets"
	"aiworkgate/internal/webui"
)

// credentialSyncApp builds an App whose DPAPI store lives in a temp dir. On
// non-Windows test hosts protect/unprotect degrade to plain files (see
// dpapi_other.go), so SetExtra/GetExtra behave the same.
func credentialSyncApp(t *testing.T) *App {
	t.Helper()
	sec, err := secrets.Open(t.TempDir())
	if err != nil {
		t.Fatalf("secrets.Open: %v", err)
	}
	t.Cleanup(func() { _ = sec.Close() })
	return &App{secrets: sec}
}

func TestCredentialExtraKeyRoundTrip(t *testing.T) {
	key := credentialExtraKey("bridge-wb", "acc 1")
	plugin, account, ok := parseCredentialExtraKey(key)
	if !ok || plugin != "bridge-wb" || account != "acc 1" {
		t.Fatalf("parseCredentialExtraKey(%q) = %q,%q,%v", key, plugin, account, ok)
	}
	// Expiry records and foreign keys are not credentials.
	if _, _, ok := parseCredentialExtraKey(key + credentialExpiresSuffix); ok {
		t.Fatal("expires_at record must not parse as a credential")
	}
	if _, _, ok := parseCredentialExtraKey("notify.url"); ok {
		t.Fatal("a foreign extra key must not parse as a credential")
	}
	if _, _, ok := parseCredentialExtraKey("credential.solo"); ok {
		t.Fatal("a key without an account part must not parse")
	}
}

func TestSyncCredentialsStoresAndReturnsRedacted(t *testing.T) {
	app := credentialSyncApp(t)
	result, err := app.syncCredentials("bridge-wb", []webui.SyncCredentialItem{
		{AccountID: "acc-1", Secret: map[string]any{"access": "SECRET_A", "refresh": "SECRET_R"}, ExpiresAt: 1893456000},
	})
	if err != nil {
		t.Fatalf("syncCredentials: %v", err)
	}
	if result.PluginID != "bridge-wb" || result.Count != 1 || !result.Credentials[0].Present {
		t.Fatalf("result = %+v", result)
	}
	// The redacted view carries key names only.
	raw, _ := json.Marshal(result)
	if strings.Contains(string(raw), "SECRET_A") || strings.Contains(string(raw), "SECRET_R") {
		t.Fatalf("redacted view leaked the secret: %s", raw)
	}
	if !reflect.DeepEqual(result.Credentials[0].Keys, []string{"access", "refresh"}) {
		t.Fatalf("keys = %v", result.Credentials[0].Keys)
	}
	// The value really landed in the DPAPI store.
	stored, ok := app.secrets.GetExtra(credentialExtraKey("bridge-wb", "acc-1"))
	if !ok || !strings.Contains(stored, "SECRET_A") {
		t.Fatalf("stored credential missing: %q %v", stored, ok)
	}
	expRaw, ok := app.secrets.GetExtra(credentialExtraKey("bridge-wb", "acc-1") + credentialExpiresSuffix)
	if !ok || expRaw != "1893456000" {
		t.Fatalf("expires_at record = %q %v", expRaw, ok)
	}
}

func TestLocalSyncCredentialsCollectsPayload(t *testing.T) {
	app := credentialSyncApp(t)
	if _, err := app.syncCredentials("bridge-wb", []webui.SyncCredentialItem{
		{AccountID: "acc-1", Secret: map[string]any{"access": "A1"}, ExpiresAt: 100},
	}); err != nil {
		t.Fatalf("seed 1: %v", err)
	}
	if _, err := app.syncCredentials("bridge-trae", []webui.SyncCredentialItem{
		{AccountID: "acc-2", Secret: map[string]any{"access": "A2"}},
	}); err != nil {
		t.Fatalf("seed 2: %v", err)
	}
	// Foreign extra keys (notify.url) must not leak into the payload.
	if err := app.secrets.SetExtra("notify.url", "https://example.test/hook"); err != nil {
		t.Fatalf("seed foreign: %v", err)
	}

	all := app.localSyncCredentials("")
	if len(all) != 2 || len(all["bridge-wb"]) != 1 || len(all["bridge-trae"]) != 1 {
		t.Fatalf("all = %+v", all)
	}
	if all["bridge-wb"][0].ExpiresAt != 100 {
		t.Fatalf("expires carried = %d, want 100", all["bridge-wb"][0].ExpiresAt)
	}
	one := app.localSyncCredentials("bridge-trae")
	if len(one) != 1 || len(one["bridge-trae"]) != 1 {
		t.Fatalf("narrowed = %+v", one)
	}
	if got := len(app.localSyncCredentials("ghost")); got != 0 {
		t.Fatalf("unknown plugin payload count = %d, want 0", got)
	}
}

func TestRunCredentialsSyncDryRun(t *testing.T) {
	app := credentialSyncApp(t)
	if err := app.setCredentialSyncToken("syncSECRET"); err != nil {
		t.Fatalf("set token: %v", err)
	}
	if _, err := app.syncCredentials("bridge-wb", []webui.SyncCredentialItem{
		{AccountID: "acc-1", Secret: map[string]any{"access": "A1"}},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	result, err := app.runCredentialsSync(webui.CredentialsSyncRequest{Target: "user@server.example.com", DryRun: true})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !result.DryRun || result.Count != 1 || result.Sync != nil {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(result.Command, "ssh user@server.example.com") {
		t.Fatalf("command = %q", result.Command)
	}
	// The token must never appear in the displayed shape.
	if strings.Contains(result.Command, "syncSECRET") {
		t.Fatalf("dry run leaked the token: %q", result.Command)
	}
	if !strings.Contains(result.Command, "--data @-") {
		t.Fatalf("command shape incomplete: %q", result.Command)
	}
	// The remote command pattern proves the token would ride through stdin.
	if !strings.Contains(result.Command, "read -r AIWG_SYNC_TOKEN") {
		t.Fatalf("remote command must read the token from stdin: %q", result.Command)
	}
}

func TestRunCredentialsSyncSSHCommandStub(t *testing.T) {
	// A stub "ssh" on PATH records its argv and stdin, then answers with the
	// endpoint's (redacted) JSON, so the whole push chain runs without a host.
	stubDir := t.TempDir()
	logFile := filepath.ToSlash(filepath.Join(stubDir, "argv.txt"))
	inFile := filepath.ToSlash(filepath.Join(stubDir, "stdin.txt"))
	var stubPath string
	var script string
	if os.PathSeparator == '\\' {
		stubPath = filepath.Join(stubDir, "ssh.cmd")
		script = "@echo off\r\n" +
			"echo %* > \"" + logFile + "\"\r\n" +
			"more > \"" + inFile + "\"\r\n" +
			"echo {\"plugin_id\":\"bridge-wb\",\"count\":1,\"credentials\":[{\"account_id\":\"acc-1\",\"keys\":[\"access\"],\"present\":true}]}\r\n"
	} else {
		stubPath = filepath.Join(stubDir, "ssh")
		script = "#!/bin/sh\n" +
			"printf '%s\\n' \"$@\" > '" + logFile + "'\n" +
			"cat - > '" + inFile + "'\n" +
			"printf '{\"plugin_id\":\"bridge-wb\",\"count\":1,\"credentials\":[{\"account_id\":\"acc-1\",\"keys\":[\"access\"],\"present\":true}]}'\n"
	}
	if err := os.WriteFile(stubPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	app := credentialSyncApp(t)
	if err := app.setCredentialSyncToken("syncSECRET"); err != nil {
		t.Fatalf("set token: %v", err)
	}
	if _, err := app.syncCredentials("bridge-wb", []webui.SyncCredentialItem{
		{AccountID: "acc-1", Secret: map[string]any{"access": "A1"}},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	result, err := app.runCredentialsSync(webui.CredentialsSyncRequest{Target: "user@server.example.com"})
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if result.Sync == nil || result.Sync.Count != 1 || result.Sync.PluginID != "bridge-wb" {
		t.Fatalf("sync = %+v", result.Sync)
	}
	// argv: ["user@server.example.com", "<remote command>"]; the token must not
	// appear in any argv row.
	argvRaw, err := os.ReadFile(filepath.Join(stubDir, "argv.txt"))
	if err != nil {
		t.Fatalf("read argv: %v", err)
	}
	if strings.Contains(string(argvRaw), "syncSECRET") {
		t.Fatalf("token leaked into ssh argv: %s", argvRaw)
	}
	if !strings.Contains(string(argvRaw), "user@server.example.com") {
		t.Fatalf("target missing from argv: %s", argvRaw)
	}
	// stdin carries token line then the JSON body with the secret.
	stdinRaw, err := os.ReadFile(filepath.Join(stubDir, "stdin.txt"))
	if err != nil {
		t.Fatalf("read stdin: %v", err)
	}
	// The token rides the first stdin line ("\r" is what the Windows pipe
	// layer records; the remote `read -r` strips it).
	tokenLine := strings.SplitN(string(stdinRaw), "\n", 2)[0]
	if strings.TrimSpace(tokenLine) != "syncSECRET" {
		t.Fatalf("stdin must start with the token line: %q", tokenLine)
	}
	if !strings.Contains(string(stdinRaw), `"plugin_id":"bridge-wb"`) || !strings.Contains(string(stdinRaw), "A1") {
		t.Fatalf("stdin body incomplete: %s", stdinRaw)
	}
}

func TestRunCredentialsSyncFailures(t *testing.T) {
	app := credentialSyncApp(t)
	// Unconfigured token → the sentinel the endpoint maps to 503.
	if _, err := app.runCredentialsSync(webui.CredentialsSyncRequest{Target: "u@h", DryRun: true}); err == nil {
		t.Fatal("push without a token must fail")
	} else if err.Error() != webui.ErrSyncTokenMissing.Error() {
		t.Fatalf("err = %v, want ErrSyncTokenMissing", err)
	}
	if err := app.setCredentialSyncToken("syncSECRET"); err != nil {
		t.Fatalf("set token: %v", err)
	}
	// Bad target is refused before anything runs.
	if _, err := app.runCredentialsSync(webui.CredentialsSyncRequest{Target: "u@h; rm -rf /"}); err == nil {
		t.Fatal("shell metacharacters in the target must fail")
	}
	// No local credentials → success with count 0, nothing sent.
	result, err := app.runCredentialsSync(webui.CredentialsSyncRequest{Target: "u@h"})
	if err != nil || result.Count != 0 || result.Sync != nil {
		t.Fatalf("empty push = %+v, %v", result, err)
	}
	// Control characters in the token are refused at configuration time.
	if err := app.setCredentialSyncToken("bad\nTOKEN"); err == nil {
		t.Fatal("a newline in the token must be rejected")
	}
}

func TestSetCredentialSyncTokenStores(t *testing.T) {
	app := credentialSyncApp(t)
	if err := app.setCredentialSyncToken("  tok-1  "); err != nil {
		t.Fatalf("set: %v", err)
	}
	tok, ok := app.credentialSyncToken()
	if !ok || tok != "tok-1" {
		t.Fatalf("token = %q %v", tok, ok)
	}
	// Empty clears it (SetExtra semantics) and the reader reports unconfigured.
	if err := app.setCredentialSyncToken(""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, ok := app.credentialSyncToken(); ok {
		t.Fatal("token must read unconfigured after clearing")
	}
}

func TestCredentialSyncReceiveOverHTTP(t *testing.T) {
	// The full receive chain through the real webui handler wired by
	// webOptions: loopback + X-Sync-Token in, DPAPI extra rows out.
	sec, err := secrets.Open(t.TempDir())
	if err != nil {
		t.Fatalf("secrets.Open: %v", err)
	}
	t.Cleanup(func() { _ = sec.Close() })
	app := &App{secrets: sec}
	if err := app.setCredentialSyncToken("tok-http"); err != nil {
		t.Fatalf("set token: %v", err)
	}
	handler := webui.New(app.webOptions())
	body := `{"plugin_id":"bridge-wb","credentials":[{"account_id":"acc-9","secret":{"access":"SECRET_HTTP"},"expires_at":123}]}`
	request := httptest.NewRequest(http.MethodPost, "/api/internal/credentials", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Sync-Token", "tok-http")
	request.RemoteAddr = "127.0.0.1:41000"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "SECRET_HTTP") {
		t.Fatalf("response leaked the secret: %s", recorder.Body.String())
	}
	stored, ok := app.secrets.GetExtra(credentialExtraKey("bridge-wb", "acc-9"))
	if !ok || !strings.Contains(stored, "SECRET_HTTP") {
		t.Fatalf("credential not stored: %q %v", stored, ok)
	}
	// The stored credential feeds back into the push payload.
	payload := app.localSyncCredentials("bridge-wb")
	if len(payload["bridge-wb"]) != 1 || payload["bridge-wb"][0].AccountID != "acc-9" || payload["bridge-wb"][0].ExpiresAt != 123 {
		t.Fatalf("push payload = %+v", payload)
	}
	// A wrong token over the same handler is a 401 (guard exemption holds:
	// no session cookie was ever exchanged).
	bad := httptest.NewRequest(http.MethodPost, "/api/internal/credentials", strings.NewReader(body))
	bad.Header.Set("Content-Type", "application/json")
	bad.Header.Set("X-Sync-Token", "wrong")
	bad.RemoteAddr = "127.0.0.1:41001"
	badRec := httptest.NewRecorder()
	handler.ServeHTTP(badRec, bad)
	if badRec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d, want 401", badRec.Code)
	}
}
