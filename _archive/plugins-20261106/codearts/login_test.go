package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type loginStartResp struct {
	SessionID    string       `json:"session_id"`
	Status       string       `json:"status"`
	Method       string       `json:"method"`
	URL          string       `json:"url"`
	Instructions string       `json:"instructions"`
	Fields       []loginField `json:"fields"`
	AccountID    string       `json:"account_id"`
	Error        string       `json:"error"`
}

type loginStatusResp struct {
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
	AccountID string `json:"account_id"`
	Error     string `json:"error"`
}

type loginSTS struct {
	mu           sync.Mutex
	forms        []url.Values
	challenge    string
	callbackPort string
}

func (s *loginSTS) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		s.mu.Lock()
		s.forms = append(s.forms, r.PostForm)
		s.mu.Unlock()
		if r.Header.Get("DPoP") == "" {
			t.Errorf("STS request missing DPoP")
		}
		if r.PostForm.Get("client_id") != "codearts-agent" || r.PostForm.Get("grant_type") != "authorization_code" {
			t.Errorf("STS form = %v", r.PostForm)
		}
		code := r.PostForm.Get("code")
		redirect := r.PostForm.Get("redirect_uri")
		u, err := url.Parse(redirect)
		if err != nil || u.Path != oauthRedirectPath {
			t.Errorf("redirect_uri = %q", redirect)
		} else {
			s.mu.Lock()
			s.callbackPort = u.Port()
			s.mu.Unlock()
		}
		verifier := r.PostForm.Get("code_verifier")
		sum := sha256.Sum256([]byte(verifier))
		challenge := base64.RawURLEncoding.EncodeToString(sum[:])
		s.mu.Lock()
		want := s.challenge
		s.mu.Unlock()
		if want != "" && challenge != want {
			t.Errorf("PKCE challenge mismatch: verifier %q gives %q, want %q", verifier, challenge, want)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"credentials": map[string]any{
				"access_key_id":     "AK-" + code,
				"secret_access_key": "SK-" + code,
				"security_token":    "ST-" + code,
				"expiration":        time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
			},
			"refresh_token": "RT-" + code,
			"domain_id":     "domain-" + code,
			"user_id":       "user-" + code,
			"user_name":     "Alice " + code,
		})
	}
}

func TestOAuthLoginLoopbackFlow(t *testing.T) {
	sts := &loginSTS{}
	stsSrv := httptest.NewServer(sts.handler(t))
	defer stsSrv.Close()
	ep := testEndpoints()
	ep.STS = stsSrv.URL
	_, h := newTestPlugin(t, func(o *Options) {
		o.Endpoints = ep
		o.OpenBrowser = func(string) error { return nil }
	})

	rec := doReq(t, h, http.MethodPost, "/accounts/login", reqOpts{body: `{"method":"oauth"}`})
	if rec.Code != http.StatusOK {
		t.Fatalf("login start status = %d, body %s", rec.Code, rec.Body.String())
	}
	start := decodeAs[loginStartResp](t, rec)
	if start.Status != "pending" || !strings.HasPrefix(start.SessionID, "ls_") || start.URL == "" {
		t.Fatalf("login start = %+v", start)
	}
	if start.Instructions != "在浏览器完成 CodeArts 登录" {
		t.Fatalf("instructions = %q", start.Instructions)
	}
	u, err := url.Parse(start.URL)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	q := u.Query()
	if q.Get("client_id") != "codearts-agent" || q.Get("code_challenge_method") != "SHA-256" ||
		q.Get("plugin-name") != "snap_AIIDE" || q.Get("plugin-version") != "5.2.0" ||
		q.Get("theme") != "2" || q.Get("locale") != "zh-cn" || q.Get("uri_scheme") != "codearts-agent" {
		t.Fatalf("authorize query = %v", q)
	}
	port := q.Get("port")
	portNum, _ := strconv.Atoi(port)
	if portNum < 10000 {
		t.Fatalf("callback port = %q", port)
	}
	if len(q.Get("ticket_id")) != 64 || len(q.Get("code_challenge")) != 43 {
		t.Fatalf("ticket/challenge length = %d/%d", len(q.Get("ticket_id")), len(q.Get("code_challenge")))
	}
	sts.mu.Lock()
	sts.challenge = q.Get("code_challenge")
	sts.mu.Unlock()

	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get("http://127.0.0.1:" + port + oauthRedirectPath + "?code=CODE1")
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("callback status = %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "login_succeed=true") {
		t.Fatalf("callback location = %q", loc)
	}
	sts.mu.Lock()
	gotPort := sts.callbackPort
	sts.mu.Unlock()
	if gotPort != port {
		t.Fatalf("STS redirect port = %q, want %q", gotPort, port)
	}

	rec = doReq(t, h, http.MethodGet, "/accounts/login/"+start.SessionID, reqOpts{})
	poll := decodeAs[loginStatusResp](t, rec)
	if poll.Status != "success" || !strings.HasPrefix(poll.AccountID, "acc_") {
		t.Fatalf("poll = %+v", poll)
	}
	accounts := listAccounts(t, h).Accounts
	if len(accounts) != 1 || accounts[0].ID != poll.AccountID || accounts[0].Label != "Alice CODE1" {
		t.Fatalf("accounts = %+v", accounts)
	}
}

