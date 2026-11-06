package main

// OAuth round-trip tests: the token exchange, the PKCE verifier, the refresh
// rotation, and the missing-client-secret diagnosis. Google is unreachable from
// the build network, so the token endpoint is an httptest server; the authorize
// URL is still the real accounts.google.com one.

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// loginTestServer builds a server whose token and userinfo endpoints are mocks.
func loginTestServer(t *testing.T, tokenHandler http.HandlerFunc) (*server, http.Handler, *httptest.Server) {
	t.Helper()
	st, err := newStore(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	tokenSrv := httptest.NewServer(tokenHandler)
	t.Cleanup(tokenSrv.Close)
	srv := newServer(pluginConfig{ID: "gemini", Token: "tk"}, st)
	srv.client.tokenURL = tokenSrv.URL
	infoSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":"sub-1","email":"u@example.com"}`)
	}))
	t.Cleanup(infoSrv.Close)
	srv.client.userInfoURL = infoSrv.URL
	return srv, srv.routes(), tokenSrv
}

// startLogin begins a flow and returns its session id.
func startLogin(t *testing.T, handler http.Handler) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/accounts/login", strings.NewReader(`{"method":"oauth"}`))
	req.Header.Set("Authorization", "Bearer tk")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login start: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.SessionID == "" {
		t.Fatalf("login start body: %s", rec.Body.String())
	}
	return out.SessionID
}

func submit(t *testing.T, handler http.Handler, sid, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/accounts/login/"+sid+"/submit", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tk")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestUnknownLoginSessionNeverInheritsAStoredCredential(t *testing.T) {
	srv, handler, _ := loginTestServer(t, func(w http.ResponseWriter, r *http.Request) {})
	stored, _ := srv.store.get()
	stored.AccessToken = "ya29.from-an-earlier-login"
	if err := srv.store.set(stored); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/accounts/login/sid-that-was-never-issued", nil)
	req.Header.Set("Authorization", "Bearer tk")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	var out struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	if out.Status == "success" {
		t.Fatalf("unknown session reported success just because a credential exists: %s", rec.Body.String())
	}
}

func TestLoginStartExposesARealGoogleAuthorizeURLWithPKCE(t *testing.T) {
	_, handler, _ := loginTestServer(t, func(w http.ResponseWriter, r *http.Request) {})
	req := httptest.NewRequest(http.MethodPost, "/accounts/login", nil)
	req.Header.Set("Authorization", "Bearer tk")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	var out struct {
		URL    string `json:"url"`
		PKCE   bool   `json:"pkce"`
		Secret bool   `json:"client_secret_configured"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	u, err := url.Parse(out.URL)
	if err != nil {
		t.Fatalf("authorize url does not parse: %v", err)
	}
	if u.Scheme != "https" || u.Host != "accounts.google.com" || u.Path != "/o/oauth2/v2/auth" {
		t.Fatalf("authorize url = %s, want the real Google endpoint", out.URL)
	}
	q := u.Query()
	if q.Get("client_id") != clientID || q.Get("response_type") != "code" {
		t.Fatalf("authorize query = %v", q)
	}
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		t.Fatalf("PKCE challenge missing from authorize URL: %v", q)
	}
	if !out.PKCE {
		t.Fatalf("response should report pkce=true")
	}
	if !strings.HasPrefix(q.Get("redirect_uri"), "http://localhost:") || !strings.HasSuffix(q.Get("redirect_uri"), callbackPath) {
		t.Fatalf("redirect_uri = %q", q.Get("redirect_uri"))
	}
}

func TestExchangeCodePersistsAccountAndSendsTheVerifier(t *testing.T) {
	t.Setenv(clientSecretEnv, "test-secret")
	var form url.Values
	srv, handler, _ := loginTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		form, _ = url.ParseQuery(string(raw))
		io.WriteString(w, `{"access_token":"ya29.fresh","refresh_token":"1//rotated",
			"expires_in":3599,"token_type":"Bearer","scope":"openid",
			"id_token":"`+testJWT("sub-9", "nine@example.com")+`"}`)
	})
	// Capture the challenge the authorize URL advertised so it can be matched
	// against the verifier the token request carries.
	startReq := httptest.NewRequest(http.MethodPost, "/accounts/login", nil)
	startReq.Header.Set("Authorization", "Bearer tk")
	startRec := httptest.NewRecorder()
	handler.ServeHTTP(startRec, startReq)
	var started struct {
		SessionID string `json:"session_id"`
		URL       string `json:"url"`
	}
	_ = json.Unmarshal(startRec.Body.Bytes(), &started)
	u, _ := url.Parse(started.URL)
	challenge := u.Query().Get("code_challenge")

	rec := submit(t, handler, started.SessionID, `{"values":{"code":"http://localhost:1/oauth-callback?code=abc&state=x"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("submit: %d %s", rec.Code, rec.Body.String())
	}
	if got := form.Get("grant_type"); got != "authorization_code" {
		t.Fatalf("grant_type = %q", got)
	}
	if form.Get("code") != "abc" {
		t.Fatalf("code = %q, want the code extracted from the pasted URL", form.Get("code"))
	}
	if form.Get("client_secret") != "test-secret" {
		t.Fatalf("client_secret was not sent from the environment")
	}
	verifier := form.Get("code_verifier")
	if verifier == "" {
		t.Fatal("code_verifier missing: PKCE was advertised but not redeemed")
	}
	sum := sha256.Sum256([]byte(verifier))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
		t.Fatalf("verifier does not match the advertised challenge")
	}

	creds, ok := srv.store.get()
	if !ok {
		t.Fatal("a successful exchange must persist an account")
	}
	if creds.AccessToken != "ya29.fresh" || creds.RefreshToken != "1//rotated" {
		t.Fatalf("stored creds = access %q refresh %q", fingerprint(creds.AccessToken), fingerprint(creds.RefreshToken))
	}
	if creds.Email != "nine@example.com" || creds.Sub != "sub-9" {
		t.Fatalf("identity not read from id_token: %+v", creds)
	}
	if creds.Expiry == "" {
		t.Fatal("expiry was not stored")
	}
	if creds.Identity == nil || creds.Identity.DeviceID == "" {
		t.Fatal("a login must issue a device identity")
	}
	// Point quota at the mock too, so the listing below never touches the real
	// sandbox endpoint from a test.
	creds.EndpointBase = srv.client.tokenURL
	if err := srv.store.set(creds); err != nil {
		t.Fatalf("set endpoint for quota: %v", err)
	}

	rec = doGet(t, handler, "/accounts")
	var list struct {
		Accounts []struct {
			ID    string `json:"id"`
			Label string `json:"label"`
		} `json:"accounts"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Accounts) != 1 || list.Accounts[0].ID != defaultAccountID {
		t.Fatalf("GET /accounts after login = %s", rec.Body.String())
	}
}

func TestExchangeRefusalWithoutClientSecretIsActionable501(t *testing.T) {
	t.Setenv(clientSecretEnv, "")
	srv, handler, _ := loginTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":"invalid_client","error_description":"The OAuth client was not found."}`)
	})
	sid := startLogin(t, handler)
	rec := submit(t, handler, sid, `{"values":{"code":"abc"}}`)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), clientSecretEnv) {
		t.Fatalf("the 501 must name the missing setting: %s", rec.Body.String())
	}
	if _, ok := srv.store.get(); ok {
		t.Fatal("a refused exchange must not register an account")
	}
	// The poll must report the failure, not success.
	poll := doGet(t, handler, "/accounts/login/"+sid)
	if !strings.Contains(poll.Body.String(), `"failed"`) {
		t.Fatalf("poll = %s", poll.Body.String())
	}
}

