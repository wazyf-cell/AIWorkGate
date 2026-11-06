package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// newLoginServer builds a server whose store lives in a temp dir. The local
// state import and the upstream validation are stubbed: a unit test must never
// read the operator's real client state or call the real upstream.
func newLoginServer(t *testing.T) *server {
	t.Helper()
	dir := t.TempDir()
	st, err := newStore(dir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	s := &server{cfg: pluginConfig{ID: "zcode"}, store: st, client: newUpstreamClient()}
	s.importOverride = func(ctx context.Context) (localImport, error) {
		return localImport{Kind: credentialKindJWT, Secret: "tok-from-client", Label: "stub"}, nil
	}
	s.validateOverride = func(token string) error { return nil }
	return s
}

func post(t *testing.T, s *server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	switch {
	case strings.HasSuffix(path, "/submit"):
		s.handleLoginPoll(rec, req)
	case strings.HasSuffix(path, "/accounts/login"):
		s.handleLoginStart(rec, req)
	default:
		s.handleLoginPoll(rec, req)
	}
	return rec
}

// The reported method must be the one the plugin actually drives.
func TestLoginStartReportsRequestedMethod(t *testing.T) {
	s := newLoginServer(t)

	cases := []struct{ requested, want string }{
		{"local_state", "local_state"},
		{"", "local_state"},
		{"paste_code", "paste_code"},
	}
	for _, c := range cases {
		rec := post(t, s, "/accounts/login", `{"method":"`+c.requested+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("method %q -> status %d, want 200", c.requested, rec.Code)
		}
		var got map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got["method"] != c.want {
			t.Fatalf("requested %q -> method %v, want %q", c.requested, got["method"], c.want)
		}
	}
}

// An unsupported method must be refused, not silently rewritten.
func TestLoginStartRejectsUnsupportedMethod(t *testing.T) {
	s := newLoginServer(t)
	rec := post(t, s, "/accounts/login", `{"method":"oauth"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unsupported method", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "unsupported_method") {
		t.Fatalf("body = %s, want unsupported_method", rec.Body.String())
	}
}

// local_state must land the imported credential in the store and answer success
// with an account id, never with the secret itself.
func TestLocalStateImportStoresAccount(t *testing.T) {
	s := newLoginServer(t)
	rec := post(t, s, "/accounts/login", `{"method":"local_state"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["status"] != "success" || got["account_id"] != "default" {
		t.Fatalf("body = %s, want success/default", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "tok-from-client") {
		t.Fatalf("response leaked the token: %s", rec.Body.String())
	}
	creds, ok := s.store.get()
	if !ok || creds.JWT != "tok-from-client" {
		t.Fatalf("stored = %+v, want the imported token", creds)
	}
	if creds.DeviceMid == "" {
		t.Fatal("import did not assign a device id")
	}
}

// A rejected upstream credential is a readable 4xx and nothing is stored.
func TestLocalStateImportRejectedByUpstream(t *testing.T) {
	s := newLoginServer(t)
	s.importOverride = func(ctx context.Context) (localImport, error) {
		return localImport{}, &localStateError{Status: 400, Code: "local_state_rejected",
			Message: "上游拒绝了这个凭据（models HTTP 401）"}
	}
	rec := post(t, s, "/accounts/login", `{"method":"local_state"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "local_state_rejected") {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if s.store.configured() {
		t.Fatal("a rejected credential must not be stored")
	}
}

// A missing file is a 400 with the path, not a fake success.
func TestLocalStateImportMissingFile(t *testing.T) {
	s := newLoginServer(t)
	s.importOverride = nil
	t.Setenv("ZCODE_CREDENTIALS_FILE", t.TempDir()+`\absent.json`)
	rec := post(t, s, "/accounts/login", `{"method":"local_state"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "local_state_missing") {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "absent.json") {
		t.Fatalf("message must name the file: %s", rec.Body.String())
	}
}

// A corrupt file is a 400, not a panic or a fake success.
func TestLocalStateImportCorruptFile(t *testing.T) {
	s := newLoginServer(t)
	s.importOverride = nil
	path := t.TempDir() + `\credentials.json`
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("ZCODE_CREDENTIALS_FILE", path)
	rec := post(t, s, "/accounts/login", `{"method":"local_state"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "local_state_corrupt") {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

// The paste flow is only real if the submitted token reaches the sealed store.
func TestLoginSubmitStoresToken(t *testing.T) {
	s := newLoginServer(t)
	rec := post(t, s, "/accounts/login/sid-1/submit", `{"values":{"jwt":"tok-from-client"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("submit status = %d, body = %s", rec.Code, rec.Body.String())
	}
	creds, ok := s.store.get()
	if !ok || creds.JWT != "tok-from-client" {
		t.Fatalf("stored = %+v", creds)
	}
	if strings.Contains(rec.Body.String(), "tok-from-client") {
		t.Fatalf("submit response leaked the token: %s", rec.Body.String())
	}
}

// A typo must not be stored as a working login.
func TestLoginSubmitRejectedByUpstream(t *testing.T) {
	s := newLoginServer(t)
	s.validateOverride = func(token string) error { return errTestRejected }
	rec := post(t, s, "/accounts/login/sid-9/submit", `{"values":{"jwt":"typo"}}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_credential") {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if s.store.configured() {
		t.Fatal("a rejected token must not be stored")
	}
}

// paste_code mode accepts a full callback URL and extracts the code.
func TestLoginSubmitAcceptsCallbackURL(t *testing.T) {
	s := newLoginServer(t)
	rec := post(t, s, "/accounts/login/sid-2/submit",
		`{"values":{"code":"https://zcode.z.ai/cb?code=abc123&state=x"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	creds, _ := s.store.get()
	if creds.JWT != "abc123" {
		t.Fatalf("extracted code = %q, want abc123", creds.JWT)
	}
}

func TestLoginSubmitRejectsEmptyValues(t *testing.T) {
	s := newLoginServer(t)
	rec := post(t, s, "/accounts/login/sid-3/submit", `{"values":{}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an empty submit", rec.Code)
	}
}

// Polling before a submit must report pending, and success only once a token exists.
func TestLoginPollReflectsStoreState(t *testing.T) {
	s := newLoginServer(t)
	rec := post(t, s, "/accounts/login/sid-4", "")
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["status"] != "pending" {
		t.Fatalf("status = %v, want pending before submit", got["status"])
	}
	if err := s.store.set(credentials{JWT: "x"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	rec = post(t, s, "/accounts/login/sid-4", "")
	got = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["status"] != "success" {
		t.Fatalf("status = %v, want success after a token exists", got["status"])
	}
}

// Guard the wire shape the host expects from a login start.
func TestLoginStartDoesNotInventAuthorizeURL(t *testing.T) {
	s := newLoginServer(t)
	rec := post(t, s, "/accounts/login", `{"method":"paste_code"}`)
	if bytes.Contains(rec.Body.Bytes(), []byte("authorize_url")) {
		t.Fatalf("response invents an authorize_url the plugin never drives: %s", rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("fields")) {
		t.Fatalf("paste flow must describe its input fields: %s", rec.Body.String())
	}
}

// A configured credential must not make a fresh paste_code session look done:
// the UI closes the dialog on success, before the operator can paste anything.
func TestPasteSessionStaysPendingWhenACredentialAlreadyExists(t *testing.T) {
	s := newLoginServer(t)
	if err := s.store.set(credentials{JWT: "existing-token", Kind: credentialKindJWT, Label: "ZCode"}); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	rec := post(t, s, "/accounts/login", `{"method":"paste_code"}`)
	var start map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil {
		t.Fatalf("start body: %v", err)
	}
	sid, _ := start["session_id"].(string)
	if sid == "" {
		t.Fatal("no session id")
	}

	poll := httptest.NewRequest(http.MethodGet, "/accounts/login/"+sid, nil)
	rec = httptest.NewRecorder()
	s.handleLoginPoll(rec, poll)
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("poll body: %v", err)
	}
	if got["status"] != "pending" {
		t.Fatalf("poll = %v, want pending while the session waits for a paste", got["status"])
	}

	rec = post(t, s, "/accounts/login/"+sid+"/submit", `{"values":{"code":"fresh-token"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("submit = %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	s.handleLoginPoll(rec, httptest.NewRequest(http.MethodGet, "/accounts/login/"+sid, nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("poll body: %v", err)
	}
	if s.loginPending(sid) {
		t.Fatal("the session is still tracked after a successful submit")
	}
}

// Cancelling the paste dialog must settle the session as failed and must not
// touch the stored credential. Without the tombstone the next poll answered
// "pending" forever (and, with an account present, could answer success).
func TestCancelledPasteSessionStaysFailed(t *testing.T) {
	s := newLoginServer(t)
	if err := s.store.set(credentials{JWT: "existing-token", Kind: credentialKindJWT, Label: "ZCode"}); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	rec := post(t, s, "/accounts/login", `{"method":"paste_code"}`)
	var start map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil {
		t.Fatalf("start body: %v", err)
	}
	sid, _ := start["session_id"].(string)

	rec = httptest.NewRecorder()
	s.handleLoginPoll(rec, httptest.NewRequest(http.MethodDelete, "/accounts/login/"+sid, nil))
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("cancel body: %v", err)
	}
	if got["status"] != "failed" {
		t.Fatalf("cancel = %v, want failed", got["status"])
	}
	rec = httptest.NewRecorder()
	s.handleLoginPoll(rec, httptest.NewRequest(http.MethodGet, "/accounts/login/"+sid, nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("poll body: %v", err)
	}
	if got["status"] != "failed" {
		t.Fatalf("poll after cancel = %v, want failed", got["status"])
	}
	creds, _ := s.store.get()
	if creds.JWT != "existing-token" {
		t.Fatalf("cancel touched the stored credential: %q", creds.JWT)
	}
}

// A paste that arrives after the dialog was cancelled must not sign in. The
// code itself is valid, so only the tombstone can stop the late submit.
func TestCancelledPasteSessionRejectsSubmit(t *testing.T) {
	s := newLoginServer(t)
	rec := post(t, s, "/accounts/login", `{"method":"paste_code"}`)
	var start map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil {
		t.Fatalf("start body: %v", err)
	}
	sid, _ := start["session_id"].(string)
	if sid == "" {
		t.Fatal("no session id")
	}

	rec = httptest.NewRecorder()
	s.handleLoginPoll(rec, httptest.NewRequest(http.MethodDelete, "/accounts/login/"+sid, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel status = %d, body = %s", rec.Code, rec.Body.String())
	}

	rec = post(t, s, "/accounts/login/"+sid+"/submit", `{"values":{"code":"late-code"}}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "login_cancelled") {
		t.Fatalf("submit after cancel = %d %s, want 400 login_cancelled", rec.Code, rec.Body.String())
	}
	if s.store.configured() {
		t.Fatal("a cancelled session must not store an account")
	}
}