func TestPasteCodeLoginFlow(t *testing.T) {
	sts := &loginSTS{}
	stsSrv := httptest.NewServer(sts.handler(t))
	defer stsSrv.Close()
	ep := testEndpoints()
	ep.STS = stsSrv.URL
	_, h := newTestPlugin(t, func(o *Options) { o.Endpoints = ep })

	rec := doReq(t, h, http.MethodPost, "/accounts/login", reqOpts{body: `{"method":"paste_code"}`})
	start := decodeAs[loginStartResp](t, rec)
	if start.Status != "need_input" || len(start.Fields) != 1 || start.Fields[0].Name != "code" {
		t.Fatalf("paste start = %+v", start)
	}
	u, _ := url.Parse(start.URL)
	sts.mu.Lock()
	sts.challenge = u.Query().Get("code_challenge")
	sts.mu.Unlock()

	rec = doReq(t, h, http.MethodPost, "/accounts/login/"+start.SessionID+"/submit",
		reqOpts{body: `{"values":{"code":"http://127.0.0.1:` + u.Query().Get("port") + `/oauth/callback?code=PASTE1"}}`})
	if rec.Code != http.StatusOK {
		t.Fatalf("submit status = %d, body %s", rec.Code, rec.Body.String())
	}
	sub := decodeAs[loginStatusResp](t, rec)
	if sub.Status != "success" || sub.AccountID == "" {
		t.Fatalf("submit = %+v", sub)
	}
	rec = doReq(t, h, http.MethodGet, "/accounts/login/"+start.SessionID, reqOpts{})
	if poll := decodeAs[loginStatusResp](t, rec); poll.Status != "success" || poll.AccountID != sub.AccountID {
		t.Fatalf("poll after paste = %+v", poll)
	}
	if accounts := listAccounts(t, h).Accounts; len(accounts) != 1 || accounts[0].ID != sub.AccountID {
		t.Fatalf("accounts = %+v", accounts)
	}

	// A second login for the same Huawei user updates the account in place.
	rec = doReq(t, h, http.MethodPost, "/accounts/login", reqOpts{body: `{"method":"paste_code"}`})
	start2 := decodeAs[loginStartResp](t, rec)
	u2, _ := url.Parse(start2.URL)
	sts.mu.Lock()
	sts.challenge = u2.Query().Get("code_challenge")
	sts.mu.Unlock()
	rec = doReq(t, h, http.MethodPost, "/accounts/login/"+start2.SessionID+"/submit", reqOpts{body: `{"values":{"code":"PASTE1"}}`})
	sub2 := decodeAs[loginStatusResp](t, rec)
	if sub2.Status != "success" || sub2.AccountID != sub.AccountID {
		t.Fatalf("repeat login = %+v (want %s)", sub2, sub.AccountID)
	}
	if accounts := listAccounts(t, h).Accounts; len(accounts) != 1 {
		t.Fatalf("repeat login duplicated account: %+v", accounts)
	}
}

