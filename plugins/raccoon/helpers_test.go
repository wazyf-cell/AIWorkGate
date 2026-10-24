package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// doReq issues an authenticated request against h and returns the recorder.
func doReq(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// doReqNoAuth issues a request with no Authorization header, so the auth
// boundary can be observed.
func doReqNoAuth(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}
