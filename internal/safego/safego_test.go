package safego

import (
	"sync"
	"testing"
	"time"
)

type testLogger struct {
	mu   sync.Mutex
	msgs []string
}

func (l *testLogger) Error(module, msg string, fields map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.msgs = append(l.msgs, msg)
}

func TestGoRecoversPanic(t *testing.T) {
	log := &testLogger{}
	done := make(chan struct{})

	Go(log, "test-panic", func() {
		defer close(done)
		panic("boom")
	})

	select {
	case <-done:
		// Goroutine finished (recovered from panic)
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for goroutine to finish")
	}

	// The recover runs after fn's own defers, so wait for the entry instead of
	// sleeping a fixed amount; read under the lock the logger writes with.
	deadline := time.Now().Add(2 * time.Second)
	for {
		log.mu.Lock()
		n, first := len(log.msgs), ""
		if n > 0 {
			first = log.msgs[0]
		}
		log.mu.Unlock()
		if n == 1 {
			if first != "background task panicked: test-panic" {
				t.Fatalf("unexpected log message: %q", first)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected 1 error log, got %d msgs=%v", n, log.msgs)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestGoWithNilLogger(t *testing.T) {
	done := make(chan struct{})

	// Should not panic even with nil logger
	Go(nil, "test-nil", func() {
		defer close(done)
		panic("boom")
	})

	select {
	case <-done:
		// Success
	case <-time.After(2 * time.Second):
		t.Fatal("timeout")
	}
}

func TestGoRunsSuccessfully(t *testing.T) {
	log := &testLogger{}
	var ran bool
	var wg sync.WaitGroup
	wg.Add(1)

	Go(log, "test-ok", func() {
		defer wg.Done()
		ran = true
	})

	wg.Wait()

	if !ran {
		t.Fatal("function was not executed")
	}
	if len(log.msgs) != 0 {
		t.Fatalf("unexpected error logs: %v", log.msgs)
	}
}
