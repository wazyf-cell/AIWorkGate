package gateway

import (
	"context"
	"errors"
	"sync"
	"time"
)

// errQueueTimeout reports that a plugin concurrency slot did not free up
// within the queue timeout.
var errQueueTimeout = errors.New("gateway: plugin concurrency queue timeout")

// limiter is a per-plugin counting semaphore. It is built on buffered channels
// so waiting requests never leak goroutines: releases are plain receives.
type limiter struct {
	mu   sync.Mutex
	sems map[string]*semSlot
}

type semSlot struct {
	ch chan struct{}
	n  int
}

func newLimiter() *limiter {
	return &limiter{sems: make(map[string]*semSlot)}
}

// acquire takes a slot for pluginID, waiting up to timeout. The returned func
// releases it. A nil error means the caller owns the slot.
func (l *limiter) acquire(ctx context.Context, pluginID string, n int, timeout time.Duration) (func(), error) {
	if n <= 0 {
		n = defaultConcurrency
	}
	if timeout <= 0 {
		timeout = defaultQueueTimeout
	}

	slot := l.slot(pluginID, n)
	if slot == nil {
		return nil, context.Canceled
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	select {
	case slot.ch <- struct{}{}:
		return func() { <-slot.ch }, nil
	default:
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case slot.ch <- struct{}{}:
		return func() { <-slot.ch }, nil
	case <-timer.C:
		return nil, errQueueTimeout
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// slot returns the semaphore for pluginID, recreating it when a drained slot
// has a different configured capacity.
func (l *limiter) slot(pluginID string, n int) *semSlot {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.sems[pluginID]
	if s == nil {
		s = &semSlot{ch: make(chan struct{}, n), n: n}
		l.sems[pluginID] = s
		return s
	}
	if s.n != n && len(s.ch) == 0 {
		s = &semSlot{ch: make(chan struct{}, n), n: n}
		l.sems[pluginID] = s
	}
	return s
}
