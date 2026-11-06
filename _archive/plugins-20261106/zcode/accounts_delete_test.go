package main

// The host's account page calls DELETE /accounts/{id} and PATCH /accounts/{id}.
// ZCode stores one credential, so DELETE must clear it and PATCH must answer
// 501 instead of pretending the channel can be disabled.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAccountDeleteClearsTheOnlyCredential(t *testing.T) {
	s := newLoginServer(t)
	if err := s.store.set(credentials{JWT: "tok-only", Label: "x"}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	do := func(method, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/accounts/default", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.handleAccountSub(rec, req)
		return rec
	}
	if rec := do(http.MethodDelete, ""); rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d body %s", rec.Code, rec.Body.String())
	}
	if s.store.configured() {
		t.Fatal("the credential survived an account delete")
	}
	if rec := do(http.MethodPatch, `{"enabled":false}`); rec.Code != http.StatusNotImplemented {
		t.Fatalf("patch status = %d body %s, want 501 not_supported", rec.Code, rec.Body.String())
	}
}
