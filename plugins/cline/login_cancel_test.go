package main

// Cancellation guard: DELETE /accounts/login/{sid} must stop the flow for real.
// The device poll can already be waiting on WorkOS when the operator cancels, so
// the success branch has to re-check the stop signal before storing anything.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCancelledDeviceLoginDoesNotStoreCredential(t *testing.T) {
	h := newLoginHarness(t)
	pollStarted := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, deviceAuthorizationPath):
			_, _ = io.WriteString(w, deviceAuthBody)
		case strings.HasSuffix(r.URL.Path, deviceAuthenticatePath):
			once.Do(func() { close(pollStarted) })
			<-release
			_, _ = io.WriteString(w, `{"access_token":"at-1","refresh_token":"rt-1"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mock.Close()
	h.srv.client.workOs = mock.URL

	_, start := h.do(t, http.MethodPost, "/accounts/login", `{"method":"oauth"}`)
	sid, _ := start["session_id"].(string)
	if sid == "" {
		t.Fatalf("login start = %v", start)
	}
	<-pollStarted
	if code, _ := h.do(t, http.MethodDelete, "/accounts/login/"+sid, ""); code != http.StatusOK {
		t.Fatalf("cancel = %d", code)
	}
	// The poll that was already in flight now answers "authorized".
	close(release)

	for i := 0; i < 200; i++ {
		if h.srv.store.configured() {
			t.Fatal("a cancelled login stored a credential")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
