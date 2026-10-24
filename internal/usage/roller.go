// Package usage folds request logs into the daily usage table and serves the
// usage admin API (summary, credits, expiry, sessions, CSV export).
package usage

import (
	"context"
	"sync"
	"time"

	"aiworkgate/internal/logging"
	"aiworkgate/internal/safego"
	"aiworkgate/internal/store"
)

const (
	// DefaultRollInterval is the incremental aggregation cadence (§8.3).
	DefaultRollInterval = 60 * time.Second
	// DefaultCatchUpDays bounds the startup catch-up window.
	DefaultCatchUpDays = 30
	// defaultRollBatch is the number of api_logs rows folded per transaction.
	defaultRollBatch = 1000
)

// Logger is the slice of the host logger the roller needs.
type Logger interface {
	Info(module, msg string, fields map[string]any)
	Warn(module, msg string, fields map[string]any)
}

// Roller periodically folds api_logs into usage_daily. The cursor lives in kv
// and advances in the same transaction as the upsert.
type Roller struct {
	Store       *store.Store
	Log         Logger
	Interval    time.Duration
	CatchUpDays int
	Batch       int
	Now         func() time.Time

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// NewRoller returns a roller with the production defaults.
func NewRoller(st *store.Store, log *logging.Logger) *Roller {
	r := &Roller{Store: st, Interval: DefaultRollInterval, CatchUpDays: DefaultCatchUpDays, Batch: defaultRollBatch, Now: time.Now}
	if log != nil {
		r.Log = log
	}
	return r
}

// Start launches Run in the background. It is idempotent while a loop runs.
func (r *Roller) Start(ctx context.Context) {
	if r == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r.mu.Lock()
	if r.cancel != nil {
		r.mu.Unlock()
		return
	}
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	r.cancel, r.done = cancel, done
	r.mu.Unlock()

	safego.Go(r.Log, "usageRoller", func() {
		defer close(done)
		r.Run(loopCtx)
	})
}

// Stop cancels Run and waits for it to return. Safe to call more than once.
func (r *Roller) Stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	cancel, done := r.cancel, r.done
	r.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	if done != nil {
		<-done
	}
	r.mu.Lock()
	if r.done == done {
		r.cancel, r.done = nil, nil
	}
	r.mu.Unlock()
}

// Run performs a catch-up pass immediately and then every Interval.
func (r *Roller) Run(ctx context.Context) {
	if r == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r.rollAndLog(ctx)
	interval := r.Interval
	if interval <= 0 {
		interval = DefaultRollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.rollAndLog(ctx)
		}
	}
}

// RollOnce drains every ready batch. It never fails startup: errors are
// returned for callers that want them and logged by Run.
func (r *Roller) RollOnce(ctx context.Context) (int64, error) {
	if r == nil || r.Store == nil {
		return 0, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	days := r.CatchUpDays
	if days <= 0 {
		days = DefaultCatchUpDays
	}
	cutoff := now.AddDate(0, 0, -days)
	batch := r.Batch
	if batch <= 0 {
		batch = defaultRollBatch
	}
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, _, more, err := r.Store.RollUsage(ctx, cutoff, batch)
		if err != nil {
			return total, err
		}
		total += n
		if !more {
			break
		}
	}
	return total, nil
}

func (r *Roller) rollAndLog(ctx context.Context) {
	n, err := r.RollOnce(ctx)
	if err != nil {
		if ctx.Err() == nil && r.Log != nil {
			r.Log.Warn("usage", "usage rollup failed", map[string]any{"error": err.Error()})
		}
		return
	}
	if n > 0 && r.Log != nil {
		r.Log.Info("usage", "usage rollup advanced", map[string]any{"rows": n})
	}
}