func TestRefreshRotatesTokensAndPersistsThem(t *testing.T) {
	var form url.Values
	srv, handler, _ := loginTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		form, _ = url.ParseQuery(string(raw))
		io.WriteString(w, `{"access_token":"ya29.second","refresh_token":"1//second","expires_in":3600}`)
	})
	if err := srv.store.set(credentials{
		AccessToken: "ya29.first", RefreshToken: "1//first",
		Expiry: "2000-01-01T00:00:00Z", Email: "t@example.com",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/accounts/default/refresh", nil)
	req.Header.Set("Authorization", "Bearer tk")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"result":"ok"`) {
		t.Fatalf("refresh: %d %s", rec.Code, rec.Body.String())
	}
	if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "1//first" {
		t.Fatalf("refresh form = %v", form)
	}
	creds, _ := srv.store.get()
	if creds.AccessToken != "ya29.second" || creds.RefreshToken != "1//second" {
		t.Fatalf("tokens were not rotated: access %q refresh %q",
			fingerprint(creds.AccessToken), fingerprint(creds.RefreshToken))
	}
	if creds.Email != "t@example.com" {
		t.Fatalf("refresh dropped the identity: %+v", creds)
	}
	exp, known := creds.expiresAtMs()
	if !known || exp <= 0 {
		t.Fatalf("expiry not refreshed: %q", creds.Expiry)
	}
}

// doGet is a small authenticated GET helper.
func doGet(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer tk")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// testJWT builds an unsigned JWT with the two claims the plugin reads.
func testJWT(sub, email string) string {
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, _ := json.Marshal(map[string]string{"sub": sub, "email": email})
	return header + "." + enc.EncodeToString(payload) + "."
}

// Only a refused grant is the operator's mistake; a dead network is a 502.
func TestExchangeErrorStatusClassifies(t *testing.T) {
	if status, kind := exchangeErrorStatus(&grantRefusedError{msg: "invalid_grant"}); status != http.StatusBadRequest || kind != "invalid_credential" {
		t.Fatalf("refused grant -> %d/%s", status, kind)
	}
	if status, kind := exchangeErrorStatus(&oauthConfigError{msg: "no secret"}); status != http.StatusNotImplemented || kind != "client_secret_required" {
		t.Fatalf("config error -> %d/%s", status, kind)
	}
	if status, kind := exchangeErrorStatus(errors.New("dial tcp: timeout")); status != http.StatusBadGateway || kind != "upstream_error" {
		t.Fatalf("transport error -> %d/%s", status, kind)
	}
}