func TestPasteCodeOneShot(t *testing.T) {
	stsSrv := httptest.NewServer((&loginSTS{}).handler(t))
	defer stsSrv.Close()
	ep := testEndpoints()
	ep.STS = stsSrv.URL
	_, h := newTestPlugin(t, func(o *Options) { o.Endpoints = ep })

	rec := doReq(t, h, http.MethodPost, "/accounts/login", reqOpts{body: `{"method":"paste_code","code":"ONESHOT"}`})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	start := decodeAs[loginStartResp](t, rec)
	if start.Status != "success" || !strings.HasPrefix(start.AccountID, "acc_") {
		t.Fatalf("one-shot paste = %+v", start)
	}
	if accounts := listAccounts(t, h).Accounts; len(accounts) != 1 || accounts[0].ID != start.AccountID {
		t.Fatalf("accounts = %+v", accounts)
	}
}

func TestLoginCancelAndExpiry(t *testing.T) {
	_, h := newTestPlugin(t, func(o *Options) {
		o.Endpoints = testEndpoints()
		o.LoginTTL = 1200 * time.Millisecond
	})

	rec := doReq(t, h, http.MethodPost, "/accounts/login", reqOpts{body: `{"method":"oauth"}`})
	start := decodeAs[loginStartResp](t, rec)
	if rec = doReq(t, h, http.MethodDelete, "/accounts/login/"+start.SessionID, reqOpts{}); rec.Code != http.StatusNoContent {
		t.Fatalf("cancel status = %d", rec.Code)
	}
	if rec = doReq(t, h, http.MethodGet, "/accounts/login/"+start.SessionID, reqOpts{}); rec.Code != http.StatusNotFound {
		t.Fatalf("poll after cancel status = %d", rec.Code)
	}

	rec = doReq(t, h, http.MethodPost, "/accounts/login", reqOpts{body: `{"method":"oauth"}`})
	expiring := decodeAs[loginStartResp](t, rec)
	deadline := time.Now().Add(3 * time.Second)
	var poll loginStatusResp
	for time.Now().Before(deadline) {
		rec = doReq(t, h, http.MethodGet, "/accounts/login/"+expiring.SessionID, reqOpts{})
		poll = decodeAs[loginStatusResp](t, rec)
		if poll.Status == "failed" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if poll.Status != "failed" || !strings.Contains(poll.Error, "超时") {
		t.Fatalf("expired login = %+v", poll)
	}
}

func TestCancelledSessionCannotCreateAnAccountLater(t *testing.T) {
	p, h := newTestPlugin(t, func(o *Options) { o.Endpoints = testEndpoints() })
	rec := doReq(t, h, http.MethodPost, "/accounts/login", reqOpts{body: `{"method":"oauth"}`})
	start := decodeAs[loginStartResp](t, rec)
	if rec := doReq(t, h, http.MethodDelete, "/accounts/login/"+start.SessionID, reqOpts{}); rec.Code != http.StatusNoContent {
		t.Fatalf("cancel status = %d", rec.Code)
	}
	s := &loginSession{ID: start.SessionID}
	cred := Credential{AccessKeyID: "AK-cancelled", SecretAccessKey: "SK-cancelled", SecurityToken: "token"}
	if _, err := p.completeLogin(s, cred); err == nil {
		t.Fatalf("a cancelled session still completed the login")
	}
	p.mu.Lock()
	accounts := len(p.accounts)
	p.mu.Unlock()
	if accounts != 0 {
		t.Fatalf("accounts created after cancel = %d, want 0", accounts)
	}
}

func TestLoginUnsupportedMethod(t *testing.T) {
	_, h := newTestPlugin(t, nil)
	rec := doReq(t, h, http.MethodPost, "/accounts/login", reqOpts{body: `{"method":"api_key"}`})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
	if env := decodeAs[errorEnvelope](t, rec); env.Error.Code != "unsupported_login_method" {
		t.Fatalf("error = %+v", env.Error)
	}
}

func TestPastedCallbackProblem(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		port     int
		wantPart string
	}{
		{"portal authorizationAvailable nested", "http://127.0.0.1:12345/oauth/callback?secret=abc&redirect=https%3A%2F%2Fcodearts.huaweicloud.com%2Fportal%2Flogin%3Ftheme%3Ddark%26authorizationAvailable%3Dfalse%26secret%3Dabc", 12345, "authorizationAvailable=false"},
		{"wrong port", "http://127.0.0.1:40000/oauth/callback?code=xyz", 12345, "端口 40000"},
		{"missing port", "http://127.0.0.1/oauth/callback?secret=abc", 12345, "端口 无"},
		{"matching callback", "http://127.0.0.1:12345/oauth/callback?code=xyz", 12345, ""},
		{"bare code", "abc123", 12345, ""},
	}
	for _, tc := range cases {
		got := pastedCallbackProblem(tc.raw, tc.port)
		if tc.wantPart == "" {
			if got != "" {
				t.Errorf("%s: problem = %q, want none", tc.name, got)
			}
			continue
		}
		if !strings.Contains(got, tc.wantPart) {
			t.Errorf("%s: problem = %q, want it to mention %q", tc.name, got, tc.wantPart)
		}
	}
}

