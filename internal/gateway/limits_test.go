package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
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

// limiterWaiting reports how many requests are parked past the fast path.
func limiterWaiting(l *limiter, pluginID string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	slot := l.sems[pluginID]
	if slot == nil {
		return 0
	}
	return slot.waiting
}

// waitForQueueDepth polls until the plugin has parked want requests.
func waitForQueueDepth(t *testing.T, l *limiter, pluginID string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := limiterWaiting(l, pluginID); got >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue depth for %s = %d after 2s, want %d", pluginID, limiterWaiting(l, pluginID), want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestLimiterQueueFullRejectsImmediately pins the P5 排队上限: a request that
// arrives on a full queue is refused at once with errQueueFull instead of
// parking behind the cap.
func TestLimiterQueueFullRejectsImmediately(t *testing.T) {
	l := newLimiter()
	l.SetQueueDepth(1)
	ctx := context.Background()

	release, err := l.acquire(ctx, "p", 1, time.Second)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	// One waiter fits inside the cap; it parks until the slot frees.
	queued := make(chan error, 1)
	go func() {
		rel, err := l.acquire(ctx, "p", 1, 5*time.Second)
		if err == nil {
			defer rel()
		}
		queued <- err
	}()
	waitForQueueDepth(t, l, "p", 1)

	start := time.Now()
	if _, err := l.acquire(ctx, "p", 1, time.Second); !errors.Is(err, errQueueFull) {
		t.Fatalf("third acquire error = %v, want errQueueFull", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("queue-full rejection took %v, want immediate", elapsed)
	}

	release()
	select {
	case err := <-queued:
		if err != nil {
			t.Fatalf("queued acquire = %v, want nil after release", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued acquire did not finish after release")
	}
}

// TestLimiterZeroQueueDepthWaitsUnbounded pins 0 = 不限: with no configured
// cap, any number of requests may park instead of being rejected.
func TestLimiterZeroQueueDepthWaitsUnbounded(t *testing.T) {
	l := newLimiter()
	ctx := context.Background()

	release, err := l.acquire(ctx, "p", 1, time.Second)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	const waiters = 3
	done := make(chan error, waiters)
	for i := 0; i < waiters; i++ {
		go func() {
			rel, err := l.acquire(ctx, "p", 1, 5*time.Second)
			if err == nil {
				rel()
			}
			done <- err
		}()
	}
	// Without a cap none of them may be refused while the slot is held.
	select {
	case err := <-done:
		t.Fatalf("waiter finished early with %v, want it to park", err)
	case <-time.After(150 * time.Millisecond):
	}

	release()
	for i := 0; i < waiters; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("waiter %d = %v, want nil", i, err)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("waiter %d did not finish after release", i)
		}
	}
}

// TestLimiterUnlimitedWaitHonorsContext pins the P5 排队等待 0 语义: a negative
// timeout waits without a deadline and is bounded only by ctx cancellation —
// it must never surface as errQueueTimeout.
func TestLimiterUnlimitedWaitHonorsContext(t *testing.T) {
	l := newLimiter()
	base := context.Background()
	release, err := l.acquire(base, "p", 1, time.Second)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(base, 30*time.Millisecond)
	defer cancel()
	if _, err := l.acquire(ctx, "p", 1, -1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unlimited acquire error = %v, want context.DeadlineExceeded", err)
	}
}

// TestLimiterQueueDepthUnwindsOnEveryExit pins the release path of the P5
// 排队上限 counter: a request that times out, is cancelled or wins the slot
// must always leave the parked count, or the cap would leak and eventually
// reject everything.
func TestLimiterQueueDepthUnwindsOnEveryExit(t *testing.T) {
	l := newLimiter()
	l.SetQueueDepth(2)
	ctx := context.Background()

	release, err := l.acquire(ctx, "p", 1, time.Second)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	// Two requests park and time out.
	for i := 0; i < 2; i++ {
		if _, err := l.acquire(ctx, "p", 1, 25*time.Millisecond); !errors.Is(err, errQueueTimeout) {
			t.Fatalf("parked acquire %d = %v, want errQueueTimeout", i, err)
		}
	}
	if got := limiterWaiting(l, "p"); got != 0 {
		t.Fatalf("parked count after timeouts = %d, want 0", got)
	}

	// One request parks and is cancelled mid-wait.
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := l.acquire(cctx, "p", 1, 5*time.Second)
		done <- err
	}()
	waitForQueueDepth(t, l, "p", 1)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled acquire = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled acquire did not return")
	}
	if got := limiterWaiting(l, "p"); got != 0 {
		t.Fatalf("parked count after cancel = %d, want 0", got)
	}

	// The cap survives every exit path — retune it to 1 (the runtime retune
	// path itself) and exactly one more request may park.
	l.SetQueueDepth(1)
	go func() {
		rel, err := l.acquire(ctx, "p", 1, 5*time.Second)
		if err == nil {
			defer rel()
		}
		done <- err
	}()
	waitForQueueDepth(t, l, "p", 1)
	if _, err := l.acquire(ctx, "p", 1, time.Second); !errors.Is(err, errQueueFull) {
		t.Fatalf("over-cap acquire = %v, want errQueueFull", err)
	}

	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("final queued acquire = %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("final queued acquire did not finish")
	}
}

// TestQueueFullReturns429Envelope pins the dispatch mapping of errQueueFull:
// a request arriving on a full queue is answered with the structured 429
// rate_limited/queue_full envelope while the queued one still waits.
func TestQueueFullReturns429Envelope(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()

	srv, _ := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	reg := newFakeRegistry()
	reg.add(pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "tok")
	reg.setConcurrency("p", 1)
	g := newTestGateway(t, Options{
		Plugins:       reg,
		QueueTimeout:  5 * time.Second,
		QueueMaxDepth: 1,
	})

	body := `{"model":"p/chat","messages":[]}`
	firstRec := httptest.NewRecorder()
	firstDone := make(chan struct{})
	go func() {
		g.ServeHTTP(firstRec, newChatRequest(t, "/v1/chat/completions", clientKey, body))
		close(firstDone)
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first request never reached the plugin")
	}

	// The second request fills the single waiting slot.
	secondRec := httptest.NewRecorder()
	secondDone := make(chan struct{})
	go func() {
		g.ServeHTTP(secondRec, newChatRequest(t, "/v1/chat/completions", clientKey, body))
		close(secondDone)
	}()
	waitForQueueDepth(t, g.lim, "p", 1)

	thirdRec := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, body))
	if thirdRec.Code != http.StatusTooManyRequests {
		t.Fatalf("third status = %d, want 429 (body %s)", thirdRec.Code, thirdRec.Body.String())
	}
	payload := decodeError(t, thirdRec)
	if payload.Error.Type != "rate_limited" || payload.Error.Code != "queue_full" {
		t.Fatalf("error = %s/%s, want rate_limited/queue_full", payload.Error.Type, payload.Error.Code)
	}
	unblock()
	for _, rec := range []struct {
		name string
		done chan struct{}
		rec  *httptest.ResponseRecorder
	}{
		{"first", firstDone, firstRec},
		{"second", secondDone, secondRec},
	} {
		select {
		case <-rec.done:
		case <-time.After(3 * time.Second):
			t.Fatalf("%s request did not finish", rec.name)
		}
		if rec.rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200 (body %s)", rec.name, rec.rec.Code, rec.rec.Body.String())
		}
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
