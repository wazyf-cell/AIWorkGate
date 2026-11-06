package main

// Paste-token and refresh tests for MiniMax. They pin the second half of the
// login contract: POST /accounts/login/{sid}/submit stores a credential and the
// poll endpoint then reports success; POST /accounts/default/refresh renews it
// with the reference's exact form.

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestMiniMaxPasteCodeSubmitAndIdempotency(t *testing.T) {
	jwt := testJWT("acct-p", 1792713600)
	h := newMMHarness(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("paste_code must not poll upstream")
	})
	code, start := h.do(t, http.MethodPost, "/accounts/login", `{"method":"paste_code"}`)
	if code != http.StatusOK || start["status"] != "pending" {
		t.Fatalf("start = %d %v", code, start)
	}
	sid, _ := start["session_id"].(string)
	if fields, _ := start["fields"].([]any); len(fields) != 2 {
		t.Fatalf("fields = %v", start["fields"])
	}
	if _, body := h.do(t, http.MethodGet, "/accounts/login/"+sid, ""); body["status"] != "pending" {
		t.Fatalf("pre-submit poll = %v", body)
	}
	code, body := h.do(t, http.MethodPost, "/accounts/login/"+sid+"/submit",
		`{"values":{"access_token":"`+jwt+`","refresh_token":"rt-p"}}`)
	if code != http.StatusOK || body["status"] != "success" || body["account_id"] != "acct-p" {
		t.Fatalf("submit = %d %v", code, body)
	}
	stored, _ := h.srv.store.get()
	if stored.AccessToken != jwt || stored.RefreshToken != "rt-p" || stored.ExpiresAt != "1792713600000" {
		t.Fatalf("stored = %+v", stored)
	}

	// A second submit replaces the same single account. An opaque (non-JWT) token
	// has no readable expiry, so none is invented.
	if code, _ := h.do(t, http.MethodPost, "/accounts/login/"+sid+"/submit",
		`{"values":{"access_token":"opaque-token"}}`); code != http.StatusOK {
		t.Fatalf("resubmit = %d", code)
	}
	stored, _ = h.srv.store.get()
	if stored.AccessToken != "opaque-token" || stored.ExpiresAt != "" {
		t.Fatalf("resubmit stored = %+v", stored)
	}
	_, accounts := h.do(t, http.MethodGet, "/accounts", "")
	if list, _ := accounts["accounts"].([]any); len(list) != 1 {
		t.Fatalf("accounts after resubmit = %v", accounts)
	}

	if code, _ := h.do(t, http.MethodPost, "/accounts/login/bogus/submit",
		`{"values":{"access_token":"x"}}`); code != http.StatusNotFound {
		t.Fatalf("bogus submit = %d, want 404", code)
	}
	if _, body := h.do(t, http.MethodGet, "/accounts/login/bogus", ""); body["status"] != "failed" {
		t.Fatalf("bogus poll = %v", body)
	}
}

func TestMiniMaxSubmitIsRejectedOnOAuthSession(t *testing.T) {
	h := newMMHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, grantBody(testJWT("acct", 1792713600), "rt"))
	})
	_, start := h.do(t, http.MethodPost, "/accounts/login", `{"method":"oauth"}`)
	sid, _ := start["session_id"].(string)
	if code, _ := h.do(t, http.MethodPost, "/accounts/login/"+sid+"/submit",
		`{"values":{"access_token":"x"}}`); code != http.StatusBadRequest {
		t.Fatalf("oauth submit = %d, want 400", code)
	}
}

func TestMiniMaxLoginRejectsUnknownMethod(t *testing.T) {
	h := newMMHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	code, body := h.do(t, http.MethodPost, "/accounts/login", `{"method":"cookie"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %v", code, body)
	}
}

func TestMiniMaxRefreshUsesTheReferenceForm(t *testing.T) {
	oldJWT := testJWT("acct-r", 1792713600)
	newJWT := testJWT("acct-r", 1792800000)
	h := newMMHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.PostForm.Get("grant_type") == "refresh_token" {
			_, _ = io.WriteString(w, grantBody(newJWT, "rt-new"))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"authorization_pending"}`)
	})
	if err := h.srv.store.set(credentials{
		AccessToken: oldJWT, RefreshToken: "rt-old", ExpiresAt: "1792713600000", AccountID: "acct-r",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	code, body := h.do(t, http.MethodPost, "/accounts/default/refresh", "")
	if code != http.StatusOK || body["result"] != "ok" || body["expires_at"] != float64(1792800000) {
		t.Fatalf("refresh = %d %v", code, body)
	}
	stored, _ := h.srv.store.get()
	if stored.AccessToken != newJWT || stored.RefreshToken != "rt-new" {
		t.Fatalf("stored = %+v", stored)
	}
	h.mu.Lock()
	var form string
	for i := len(h.forms) - 1; i >= 0; i-- {
		if h.forms[i].Get("grant_type") == "refresh_token" {
			form = h.forms[i].Encode()
			break
		}
	}
	h.mu.Unlock()
	for _, want := range []string{"grant_type=refresh_token", "refresh_token=rt-old", "client_id=mcode-public", "scope=agent.default", "audience=agent-backend"} {
		if !strings.Contains(form, want) {
			t.Fatalf("refresh form %q missing %q", form, want)
		}
	}

	// A grant that fails the reference's token_type check must be refused, and
	// the working credential must survive.
	if err := h.srv.store.set(credentials{AccessToken: oldJWT, RefreshToken: "rt-old"}); err != nil {
		t.Fatalf("reseed: %v", err)
	}
	bad := newMMHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"x","refresh_token":"y","token_type":"mac","expires_in":3600,"scope":"agent.default"}`)
	})
	if err := bad.srv.store.set(credentials{AccessToken: oldJWT, RefreshToken: "rt-old"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, body = bad.do(t, http.MethodPost, "/accounts/default/refresh", "")
	if body["result"] != "fail" || !strings.Contains(body["message"].(string), "token_type") {
		t.Fatalf("bad refresh = %v", body)
	}
	if stored, _ := bad.srv.store.get(); stored.AccessToken != oldJWT {
		t.Fatalf("a rejected refresh overwrote the credential: %+v", stored)
	}
}
