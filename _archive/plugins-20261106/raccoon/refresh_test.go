package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRefreshRotatesTokenInPlace pins refreshRaccoonCredential: the stored token
// is replaced, an absent refresh_token keeps the old one, and the exp is redone.
func TestRefreshRotatesTokenInPlace(t *testing.T) {
	var gotBody map[string]string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != refreshPath || r.Method != http.MethodPost {
			t.Errorf("refresh called %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"access_token":"new.eyJleHAiOjE5MDAwMDAwMDB9.sig"}}`))
	}))
	defer upstream.Close()

	srv, h := newLoginTestServer(t, upstream.URL)
	if err := srv.store.set(credentials{AccessToken: "old", RefreshToken: "rt-old", BaseURL: upstream.URL}); err != nil {
		t.Fatalf("set: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/accounts/default/refresh", nil)
	r.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh = %d %s", rec.Code, rec.Body.String())
	}
	if gotBody["refresh_token"] != "rt-old" {
		t.Fatalf("upstream got refresh_token %q, want the stored one", gotBody["refresh_token"])
	}
	creds, _ := srv.store.get()
	if creds.AccessToken != "new.eyJleHAiOjE5MDAwMDAwMDB9.sig" {
		t.Fatalf("access token not replaced: %+v", creds)
	}
	if creds.RefreshToken != "rt-old" {
		t.Fatalf("refresh token should be kept when the response omits it, got %q", creds.RefreshToken)
	}
	if creds.ExpiresAt != "1900000000000" {
		t.Fatalf("expires_at = %q, want the new JWT exp", creds.ExpiresAt)
	}
}

// TestRefreshUsesTheAccountEgress is the egress guard for 续期: the account is
// bound to a proxy, so the refresh POST must leave through that proxy exactly
// like chat and the catalog do. A refresh that calls clientFor("") would reach
// the upstream directly while the user believes the account is proxied.
func TestRefreshUsesTheAccountEgress(t *testing.T) {
	var gotPath, gotMethod string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		_, _ = w.Write([]byte(`{"code":0,"data":{"access_token":"new.eyJleHAiOjE5MDAwMDAwMDB9.sig"}}`))
	}))
	defer upstream.Close()

	hits := 0
	proxy := newProxyRecorder(t, &hits)
	srv, h := newLoginTestServer(t, upstream.URL)
	if err := srv.store.set(credentials{AccessToken: "old", RefreshToken: "rt", BaseURL: upstream.URL}); err != nil {
		t.Fatalf("set: %v", err)
	}
	srv.egress.Accounts["default"] = egressEntry{Mode: egressProxy, URL: proxy.URL}

	r := httptest.NewRequest(http.MethodPost, "/accounts/default/refresh", nil)
	r.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh = %d %s", rec.Code, rec.Body.String())
	}
	if hits != 1 {
		t.Fatalf("proxy saw %d refresh requests, want 1 (refresh bypassed the egress binding)", hits)
	}
	if gotMethod != http.MethodPost || gotPath != refreshPath {
		t.Fatalf("proxied refresh = %s %s, want POST %s", gotMethod, gotPath, refreshPath)
	}
}

// TestRefreshHonoursStrictEgress: strict mode refuses a direct account on every
// upstream call, refresh included. Refreshing directly would be the same leak.
func TestRefreshHonoursStrictEgress(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("strict refresh reached the upstream directly: %s %s", r.Method, r.URL.Path)
		_, _ = w.Write([]byte(`{"code":0,"data":{"access_token":"x"}}`))
	}))
	defer upstream.Close()

	srv, h := newLoginTestServer(t, upstream.URL)
	if err := srv.store.set(credentials{AccessToken: "old", RefreshToken: "rt", BaseURL: upstream.URL}); err != nil {
		t.Fatalf("set: %v", err)
	}
	srv.egress.Strict = true

	r := httptest.NewRequest(http.MethodPost, "/accounts/default/refresh", nil)
	r.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "egress_missing") {
		t.Fatalf("strict direct refresh = %d %s, want 403 egress_missing", rec.Code, rec.Body.String())
	}
}

// TestRefreshDeadLoginIsReadable: 200003 means the user has to log in again, and
// the answer must say so instead of a generic 502.
func TestRefreshDeadLoginIsReadable(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":200003,"message":"token expired"}`))
	}))
	defer upstream.Close()

	srv, h := newLoginTestServer(t, upstream.URL)
	if err := srv.store.set(credentials{AccessToken: "old", RefreshToken: "rt", BaseURL: upstream.URL}); err != nil {
		t.Fatalf("set: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/accounts/default/refresh", nil)
	r.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "重新登录") {
		t.Fatalf("dead login refresh = %d %s, want 401 + 重新登录", rec.Code, rec.Body.String())
	}
	// No refresh token at all: refused before spending a round trip.
	srv2, h2 := newLoginTestServer(t, upstream.URL)
	if err := srv2.store.set(credentials{AccessToken: "old", BaseURL: upstream.URL}); err != nil {
		t.Fatalf("set: %v", err)
	}
	rec = httptest.NewRecorder()
	h2.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-refresh-token = %d, want 401", rec.Code)
	}
}
