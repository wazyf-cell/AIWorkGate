package main

// Account surface guards: this channel stores one credential in a sealed file and
// has no account-level "enabled" flag. PATCH must say so instead of acknowledging
// a change it did not make.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newAccountHarness(t *testing.T) (http.Handler, *server) {
	t.Helper()
	srv, err := newServer(serverConfig{ID: "cline", Token: "tk", DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	if err := srv.store.set(&clineCredential{AccessToken: "tok", RefreshToken: "rt"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	return srv.routes(), srv
}

func TestPatchAccountDoesNotFakeAnEnableState(t *testing.T) {
	handler, _ := newAccountHarness(t)
	req := httptest.NewRequest(http.MethodPatch, "/accounts/default", strings.NewReader(`{"enabled":false}`))
	req.Header.Set("Authorization", "Bearer tk")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("PATCH = %d %s, want 501", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != "not_supported" {
		t.Fatalf("error code = %q, want not_supported", body.Error.Code)
	}

	// It must not have changed anything: the account is still reported as enabled.
	get := httptest.NewRequest(http.MethodGet, "/accounts", nil)
	get.Header.Set("Authorization", "Bearer tk")
	list := httptest.NewRecorder()
	handler.ServeHTTP(list, get)
	var accounts struct {
		Accounts []struct {
			Enabled bool `json:"enabled"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &accounts); err != nil {
		t.Fatalf("decode accounts: %v", err)
	}
	if len(accounts.Accounts) != 1 || !accounts.Accounts[0].Enabled {
		t.Fatalf("PATCH disabled something it cannot persist: %s", list.Body.String())
	}
}
