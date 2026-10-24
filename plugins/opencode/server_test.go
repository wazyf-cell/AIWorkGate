package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newLoginTestServer(t *testing.T) *server {
	t.Helper()
	st, err := newStore(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	return &server{store: st, logins: map[string]loginSession{}}
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return out
}

// An api_key session must not report success before the user submits: the UI
// closes the dialog on success, so an early success would make the paid-key
// field unusable.
func TestApiKeySessionStaysPendingUntilSubmit(t *testing.T) {
	s := newLoginTestServer(t)

	rec := httptest.NewRecorder()
	s.handleLoginStart(rec, httptest.NewRequest(http.MethodPost, "/accounts/login",
		strings.NewReader(`{"method":"api_key"}`)))
	start := decodeBody(t, rec)
	sid, _ := start["session_id"].(string)
	if sid == "" || start["status"] != "pending" {
		t.Fatalf("start = %v, want pending with a session id", start)
	}

	rec = httptest.NewRecorder()
	s.handleLoginPoll(rec, httptest.NewRequest(http.MethodGet, "/accounts/login/"+sid, nil))
	if got := decodeBody(t, rec)["status"]; got != "pending" {
		t.Fatalf("poll before submit = %v, want pending", got)
	}

	rec = httptest.NewRecorder()
	s.handleLoginPoll(rec, httptest.NewRequest(http.MethodPost, "/accounts/login/"+sid+"/submit",
		strings.NewReader(`{"values":{"api_key":""}}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("empty submit = %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	s.handleLoginPoll(rec, httptest.NewRequest(http.MethodGet, "/accounts/login/"+sid, nil))
	if got := decodeBody(t, rec)["status"]; got != "success" {
		t.Fatalf("poll after submit = %v, want success", got)
	}
}

func TestCancelFailsTheLoginSession(t *testing.T) {
	s := newLoginTestServer(t)
	rec := httptest.NewRecorder()
	s.handleLoginStart(rec, httptest.NewRequest(http.MethodPost, "/accounts/login",
		strings.NewReader(`{"method":"api_key"}`)))
	sid, _ := decodeBody(t, rec)["session_id"].(string)

	rec = httptest.NewRecorder()
	s.handleLoginPoll(rec, httptest.NewRequest(http.MethodDelete, "/accounts/login/"+sid, nil))
	if got := decodeBody(t, rec)["status"]; got != "failed" {
		t.Fatalf("cancel = %v, want failed", got)
	}
	if s.loginPending(sid) {
		t.Fatal("the cancelled session is still tracked")
	}
}

// Cancelling the dialog is not "log out": a key the user stored earlier must
// survive, and the session must keep reporting failed instead of falling back
// to the anonymous lane's success.
func TestCancelKeepsStoredCredential(t *testing.T) {
	s := newLoginTestServer(t)
	if err := s.store.set(credentials{APIKey: "sk-keep-me", Label: "paid"}); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	rec := httptest.NewRecorder()
	s.handleLoginStart(rec, httptest.NewRequest(http.MethodPost, "/accounts/login",
		strings.NewReader(`{"method":"api_key"}`)))
	sid, _ := decodeBody(t, rec)["session_id"].(string)

	rec = httptest.NewRecorder()
	s.handleLoginPoll(rec, httptest.NewRequest(http.MethodDelete, "/accounts/login/"+sid, nil))
	if got := decodeBody(t, rec)["status"]; got != "failed" {
		t.Fatalf("cancel = %v, want failed", got)
	}
	if got := s.store.current().APIKey; got != "sk-keep-me" {
		t.Fatalf("cancel wiped the stored credential: %q", got)
	}

	rec = httptest.NewRecorder()
	s.handleLoginPoll(rec, httptest.NewRequest(http.MethodGet, "/accounts/login/"+sid, nil))
	if got := decodeBody(t, rec)["status"]; got != "failed" {
		t.Fatalf("poll after cancel = %v, want failed", got)
	}
}
