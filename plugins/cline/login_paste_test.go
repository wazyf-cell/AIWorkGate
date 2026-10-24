package main

// Paste-token login and session-lifecycle guards.

import (
	"net/http"
	"testing"
	"time"
)

func TestPasteCodeSubmitPersistsAndIsIdempotent(t *testing.T) {
	h := newLoginHarness(t)
	code, start := h.do(t, http.MethodPost, "/accounts/login", `{"method":"paste_code"}`)
	if code != http.StatusOK {
		t.Fatalf("start = %d %v", code, start)
	}
	sid, _ := start["session_id"].(string)
	if sid == "" {
		t.Fatal("paste_code start has no session id")
	}
	fields, _ := start["fields"].([]any)
	if len(fields) != 2 {
		t.Fatalf("fields = %v", start["fields"])
	}
	// Before submit the session is pending, never a fake success.
	if _, body := h.do(t, http.MethodGet, "/accounts/login/"+sid, ""); body["status"] != "pending" {
		t.Fatalf("pre-submit poll = %v", body)
	}
	code, body := h.do(t, http.MethodPost, "/accounts/login/"+sid+"/submit",
		`{"values":{"access_token":"workos:pasted-1","refresh_token":"rt-p"}}`)
	if code != http.StatusOK || body["status"] != "success" {
		t.Fatalf("submit = %d %v", code, body)
	}
	stored, _ := h.srv.store.get()
	if stored.AccessToken != "pasted-1" || stored.RefreshToken != "rt-p" {
		t.Fatalf("stored = %+v", stored)
	}
	// A second submit updates the same single account, it does not add one.
	if code, _ := h.do(t, http.MethodPost, "/accounts/login/"+sid+"/submit",
		`{"values":{"access_token":"pasted-2"}}`); code != http.StatusOK {
		t.Fatalf("second submit = %d", code)
	}
	_, accounts := h.do(t, http.MethodGet, "/accounts", "")
	if list, _ := accounts["accounts"].([]any); len(list) != 1 {
		t.Fatalf("accounts after resubmit = %v", accounts)
	}
	if stored, _ = h.srv.store.get(); stored.AccessToken != "pasted-2" {
		t.Fatalf("resubmit did not replace the token: %+v", stored)
	}
	// An unknown session must not claim success.
	if code, _ := h.do(t, http.MethodPost, "/accounts/login/bogus/submit",
		`{"values":{"access_token":"x"}}`); code != http.StatusNotFound {
		t.Fatalf("bogus submit status = %d, want 404", code)
	}
	if _, body := h.do(t, http.MethodGet, "/accounts/login/bogus", ""); body["status"] != "failed" {
		t.Fatalf("bogus poll = %v", body)
	}
}

// The models catalog answers anonymously, so a token probe that only lists
// models would accept garbage. Inference is the discriminator: a 401 must keep
// the credential out of the store.
func TestPasteCodeRejectsTokenTheUpstreamRefuses(t *testing.T) {
	h := newLoginHarness(t)
	h.mu.Lock()
	h.chatStatus = http.StatusUnauthorized
	h.mu.Unlock()
	_, start := h.do(t, http.MethodPost, "/accounts/login", `{"method":"paste_code"}`)
	sid, _ := start["session_id"].(string)
	code, body := h.do(t, http.MethodPost, "/accounts/login/"+sid+"/submit",
		`{"values":{"access_token":"workos:bogus"}}`)
	if code != http.StatusBadRequest {
		t.Fatalf("submit = %d %v, want 400", code, body)
	}
	if h.srv.store.configured() {
		t.Fatal("a refused token was stored")
	}
}

func TestAbandonedPasteSessionIsSwept(t *testing.T) {
	s := &server{logins: map[string]*loginSession{
		"stale":  {method: "paste_code", status: "pending", createdAt: time.Now().Add(-16 * time.Minute)},
		"fresh":  {method: "paste_code", status: "pending", createdAt: time.Now()},
		"device": {method: "oauth", status: "pending", createdAt: time.Now().Add(-16 * time.Minute)},
	}}
	s.sweepLogins()
	if _, ok := s.logins["stale"]; ok {
		t.Fatalf("an abandoned paste session survived the sweep")
	}
	if _, ok := s.logins["fresh"]; !ok {
		t.Fatalf("a fresh paste session was swept")
	}
	if _, ok := s.logins["device"]; !ok {
		t.Fatalf("a pending device flow was swept")
	}
}
