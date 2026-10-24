package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newLoginTestServer builds a server whose upstream calls go to upstream (the
// stored BaseURL is the only redirection hook, which is also what a mirror uses).
func newLoginTestServer(t *testing.T, upstream string) (*server, http.Handler) {
	t.Helper()
	st, err := newStore(t.TempDir())
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	if upstream != "" {
		if err := st.set(credentials{AccessToken: "seed", BaseURL: upstream}); err != nil {
			t.Fatalf("seed store: %v", err)
		}
	}
	srv := &server{
		cfg:   pluginConfig{ID: "raccoon", APILevel: 2, Token: "tok", Port: "47801"},
		store: st, client: newUpstreamClient(), logins: newLoginManager(),
		egress: egressFile{Version: 1, Default: egressEntry{Mode: egressDirect}, Accounts: map[string]egressEntry{}},
	}
	return srv, srv.routes()
}

func startLogin(t *testing.T, h http.Handler, body string) (code int, out map[string]any) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/accounts/login", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func pollLogin(t *testing.T, h http.Handler, sid string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/accounts/login/"+sid, nil)
	r.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// fakeQRUpstream answers login_with_qrcode_code with the reference's envelope
// shape and user_info with the label shape, recording what it was asked.
type fakeQRUpstream struct {
	polls   []string
	devices []string
	reply   func(n int) string
	srv     *httptest.Server
}

