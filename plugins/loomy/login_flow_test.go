package main

// End-to-end login tests against local stand-ins for open.weixin.qq.com and the
// 讯飞 account API. What is asserted is the part a user feels: a scan the phone
// confirmed ends with a sealed credential on disk and a "success" the host
// understands.
//
// The signing pair used here is a TEST value created by this file at runtime. No
// production credential exists in the repository: postSigned refuses to run
// without an operator-supplied pair, and the manifest only names the variables.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// withLoginEndpoints points the three network seams at local stubs for one test
// and restores the production values afterwards.
func withLoginEndpoints(t *testing.T, authPage, pollURL, accountURL string) {
	t.Helper()
	oldAuth, oldPoll, oldAccount := wechatAuthPageURL, wechatPollEndpoint, accountEndpoint
	wechatAuthPageURL = func(string) string { return authPage }
	wechatPollEndpoint = pollURL
	accountEndpoint = accountURL
	t.Cleanup(func() {
		wechatAuthPageURL, wechatPollEndpoint, accountEndpoint = oldAuth, oldPoll, oldAccount
	})
}

// fakeWechat answers the auth page and the long poll. reply decides the body of
// each successive long-poll call, so a test can walk waiting → scanned →
// confirmed.
type fakeWechat struct {
	uuid      string
	polls     int
	lastSeen  []string
	reply     func(n int) string
	pollSrv   *httptest.Server
	authSrv   *httptest.Server
	onLongPol func()
}

