package main

// The host's account page calls DELETE /accounts/{id} and PATCH /accounts/{id}.
// This plugin owns a single credential, so DELETE must clear it and PATCH must
// say plainly that the channel cannot be disabled instead of 404-ing.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAccountDeleteClearsTheOnlyCredential(t *testing.T) {
	srv, handler, _ := loginTestServer(t, func(w http.ResponseWriter, r *http.Request) {})
	if err := srv.store.set(credentials{AccessToken: "ya29.only"}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	do := func(method, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/accounts/default", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer tk")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	if rec := do(http.MethodDelete, ""); rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d body %s", rec.Code, rec.Body.String())
	}
	if srv.store.configured() {
		t.Fatal("the credential survived an account delete")
	}
	if rec := do(http.MethodPatch, `{"enabled":false}`); rec.Code != http.StatusNotImplemented {
		t.Fatalf("patch status = %d body %s, want 501 not_supported", rec.Code, rec.Body.String())
	}
}