func newFakeQRUpstream(t *testing.T, reply func(n int) string) *fakeQRUpstream {
	t.Helper()
	f := &fakeQRUpstream{reply: reply}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == qrLoginPath:
			var body struct {
				QRCode string `json:"qrcode_code"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.polls = append(f.polls, body.QRCode)
			if r.Header.Get("Authorization") != "" {
				t.Errorf("scan poll must be unauthenticated, got %q", r.Header.Get("Authorization"))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(f.reply(len(f.polls))))
		case r.URL.Path == userInfoPath:
			f.devices = append(f.devices, r.Header.Get("X-Client-Device-ID"))
			_, _ = w.Write([]byte(`{"code":0,"data":{"name":"小浣熊用户","office_identity":"org-9"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// TestQRStartIsActionableEntry is the M4 guard: the start answer must carry a
// link the user can open, the scan code, and instructions — not a bare pending.
func TestQRStartIsActionableEntry(t *testing.T) {
	// -1 makes every QR code expire: the poll then sees "canceled".
	fake := newFakeQRUpstream(t, func(int) string { return `{"code":0,"data":{"status":"pending"}}` })
	_, h := newLoginTestServer(t, fake.srv.URL)

	code, out := startLogin(t, h, `{"method":"oauth"}`)
	if code != http.StatusOK {
		t.Fatalf("start status = %d, want 200: %v", code, out)
	}
	url, _ := out["url"].(string)
	if !strings.HasPrefix(url, "http://127.0.0.1:47801/qr/") {
		t.Fatalf("url = %q, want the loopback QR page", url)
	}
	qr, _ := out["code"].(string)
	if len(qr) != 32 {
		t.Fatalf("code = %q, want a 32-hex scan code", qr)
	}
	payload, _ := out["qr_content"].(string)
	if payload != "https://xiaohuanxiong.com/login/mp?code="+qr+"&appname=%E5%95%86%E6%B1%A4%E5%B0%8F%E6%B5%A3%E7%86%8A%E5%AE%98%E7%BD%91" {
		t.Fatalf("qr_content = %q, want buildQrImageUrl's payload", payload)
	}
	if !strings.Contains(out["instructions"].(string), "扫") {
		t.Fatalf("instructions do not tell the user to scan: %v", out["instructions"])
	}
	if out["status"] != "pending" {
		t.Fatalf("status = %v, want pending", out["status"])
	}

	// The page the URL points at must really render the QR image.
	r := httptest.NewRequest(http.MethodGet, url[len("http://127.0.0.1:47801"):], nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<svg") {
		t.Fatalf("QR page = %d, body starts %q", rec.Code, truncate(rec.Body.String(), 80))
	}
}

// TestQRLoginFullFlow walks the reference's state machine end to end:
// pending -> logging -> success, then credentials and the account appear.
func TestQRLoginFullFlow(t *testing.T) {
	fake := newFakeQRUpstream(t, func(n int) string {
		if n < 2 {
			return `{"code":0,"message":"success","data":{"status":"pending"}}`
		}
		if n == 2 {
			return `{"code":0,"message":"success","data":{"status":"logging","expired_at":"2999-01-01T00:00:00Z"}}`
		}
		return `{"code":0,"message":"success","data":{"status":"success","access_token":"hdr.eyJleHAiOjE4OTM0NTYwMDB9.sig","refresh_token":"rt-1"}}`
	})
	srv, h := newLoginTestServer(t, fake.srv.URL)

	code, out := startLogin(t, h, `{"method":""}`)
	if code != http.StatusOK || out["method"] != "oauth" {
		t.Fatalf("start = %d %v, want 200 method oauth", code, out)
	}
	sid := out["session_id"].(string)

	if code, st := pollLogin(t, h, sid); code != 200 || st["status"] != "pending" {
		t.Fatalf("poll 1 = %d %v, want pending", code, st)
	}
	code, st := pollLogin(t, h, sid)
	if code != 200 || st["status"] != "pending" || !strings.Contains(st["note"].(string), "确认") {
		t.Fatalf("scanned poll = %d %v, want pending with a confirm hint", code, st)
	}
	code, st = pollLogin(t, h, sid)
	if code != 200 || st["status"] != "success" || st["account_id"] != "default" {
		t.Fatalf("final poll = %d %v, want success/default", code, st)
	}

	// Credentials really landed, with the JWT exp decoded and the label from
	// user_info — and user_info really received the device id.
	creds, ok := srv.store.get()
	if !ok || creds.AccessToken != "hdr.eyJleHAiOjE4OTM0NTYwMDB9.sig" || creds.RefreshToken != "rt-1" {
		t.Fatalf("stored credentials = %+v ok=%v", creds, ok)
	}
	if creds.ExpiresAt != "1893456000000" {
		t.Errorf("expires_at = %q, want the JWT exp in ms", creds.ExpiresAt)
	}
	if creds.Label != "小浣熊用户" || creds.OfficeIdentity != "org-9" {
		t.Errorf("label/org = %q/%q, want user_info's values", creds.Label, creds.OfficeIdentity)
	}
	if len(fake.devices) != 1 || fake.devices[0] != creds.Identity.DeviceID {
		t.Errorf("user_info X-Client-Device-ID = %v, want the stored device id %q", fake.devices, creds.Identity.DeviceID)
	}

	// The account the scheduler needs is there, with its identity block.
	rec := doReq(t, h, "/accounts")
	body := rec.Body.String()
	for _, want := range []string{`"id":"default"`, `"identity":{`, `"device_id":` + `"` + creds.Identity.DeviceID + `"`, `"user_agent":"` + userAgent + `"`} {
		if !strings.Contains(body, want) {
			t.Errorf("/accounts missing %s: %s", want, body)
		}
	}
	if !strings.Contains(body, `"refreshable":true`) {
		t.Errorf("/accounts lost refreshable: %s", body)
	}
}

// TestQRCanceledRefreshesCodeInsteadOfEnding covers the reference's behaviour on
// a stale phone-side code: mint a NEW code and keep the session alive so the page
// can re-render a scannable QR. Ending the session here is what produced the
// user-visible "扫码后立刻显示过期".
//
// The plugin's own five-minute deadline is the only thing that ends a session;
// that path keeps its record below.
func TestQRCanceledRefreshesCodeInsteadOfEnding(t *testing.T) {
	fake := newFakeQRUpstream(t, func(int) string { return `{"code":0,"data":{"status":"canceled"}}` })
	srv, h := newLoginTestServer(t, fake.srv.URL)

	_, out := startLogin(t, h, `{"method":"oauth"}`)
	sid := out["session_id"].(string)
	firstCode := out["code"].(string)

	code, st := pollLogin(t, h, sid)
	if code != http.StatusOK {
		t.Fatalf("canceled poll = %d %v, want 200 (the session must survive)", code, st)
	}
	if st["status"] != "pending" {
		t.Fatalf("canceled poll status = %v, want pending after a refresh", st["status"])
	}
	if msg, _ := st["note"].(string); !strings.Contains(msg, "已刷新") {
		t.Fatalf("note must tell the user the QR was refreshed; got %q", msg)
	}
	fresh, _ := st["code"].(string)
	if fresh == "" || fresh == firstCode {
		t.Fatalf("a canceled code must be replaced by a new one; got %q (was %q)", fresh, firstCode)
	}
	// The session must still be pollable, and it must now carry that fresh code.
	// Read the session BEFORE polling again: another poll would answer canceled
	// once more and mint yet another code, which is correct behaviour but makes
	// this assertion compare against a code the caller never saw.
	sess, ok := srv.logins.snapshot(sid)
	if !ok {
		t.Fatal("session disappeared after a canceled poll")
	}
	if sess.QRCode != fresh {
		t.Fatalf("session QRCode = %q, want the returned fresh code %q", sess.QRCode, fresh)
	}
	if code, _ := pollLogin(t, h, sid); code != http.StatusOK {
		t.Fatalf("session was dropped after a refresh; poll = %d", code)
	}

	// Local deadline is still the one thing that ends a session.
	srv2, h2 := newLoginTestServer(t, fake.srv.URL)
	_, out2 := startLogin(t, h2, `{"method":"oauth"}`)
	sid2 := out2["session_id"].(string)
	srv2.logins.update(sid2, func(s *loginSession) { s.ExpiresAt = time.Now().Add(-time.Second) })
	if code, st := pollLogin(t, h2, sid2); code != http.StatusGone || !strings.Contains(st["error"].(map[string]any)["message"].(string), "过期") {
		t.Fatalf("expired-by-deadline poll = %d %v, want 410 expired", code, st)
	}
}

// TestQRUpstreamExpiredAtDoesNotEndSession pins the bug that caused the report:
// the upstream's expired_at must never be read as an expiry verdict. The
// reference only carries it through for display, and an unobserved wire format
// (e.g. a relative "300") would otherwise be parsed as an instant in 1970.
func TestQRUpstreamExpiredAtDoesNotEndSession(t *testing.T) {
	for _, expiredAt := range []string{"300", "300000", "1970-01-01T00:00:00Z", "0"} {
		t.Run("expired_at="+expiredAt, func(t *testing.T) {
			fake := newFakeQRUpstream(t, func(int) string {
				return `{"code":0,"message":"success","data":{"status":"logging","expired_at":"` + expiredAt + `"}}`
			})
			_, h := newLoginTestServer(t, fake.srv.URL)
			_, out := startLogin(t, h, `{"method":"oauth"}`)
			sid := out["session_id"].(string)

			code, st := pollLogin(t, h, sid)
			if code != http.StatusOK {
				t.Fatalf("poll = %d %v; expired_at must not produce a 4xx", code, st)
			}
			if st["status"] != "pending" {
				t.Fatalf("status = %v, want pending while the user confirms on the phone", st["status"])
			}
			if msg, _ := st["note"].(string); !strings.Contains(msg, "已扫码") {
				t.Fatalf("a logging status must tell the user to confirm; got %q", msg)
			}
		})
	}
}

// TestQRUpstreamFailureStaysPending pins pollRaccoonQrLogin's tolerance: a
// network/parse error or a non-zero envelope must not kill a live scan.
func TestQRUpstreamFailureStaysPending(t *testing.T) {
	fake := newFakeQRUpstream(t, func(n int) string {
		if n == 1 {
			return `{"code":100003,"message":"服务繁忙"}`
		}
		return `not json at all`
	})
	_, h := newLoginTestServer(t, fake.srv.URL)
	_, out := startLogin(t, h, `{"method":"oauth"}`)
	sid := out["session_id"].(string)
	for i := 0; i < 2; i++ {
		if code, st := pollLogin(t, h, sid); code != 200 || st["status"] != "pending" {
			t.Fatalf("poll %d = %d %v, want 200 pending", i+1, code, st)
		}
	}
	// Unknown session ids are a 404, not a silent new login.
	if code, _ := pollLogin(t, h, "nope"); code != http.StatusNotFound {
		t.Fatalf("unknown session = %d, want 404", code)
	}
}

// TestQRLoginRejectsOtherMethods is the M3 guard: a method the plugin does not
// implement must be refused, never silently served by the QR path.
func TestQRLoginRejectsOtherMethods(t *testing.T) {
	_, h := newLoginTestServer(t, "")
	code, out := startLogin(t, h, `{"method":"local_state"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("local_state = %d, want 400", code)
	}
	msg := out["error"].(map[string]any)["message"].(string)
	if !strings.Contains(msg, "oauth") || !strings.Contains(msg, "admin/credentials") {
		t.Fatalf("400 must name the supported paths: %q", msg)
	}
}

// TestQRSubmitExplainsThereIsNothingToPaste keeps the UI's oauth fallback input
// honest instead of accepting a value that would be dropped.
func TestQRSubmitExplainsThereIsNothingToPaste(t *testing.T) {
	_, h := newLoginTestServer(t, "")
	_, out := startLogin(t, h, `{"method":"oauth"}`)
	sid := out["session_id"].(string)
	r := httptest.NewRequest(http.MethodPost, "/accounts/login/"+sid+"/submit", strings.NewReader(`{"values":{"code":"x"}}`))
	r.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "no_submit_needed") {
		t.Fatalf("submit = %d %s, want 400 no_submit_needed", rec.Code, rec.Body.String())
	}
	// Cancelling is a real DELETE, not a 404.
	r = httptest.NewRequest(http.MethodDelete, "/accounts/login/"+sid, nil)
	r.Header.Set("Authorization", "Bearer tok")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel = %d, want 200", rec.Code)
	}
}
