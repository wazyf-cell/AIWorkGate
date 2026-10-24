package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"aiworkgate/internal/model"
)

func TestLimiterCapacityAndQueueTimeout(t *testing.T) {
	l := newLimiter()
	ctx := context.Background()

	release, err := l.acquire(ctx, "p", 1, time.Second)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	start := time.Now()
	if _, err := l.acquire(ctx, "p", 1, 50*time.Millisecond); !errors.Is(err, errQueueTimeout) {
		t.Fatalf("second acquire error = %v, want errQueueTimeout", err)
	}
	if elapsed := time.Since(start); elapsed < 25*time.Millisecond {
		t.Fatalf("queue timeout fired after %v, want about 50ms", elapsed)
	}

	release()
	release2, err := l.acquire(ctx, "p", 1, time.Second)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	release2()
}

func TestLimiterUsesDefaultConcurrency(t *testing.T) {
	l := newLimiter()
	ctx := context.Background()
	releases := make([]func(), 0, defaultConcurrency)
	for i := 0; i < defaultConcurrency; i++ {
		release, err := l.acquire(ctx, "p", 0, time.Second)
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		releases = append(releases, release)
	}
	if _, err := l.acquire(ctx, "p", 0, 25*time.Millisecond); !errors.Is(err, errQueueTimeout) {
		t.Fatalf("over-capacity acquire error = %v, want errQueueTimeout", err)
	}
	for _, release := range releases {
		release()
	}
}

func TestLimiterHonorsContextCancellation(t *testing.T) {
	l := newLimiter()
	base := context.Background()
	release, err := l.acquire(base, "p", 1, time.Second)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(base, 20*time.Millisecond)
	defer cancel()
	if _, err := l.acquire(ctx, "p", 1, time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued acquire error = %v, want context.DeadlineExceeded", err)
	}

	canceled, cancelNow := context.WithCancel(base)
	cancelNow()
	if _, err := l.acquire(canceled, "p", 1, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled acquire error = %v, want context.Canceled", err)
	}
}

func TestNoGoroutineLeakOnAbortedRequests(t *testing.T) {
	if testing.Short() {
		t.Skip("goroutine settling test")
	}

	entered := make(chan struct{}, 16)
	srv, _ := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		select {
		case entered <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	})
	reg := newFakeRegistry().add(
		pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "tok")
	g := newTestGateway(t, Options{Plugins: reg})
	gw := httptest.NewServer(g)
	defer gw.Close()

	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	time.Sleep(20 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	for i := 0; i < 10; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			gw.URL+"/v1/chat/completions", strings.NewReader(`{"model":"p/chat","stream":true}`))
		if err != nil {
			cancel()
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+clientKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			cancel()
			t.Fatalf("request %d: %v", i, err)
		}
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			cancel()
			resp.Body.Close()
			t.Fatalf("request %d never reached the plugin", i)
		}
		cancel()
		resp.Body.Close()
	}

	client.CloseIdleConnections()
	g.CloseIdleConnections()

	// Connection and handler goroutines unwind asynchronously; allow a small
	// settle window and a small tolerance for unrelated runtime goroutines.
	deadline := time.Now().Add(5 * time.Second)
	for {
		now := runtime.NumGoroutine()
		if now <= baseline+8 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("possible goroutine leak: baseline %d, now %d after 10 aborted requests",
				baseline, now)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
