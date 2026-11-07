package gateway

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// errQueueTimeout reports that a plugin concurrency slot did not free up
// within the queue timeout.
var errQueueTimeout = errors.New("gateway: plugin concurrency queue timeout")

// errQueueFull reports that the plugin's queue already holds the configured
// maximum number of waiting requests (P5 排队上限). The caller rejects the
// request immediately instead of parking it.
var errQueueFull = errors.New("gateway: plugin concurrency queue is full")

// limiter is a per-plugin counting semaphore. It is built on buffered channels
// so waiting requests never leak goroutines: releases are plain receives.
type limiter struct {
	mu   sync.Mutex
	sems map[string]*semSlot
	// depth caps how many requests may park per slot (P5 排队上限); 0 =
	// unbounded. Stored atomically so the admin config save can retune it
	// while traffic flows.
	depth atomic.Int64
}

type semSlot struct {
	ch chan struct{}
	n  int
	// waiting counts requests that passed the fast path and are parked on the
	// send below. Guarded by limiter.mu; it bounds the queue depth without
	// blocking the release path.
	waiting int
}

func newLimiter() *limiter {
	return &limiter{sems: make(map[string]*semSlot)}
}

// SetQueueDepth retunes the per-plugin waiting cap at runtime. 0 removes the
// cap.
func (l *limiter) SetQueueDepth(depth int) {
	if l == nil {
		return
	}
	if depth < 0 {
		depth = 0
	}
	l.depth.Store(int64(depth))
}

// acquire takes a slot for pluginID, waiting up to timeout. The returned func
// releases it. A nil error means the caller owns the slot.
//
// timeout == 0 falls back to defaultQueueTimeout (the historical unset
// behaviour); a negative timeout waits without a deadline and is bounded only
// by ctx cancellation (P5 排队等待, 0 = 一直等). A maxDepth greater than zero
// rejects a request that would park past the cap with errQueueFull instead of
// queuing it.
func (l *limiter) acquire(ctx context.Context, pluginID string, n int, timeout time.Duration) (func(), error) {
	if n <= 0 {
		n = defaultConcurrency
	}
	if timeout == 0 {
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

	// Queue-depth gate (P5 排队上限): past the fast path the request parks on
	// the channel. Shed load immediately when the park is full so an overload
	// cannot build an unbounded queue. The counter is bumped under the limiter
	// mutex and unwound on every exit path (slot won, timeout, cancel).
	maxDepth := int(l.depth.Load())
	if maxDepth > 0 {
		l.mu.Lock()
		if slot.waiting >= maxDepth {
			l.mu.Unlock()
			return nil, errQueueFull
		}
		slot.waiting++
		l.mu.Unlock()
		defer func() {
			l.mu.Lock()
			slot.waiting--
			l.mu.Unlock()
		}()
	}

	if timeout < 0 {
		select {
		case slot.ch <- struct{}{}:
			return func() { <-slot.ch }, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
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
	if s.n != n && len(s.ch) == 0 && s.waiting == 0 {
		s = &semSlot{ch: make(chan struct{}, n), n: n}
		l.sems[pluginID] = s
	}
	return s
}
