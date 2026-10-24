package main

// Refresh single-flight: a burst of renewals for the same account must spend the
// upstream refresh endpoint once, not once per caller. WorkOS refresh tokens are
// rotated, so duplicated calls can invalidate each other.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestConcurrentRefreshesCallUpstreamOnce(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != refreshPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mu.Lock()
		calls++
		mu.Unlock()
		// Hold the answer so every concurrent caller is still in flight.
		time.Sleep(150 * time.Millisecond)
		_, _ = io.WriteString(w, `{"accessToken":"at-new","refreshToken":"rt-1","expiresAt":1792627200}`)
	}))
	defer mock.Close()

	srv, err := newServer(serverConfig{ID: "cline", Token: "tk", DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	srv.client.api = mock.URL
	if err := srv.store.set(&clineCredential{AccessToken: "at-old", RefreshToken: "rt-1"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	handler := srv.routes()

	const n = 8
	start := make(chan struct{})
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			req := httptest.NewRequest(http.MethodPost, "/accounts/default/refresh", nil)
			req.Header.Set("Authorization", "Bearer tk")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			codes[i] = rec.Code
		}(i)
	}
	close(start)
	wg.Wait()

	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Fatalf("upstream refresh calls = %d, want 1 (concurrent renewals must be single-flighted)", got)
	}
	for i, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("refresh %d = %d", i, code)
		}
	}
	if stored, _ := srv.store.get(); stored.AccessToken != "at-new" {
		t.Fatalf("renewed token not stored: %+v", stored)
	}
}