func TestLoginStartReusesLiveSession(t *testing.T) {
	_, h := newTestPlugin(t, nil)
	first := doReq(t, h, http.MethodPost, "/accounts/login", reqOpts{body: `{"method":"paste_code"}`})
	second := doReq(t, h, http.MethodPost, "/accounts/login", reqOpts{body: `{"method":"paste_code"}`})
	a := decodeAs[map[string]any](t, first)
	b := decodeAs[map[string]any](t, second)
	if a["session_id"] != b["session_id"] {
		t.Fatalf("a second 开始登录 must not mint a new ticket: %v vs %v", a["session_id"], b["session_id"])
	}
	if a["url"] != b["url"] {
		t.Fatal("reused session must carry the same login URL")
	}
	if sid, ok := a["session_id"].(string); ok {
		doReq(t, h, http.MethodDelete, "/accounts/login/"+sid, reqOpts{})
	}
	third := doReq(t, h, http.MethodPost, "/accounts/login", reqOpts{body: `{"method":"paste_code"}`})
	c := decodeAs[map[string]any](t, third)
	if c["session_id"] == a["session_id"] {
		t.Fatal("after cancelling, a new session must be created")
	}
}

func TestLoginSubmitForeignSecretFailsFast(t *testing.T) {
	_, h := newTestPlugin(t, nil)
	start := decodeAs[map[string]any](t, doReq(t, h, http.MethodPost, "/accounts/login", reqOpts{body: `{"method":"paste_code"}`}))
	sid, _ := start["session_id"].(string)
	u, uerr := url.Parse(start["url"].(string))
	if uerr != nil {
		t.Fatalf("login url %v: %v", start["url"], uerr)
	}
	// Same callback port as this session, but a secret that no ticket knows.
	payload := `http://127.0.0.1:` + u.Query().Get("port") + `/oauth/callback?secret=deadbeef`
	s := time.Now()
	rec := doReq(t, h, http.MethodPost, "/accounts/login/"+sid+"/submit", reqOpts{
		body: submitBodyWithCode(t, payload),
	})
	if elapsed := time.Since(s); elapsed > 20*time.Second {
		t.Fatalf("submit took %v; a pasted secret must be polled with a short budget", elapsed)
	}
	body := decodeAs[map[string]any](t, rec)
	if body["status"] != "failed" {
		t.Fatalf("resp = %+v body %s", body, rec.Body.String())
	}
	if err, _ := body["error"].(string); !strings.Contains(err, "另一次") {
		t.Fatalf("error = %q, want it to explain the stale/foreign ticket", err)
	}
}

func submitBodyWithCode(t *testing.T, payload string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"values": map[string]string{"code": payload}})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
