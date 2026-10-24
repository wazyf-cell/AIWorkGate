package main

// Poll-surface tests: what GET /accounts/login/{sid} and DELETE on it answer.
// The vocabulary is the host's (pending | success | failed); "ok"/"expired"
// leave the login dialog spinning, so they are asserted against.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// An unknown session is a readable "failed", which is the status the host
// understands; a bare 404 would surface as "登录等待超时".
func TestLoginPollUnknownSessionFails(t *testing.T) {
	s := newTestServer(t, xfyunSecret{AccessKeyID: "id", AccessKeySecret: "secret"})
	rec := httptest.NewRecorder()
	s.handleLoginPoll(rec, httptest.NewRequest(http.MethodGet, "/accounts/login/nope", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with a failed status", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"status":"failed"`) {
		t.Fatalf("body = %q, want status failed", rec.Body.String())
	}
}

// A session past its window reports failed and is dropped, so a stale QR does
// not stay pending forever.
func TestLoginPollExpiredSession(t *testing.T) {
	s := newTestServer(t, xfyunSecret{AccessKeyID: "id", AccessKeySecret: "secret"})
	sess := s.logins.start("uuidvalue", time.Now().Add(-10*time.Minute))
	rec := httptest.NewRecorder()
	s.handleLoginPoll(rec, httptest.NewRequest(http.MethodGet, "/accounts/login/"+sess.ID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"status":"failed"`) {
		t.Fatalf("body = %q, want failed", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "过期") {
		t.Fatalf("body = %q, want the expiry reason", rec.Body.String())
	}
	if _, ok := s.logins.get(sess.ID); ok {
		t.Fatal("an expired session must be removed")
	}
}

// A finished session reports success with the account id the host should adopt.
func TestLoginPollCompletedSessionReportsSuccess(t *testing.T) {
	s := newTestServer(t, xfyunSecret{AccessKeyID: "id", AccessKeySecret: "secret"})
	sess := s.logins.start("uuidvalue", time.Now())
	s.logins.update(sess.ID, func(ls *loginSession) { ls.Status = "success" })
	rec := httptest.NewRecorder()
	s.handleLoginPoll(rec, httptest.NewRequest(http.MethodGet, "/accounts/login/"+sess.ID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var out struct {
		Status    string `json:"status"`
		AccountID string `json:"account_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if out.Status != "success" || out.AccountID != defaultAccountID {
		t.Fatalf("poll = %+v, want success/%s", out, defaultAccountID)
	}
}

// DELETE is the host cancelling an abandoned flow: the session must be gone, so
// a later poll cannot resurrect it.
func TestLoginPollDeleteCancelsSession(t *testing.T) {
	s := newTestServer(t, xfyunSecret{AccessKeyID: "id", AccessKeySecret: "secret"})
	sess := s.logins.start("uuidvalue", time.Now())
	rec := httptest.NewRecorder()
	s.handleLoginPoll(rec, httptest.NewRequest(http.MethodDelete, "/accounts/login/"+sess.ID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if _, ok := s.logins.get(sess.ID); ok {
		t.Fatal("a cancelled session must be removed")
	}
	rec = httptest.NewRecorder()
	s.handleLoginPoll(rec, httptest.NewRequest(http.MethodGet, "/accounts/login/"+sess.ID, nil))
	if !strings.Contains(rec.Body.String(), `"status":"failed"`) {
		t.Fatalf("poll after cancel = %q, want failed", rec.Body.String())
	}
}

// A non-GET/DELETE verb on a session is refused rather than silently treated as
// a poll.
func TestLoginPollRejectsBadMethod(t *testing.T) {
	s := newTestServer(t, xfyunSecret{AccessKeyID: "id", AccessKeySecret: "secret"})
	rec := httptest.NewRecorder()
	s.handleLoginPoll(rec, httptest.NewRequest(http.MethodPut, "/accounts/login/abc", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}