func newFakeWechat(t *testing.T, uuid string, reply func(n int) string) *fakeWechat {
	t.Helper()
	f := &fakeWechat{uuid: uuid, reply: reply}
	f.authSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<img src="/connect/qrcode/` + uuid + `"/>`))
	}))
	f.pollSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.polls++
		f.lastSeen = append(f.lastSeen, r.URL.Query().Get("last"))
		if f.onLongPol != nil {
			f.onLongPol()
		}
		_, _ = w.Write([]byte(f.reply(f.polls)))
	}))
	t.Cleanup(func() {
		f.authSrv.Close()
		f.pollSrv.Close()
	})
	return f
}

// fakeAccount answers the signed account endpoints with the reference's envelope
// shape and records the Authorization header so a test can prove the request was
// actually signed.
type fakeAccount struct {
	bind        int
	session     string
	userID      string
	gotAuth     []string
	gotBindAuth int
	gotSkip     int
	srv         *httptest.Server
}

func newFakeAccount(t *testing.T, bind int) *fakeAccount {
	t.Helper()
	f := &fakeAccount{bind: bind, session: strings.Repeat("ab", 16), userID: "123456789012345678"}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.gotAuth = append(f.gotAuth, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case pathBindAuth:
			f.gotBindAuth++
			_, _ = w.Write([]byte(`{"code":"000000","desc":"ok","data":{"bind":` +
				itoa(f.bind) + `,"rcode":"rc-1","nickname":"测试用户"}}`))
		case pathBindSkip:
			f.gotSkip++
			_, _ = w.Write([]byte(`{"code":"000000","desc":"ok","data":{"session":"` +
				f.session + `","userid":"` + f.userID + `"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	return "1"
}

// A confirmed scan with an already phone-bound WeChat must seal the session into
// the store and answer the host's success status.
func TestLoginPollConfirmedStoresSession(t *testing.T) {
	acct := newFakeAccount(t, 1)
	// 408 waiting first, then 405 with the wx_code.
	wx := newFakeWechat(t, "uuid-abc123", func(n int) string {
		if n == 1 {
			return "window.wx_errcode=408;"
		}
		return "window.wx_errcode=405; window.wx_code='WXCODE-1';"
	})
	withLoginEndpoints(t, wx.authSrv.URL, wx.pollSrv.URL, acct.srv.URL)

	s := newTestServer(t, xfyunSecret{AccessKeyID: "test-ak-id", AccessKeySecret: "test-ak-secret"})
	start := httptest.NewRecorder()
	s.handleLoginStart(start, httptest.NewRequest(http.MethodPost, "/accounts/login", strings.NewReader(`{"method":"oauth"}`)))
	if start.Code != http.StatusOK {
		t.Fatalf("start = %d, body %s", start.Code, start.Body.String())
	}
	var started struct {
		SessionID string `json:"session_id"`
		Code      string `json:"code"`
	}
	if err := json.Unmarshal(start.Body.Bytes(), &started); err != nil {
		t.Fatalf("decode start: %v (%s)", err, start.Body.String())
	}
	if started.Code != "uuid-abc123" {
		t.Fatalf("start code = %q, want the stub uuid", started.Code)
	}

	// First poll: the QR is still waiting, so nothing may be stored yet.
	rec := httptest.NewRecorder()
	s.handleLoginPoll(rec, httptest.NewRequest(http.MethodGet, "/accounts/login/"+started.SessionID, nil))
	if !strings.Contains(rec.Body.String(), `"pending"`) {
		t.Fatalf("first poll = %q, want pending", rec.Body.String())
	}
	if s.store.configured() {
		t.Fatal("a pending scan must not store a credential")
	}

	// Second poll: WeChat confirmed, so the signed exchange runs and stores.
	rec = httptest.NewRecorder()
	s.handleLoginPoll(rec, httptest.NewRequest(http.MethodGet, "/accounts/login/"+started.SessionID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("confirmed poll = %d, body %s", rec.Code, rec.Body.String())
	}
	var done struct {
		Status    string `json:"status"`
		AccountID string `json:"account_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &done); err != nil {
		t.Fatalf("decode poll: %v (%s)", err, rec.Body.String())
	}
	if done.Status != "success" {
		t.Fatalf("confirmed poll = %+v, want success; body %s", done, rec.Body.String())
	}
	if done.AccountID != defaultAccountID {
		t.Fatalf("account_id = %q, want %q", done.AccountID, defaultAccountID)
	}

	creds, ok := s.store.get()
	if !ok {
		t.Fatal("the confirmed scan stored no credential")
	}
	if creds.AccessToken != acct.session {
		t.Fatalf("stored token = %q, want the exchange's session", creds.AccessToken)
	}
	if creds.UserID != acct.userID {
		t.Fatalf("stored userid = %q, want %q", creds.UserID, acct.userID)
	}
	if creds.ExpiresAt == "" {
		t.Fatal("the stored credential needs an expires_at (login time + 14 days)")
	}
	// The exchange must be signed, and it must be the ONLY thing signed: the QR
	// steps need no secret at all.
	if len(acct.gotAuth) == 0 {
		t.Fatal("the 讯飞 exchange was not signed")
	}
	for _, h := range acct.gotAuth {
		if !strings.HasPrefix(h, "account test-ak-id:") {
			t.Fatalf("Authorization = %q, want account <accessKeyId>:<sig>", h)
		}
	}
	if acct.gotBindAuth != 1 || acct.gotSkip != 1 {
		t.Fatalf("bind/auth calls = %d, bind/skip calls = %d, want 1 and 1", acct.gotBindAuth, acct.gotSkip)
	}

	// A completed session keeps answering success instead of re-exchanging.
	rec = httptest.NewRecorder()
	s.handleLoginPoll(rec, httptest.NewRequest(http.MethodGet, "/accounts/login/"+started.SessionID, nil))
	if !strings.Contains(rec.Body.String(), `"success"`) {
		t.Fatalf("poll after success = %q, want success", rec.Body.String())
	}
	if acct.gotSkip != 1 {
		t.Fatalf("bind/skip ran %d times: a finished session must not re-exchange", acct.gotSkip)
	}
}

// An unbound WeChat (bind != 1) cannot be finished by this plugin: it needs a
// phone + SMS form the host's login dialog has no shape for. The user must get
// that reason, and nothing may be stored.
func TestLoginPollUnboundWechatReportsActionableFailure(t *testing.T) {
	acct := newFakeAccount(t, 0)
	wx := newFakeWechat(t, "uuid-unbound", func(int) string {
		return "window.wx_errcode=405; window.wx_code='WXCODE-2';"
	})
	withLoginEndpoints(t, wx.authSrv.URL, wx.pollSrv.URL, acct.srv.URL)

	s := newTestServer(t, xfyunSecret{AccessKeyID: "test-ak-id", AccessKeySecret: "test-ak-secret"})
	sess := s.logins.start("uuid-unbound", time.Now())

	rec := httptest.NewRecorder()
	s.handleLoginPoll(rec, httptest.NewRequest(http.MethodGet, "/accounts/login/"+sess.ID, nil))
	var out struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if out.Status != "failed" {
		t.Fatalf("status = %q, want failed", out.Status)
	}
	if !strings.Contains(out.Error, "绑定") {
		t.Fatalf("error = %q, want an explanation about the phone binding", out.Error)
	}
	if s.store.configured() {
		t.Fatal("an unbound scan must not store a credential")
	}
	if acct.gotSkip != 0 {
		t.Fatal("bind/skip must not run when bind != 1")
	}
}

// The long poll's progress code must be fed back as `last`, mirroring
// pollLoomyWechatOnce: dropping it makes WeChat re-report the same state.
func TestLoginPollSendsLastCode(t *testing.T) {
	acct := newFakeAccount(t, 1)
	wx := newFakeWechat(t, "uuid-lastcode", func(int) string {
		return "window.wx_errcode=408;"
	})
	withLoginEndpoints(t, wx.authSrv.URL, wx.pollSrv.URL, acct.srv.URL)

	s := newTestServer(t, xfyunSecret{AccessKeyID: "test-ak-id", AccessKeySecret: "test-ak-secret"})
	sess := s.logins.start("uuid-lastcode", time.Now())

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		s.handleLoginPoll(rec, httptest.NewRequest(http.MethodGet, "/accounts/login/"+sess.ID, nil))
	}
	if len(wx.lastSeen) != 2 {
		t.Fatalf("long polls = %d, want 2", len(wx.lastSeen))
	}
	if wx.lastSeen[0] != "" {
		t.Errorf("first poll sent last=%q, want empty", wx.lastSeen[0])
	}
	if wx.lastSeen[1] != "408" {
		t.Errorf("second poll sent last=%q, want the previous errcode 408", wx.lastSeen[1])
	}
}

// A transport failure on the long poll is "pending", never a failure: the
// reference does the same so a flaky network cannot kill a login the user has
// already completed on the phone.
func TestLoginPollNetworkErrorStaysPending(t *testing.T) {
	acct := newFakeAccount(t, 1)
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<img src="/connect/qrcode/uuid-neterr"/>`))
	}))
	defer authSrv.Close()
	withLoginEndpoints(t, authSrv.URL, deadURL, acct.srv.URL)

	s := newTestServer(t, xfyunSecret{AccessKeyID: "test-ak-id", AccessKeySecret: "test-ak-secret"})
	sess := s.logins.start("uuid-neterr", time.Now())
	rec := httptest.NewRecorder()
	s.handleLoginPoll(rec, httptest.NewRequest(http.MethodGet, "/accounts/login/"+sess.ID, nil))
	if !strings.Contains(rec.Body.String(), `"pending"`) {
		t.Fatalf("body = %q, want pending", rec.Body.String())
	}
}
