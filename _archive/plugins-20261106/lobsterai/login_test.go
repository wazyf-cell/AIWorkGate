package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newFlowServer builds a server with a sealed store in a temp dir and a stubbed
// exchange, so no test touches the real portal.
func newFlowServer(t *testing.T) *server {
	t.Helper()
	st, err := newStore(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	s := &server{cfg: pluginConfig{ID: "lobsterai", Token: "tok"}, store: st, client: newUpstreamClient()}
	s.exchangeOverride = func(ctx context.Context, code string, session loginSession) (credentials, error) {
		return credentials{AccessToken: "at-" + code, RefreshToken: "rt", UUID: session.UUID,
			FirstKeyfrom: session.FirstKeyfrom, LatestKeyfrom: session.LatestKeyfrom,
			Nickname: "13800001111"}, nil
	}
	return s
}

// M1 guard: the host reads `url`. A start response that only carries login_url
// loses the link, which is exactly the defect this test pins down.
func TestLoginStartCarriesCanonicalURL(t *testing.T) {
	s := newFlowServer(t)
	rec := doReqToken(t, s.routes(), http.MethodPost, "/accounts/login", `{"method":"oauth"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	url, _ := got["url"].(string)
	if !strings.HasPrefix(url, "https://lobsterai.youdao.com/portal#/login?") {
		t.Fatalf("url = %q", url)
	}
	if got["login_url"] != url {
		t.Fatalf("login_url = %v, want the same value as url", got["login_url"])
	}
	if !strings.Contains(url, "redirect_uri=http%3A%2F%2F127.0.0.1") {
		t.Fatalf("url lacks the loopback redirect: %q", url)
	}
	if got["session_id"] == "" || got["session_id"] == nil {
		t.Fatal("session_id missing")
	}
	if _, ok := got["fields"]; !ok {
		t.Fatal("manual code fallback field missing")
	}
}

func TestUnknownLoginSessionDoesNotInheritCredential(t *testing.T) {
	s := newFlowServer(t)
	if err := s.store.set(credentials{AccessToken: "at-old"}); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	rec := doReqToken(t, s.routes(), http.MethodGet, "/accounts/login/never-issued", "")
	if strings.Contains(rec.Body.String(), `"status":"success"`) {
		t.Fatalf("unknown session inherited the stored credential: %s", rec.Body.String())
	}
}

func TestLoginStartRejectsUnknownMethod(t *testing.T) {
	s := newFlowServer(t)
	rec := doReqToken(t, s.routes(), http.MethodPost, "/accounts/login", `{"method":"cookie"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unsupported_method") {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

// The browser callback must complete the exchange itself (M4): before this the
// redirect landed on a route the plugin never served.
func TestAuthCallbackExchangesAndStores(t *testing.T) {
	s := newFlowServer(t)
	rec := doReqToken(t, s.routes(), http.MethodPost, "/accounts/login", `{"method":"oauth"}`)
	var start struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &start)

	cb := doReqNoAuth(t, s.routes(), http.MethodGet, "/auth/callback?code=CODE-1&state="+start.SessionID)
	if cb.Code != http.StatusOK {
		t.Fatalf("callback status = %d body = %s", cb.Code, cb.Body.String())
	}
	creds, ok := s.store.get()
	if !ok || creds.AccessToken != "at-CODE-1" {
		t.Fatalf("stored = %+v", creds)
	}
	if strings.Contains(cb.Body.String(), "at-CODE-1") {
		t.Fatal("callback page leaked the token")
	}
	// The poll the host runs must now report success.
	poll := doReqToken(t, s.routes(), http.MethodGet, "/accounts/login/"+start.SessionID, "")
	if !strings.Contains(poll.Body.String(), `"status":"success"`) {
		t.Fatalf("poll = %s", poll.Body.String())
	}
}

// A state that this process never issued is refused, not exchanged.
func TestAuthCallbackRejectsUnknownState(t *testing.T) {
	s := newFlowServer(t)
	cb := doReqNoAuth(t, s.routes(), http.MethodGet, "/auth/callback?code=CODE-2&state=forged")
	if cb.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body = %s", cb.Code, cb.Body.String())
	}
	if s.store.configured() {
		t.Fatal("a forged state must not store a credential")
	}
}

// The host's submit path is a real alternative to the browser callback.
func TestLoginSubmitExchangesCode(t *testing.T) {
	s := newFlowServer(t)
	rec := doReqToken(t, s.routes(), http.MethodPost, "/accounts/login/sid-1/submit",
		`{"values":{"code":"https://x/auth/callback?code=CODE-3&state=sid-1"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	creds, _ := s.store.get()
	if creds.AccessToken != "at-CODE-3" {
		t.Fatalf("token = %q", creds.AccessToken)
	}
}

func TestIdentityResetRotatesUUID(t *testing.T) {
	s := newFlowServer(t)
	if err := s.store.set(credentials{AccessToken: "at", RefreshToken: "rt", UUID: "uuid-old",
		FirstKeyfrom: "1", LatestKeyfrom: "2"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	rec := doReqToken(t, s.routes(), http.MethodPost, "/accounts/default/identity/reset", `{"mode":"request"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	after, _ := s.store.get()
	if after.UUID == "uuid-old" || after.UUID == "" {
		t.Fatalf("uuid not rotated: %q", after.UUID)
	}
	if after.LatestKeyfrom != "2" {
		t.Fatal("identity reset must not touch the frozen keyfrom tuple")
	}
	os := doReqToken(t, s.routes(), http.MethodPost, "/accounts/default/identity/reset", `{"mode":"os"}`)
	if os.Code != http.StatusBadRequest {
		t.Fatalf("os mode status = %d", os.Code)
	}
}

func TestAccountsExposeIdentity(t *testing.T) {
	s := newFlowServer(t)
	if err := s.store.set(credentials{AccessToken: "at", UUID: "uuid-1"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	rec := doReqToken(t, s.routes(), http.MethodGet, "/accounts", "")
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
	if len(body.Accounts) != 1 || body.Accounts[0].Identity == nil || body.Accounts[0].Identity.DeviceID != "uuid-1" {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestStrictEgressBlocksDirect(t *testing.T) {
	s := newFlowServer(t)
	// A credential is needed to get past the not_configured gate: this test is
	// about egress, and an unconfigured account would answer 503 first.
	if err := s.store.set(credentials{AccessToken: "at", RefreshToken: "rt", UUID: "uuid-1"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	s.egress.file = egressFile{Version: 1, Strict: true,
		Default: egressEntry{Mode: egressDirect}, Accounts: map[string]egressEntry{}}
	_, err := s.proxyFor("default")
	var em *egressMissingError
	if !errors.As(err, &em) {
		t.Fatalf("err = %v, want egressMissingError", err)
	}
	rec := doReqToken(t, s.routes(), http.MethodPost, "/v1/chat/completions", `{"model":"glm-5"}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "egress_missing") {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestReloadEgressReadsHostFile(t *testing.T) {
	s := newFlowServer(t)
	path := filepath.Join(t.TempDir(), "egress.json")
	body := `{"version":1,"strict":false,"default":{"mode":"proxy","url":"http://127.0.0.1:7"},"accounts":{}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	s.cfg.Egress = path
	if err := s.reloadEgress(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if proxy, _ := s.proxyFor("default"); proxy != "http://127.0.0.1:7" {
		t.Fatalf("proxy = %q", proxy)
	}
}

// A rejected auth code is the operator's mistake; the host must see 4xx rather
// than a 502 that reads as "plugin broken".
func TestRejectedAuthCodeIsClientError(t *testing.T) {
	s := newFlowServer(t)
	s.exchangeOverride = func(ctx context.Context, code string, session loginSession) (credentials, error) {
		return credentials{}, &exchangeRejectedError{Code: 40102, Msg: "code expired"}
	}
	rec := doReqToken(t, s.routes(), http.MethodPost, "/accounts/login/sid-1/submit",
		`{"values":{"code":"bad-code"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body = %s, want 400", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "40102") && !strings.Contains(rec.Body.String(), "code expired") {
		t.Fatalf("the upstream reason was dropped: %s", rec.Body.String())
	}
}

// A dialog opened to add a credential must not close itself just because an
// older credential exists, and cancelling must settle as failed.
func TestLoginDialogAndCancelIgnoreTheExistingCredential(t *testing.T) {
	s := newFlowServer(t)
	if err := s.store.set(credentials{AccessToken: "at-old"}); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	rec := doReqToken(t, s.routes(), http.MethodPost, "/accounts/login", `{"method":"oauth"}`)
	var start map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil {
		t.Fatalf("start body: %v", err)
	}
	sid, _ := start["session_id"].(string)
	rec = doReqToken(t, s.routes(), http.MethodGet, "/accounts/login/"+sid, "")
	var poll map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &poll); err != nil {
		t.Fatalf("poll body: %v", err)
	}
	if poll["status"] != "pending" {
		t.Fatalf("poll with an existing credential = %v, want pending", poll["status"])
	}
	rec = doReqToken(t, s.routes(), http.MethodDelete, "/accounts/login/"+sid, "")
	if err := json.Unmarshal(rec.Body.Bytes(), &poll); err != nil {
		t.Fatalf("cancel body: %v", err)
	}
	if poll["status"] != "failed" {
		t.Fatalf("cancel = %v, want failed", poll["status"])
	}
	rec = doReqToken(t, s.routes(), http.MethodGet, "/accounts/login/"+sid, "")
	_ = json.Unmarshal(rec.Body.Bytes(), &poll)
	if poll["status"] != "failed" {
		t.Fatalf("poll after cancel = %v, want failed", poll["status"])
	}
	if !s.store.configured() {
		t.Fatal("cancel dropped the stored credential")
	}
}

// A proxied answer must not be cut by a whole-request timeout: SSE bodies can
// outlive requestTimeout, which only bounds the wait for response headers.
func TestProxyClientHasNoWholeRequestTimeout(t *testing.T) {
	c := newUpstreamClient()
	c.transport = func(proxy string) *http.Transport {
		return &http.Transport{}
	}
	if got := c.clientFor("http://127.0.0.1:1").Timeout; got != 0 {
		t.Fatalf("proxied client Timeout = %v, want 0 (stream cut)", got)
	}
}
