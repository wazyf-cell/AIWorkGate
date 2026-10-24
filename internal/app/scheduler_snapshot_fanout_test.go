package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aiworkgate/internal/logging"
	"aiworkgate/internal/model"
)

// fanoutSource is a schedulerSource that reports what a snapshot round actually
// did to the plugins: how many reads overlapped, which deadline a read was
// given, and how often one plugin was called. Every plugin it lists is running.
//
// Per plugin a test can either block the read until the test releases it
// (block) or hold it for a fixed time (delay). Holding every read for the same
// time makes peakConcurrency the evidence of the fan-out: a round that walks
// plugins one by one can never have more than one read in flight.
type fanoutSource struct {
	mu          sync.Mutex
	plugins     []string
	inFlight    int
	maxInFlight int
	calls       map[string]int
	budgets     map[string]time.Duration
	block       map[string]chan struct{}
	delay       map[string]time.Duration
}

func newFanoutSource(plugins ...string) *fanoutSource {
	return &fanoutSource{
		plugins: plugins,
		calls:   map[string]int{},
		budgets: map[string]time.Duration{},
		block:   map[string]chan struct{}{},
		delay:   map[string]time.Duration{},
	}
}

func (f *fanoutSource) List() []model.PluginStatus {
	out := make([]model.PluginStatus, 0, len(f.plugins))
	for _, id := range f.plugins {
		out = append(out, model.PluginStatus{
			Manifest: model.Manifest{ID: id, Name: id, APILevel: 2},
			Enabled:  true, Running: true,
		})
	}
	return out
}

func (f *fanoutSource) Models(ctx context.Context, pluginID string) ([]model.ModelInfo, error) {
	return nil, nil
}

// Accounts records the read, honours the test's block/delay/barrier, and answers
// with one account so the round can tell a live answer from a cache fallback.
func (f *fanoutSource) Accounts(ctx context.Context, pluginID string) ([]model.Account, error) {
	entered := time.Now()
	f.mu.Lock()
	f.calls[pluginID]++
	f.inFlight++
	if f.inFlight > f.maxInFlight {
		f.maxInFlight = f.inFlight
	}
	if deadline, ok := ctx.Deadline(); ok {
		f.budgets[pluginID] = deadline.Sub(entered)
	}
	blocked, hasBlock := f.block[pluginID]
	delay := f.delay[pluginID]
	f.mu.Unlock()

	if hasBlock {
		select {
		case <-blocked:
		case <-ctx.Done():
		}
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
		}
	}

	f.mu.Lock()
	f.inFlight--
	f.mu.Unlock()

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return []model.Account{{
		ID: "acc_" + pluginID, Label: pluginID, Enabled: true,
		Credential: &model.Credential{Present: true},
	}}, nil
}

func (f *fanoutSource) callCount(pluginID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[pluginID]
}

func (f *fanoutSource) peakConcurrency() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxInFlight
}

// budgetOf returns the deadline the round handed to one plugin's read: 0 when
// the read never reported one.
func (f *fanoutSource) budgetOf(pluginID string) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.budgets[pluginID]
}

// TestSnapshotReadsPluginsInParallel is the guard for the fan-out. The old round
// walked src.List() one plugin at a time, so at most one read was ever in
// flight and the round cost was the sum of the plugin latencies.
func TestSnapshotReadsPluginsInParallel(t *testing.T) {
	const hold = 100 * time.Millisecond
	app, _ := newSchedulerApp(t)
	src := newFanoutSource("aichat", "officeace", "gemini", "zcode")
	for _, id := range src.plugins {
		src.delay[id] = hold
	}

	start := time.Now()
	snap, err := app.snapshotFrom(context.Background(), src)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if peak := src.peakConcurrency(); peak < len(src.plugins) {
		t.Fatalf("max concurrent plugin reads = %d, want %d (the round still walks plugins one by one)",
			peak, len(src.plugins))
	}
	if elapsed >= time.Duration(len(src.plugins))*hold {
		t.Fatalf("round took %s for %d plugins held %s each: the reads did not overlap",
			elapsed, len(src.plugins), hold)
	}
	for _, id := range src.plugins {
		pool := snap.Pools[id]
		if pool == nil || len(pool.Accounts) != 1 || pool.Accounts[0].ID != "acc_"+id {
			t.Fatalf("pool %s = %+v, want the live account", id, pool)
		}
	}
}

// TestPluginReadBudgetIsNotInheritedFromCaller is the root-cause assertion: the
// per-plugin budget used to be context.WithTimeout(callerCtx, 5s), so a caller
// that had already spent its own deadline handed every plugin a context that was
// expired on arrival, and the plugin was then logged as broken.
func TestPluginReadBudgetIsNotInheritedFromCaller(t *testing.T) {
	app, _ := newSchedulerApp(t)
	src := newFanoutSource("zcode")
	// Slower than the caller is willing to wait, so the round gives up while the
	// read is still running.
	src.delay["zcode"] = 200 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	snap, err := app.snapshotFrom(ctx, src)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if pool := snap.Pools["zcode"]; pool != nil && len(pool.Accounts) != 0 {
		t.Fatalf("the round waited past its own deadline: %+v", pool.Accounts)
	}

	if calls := src.callCount("zcode"); calls != 1 {
		t.Fatalf("accounts calls = %d, want 1", calls)
	}
	budget := src.budgetOf("zcode")
	if budget < schedulerAccountTimeout-time.Second || budget > schedulerAccountTimeout+time.Second {
		t.Fatalf("read budget = %s, want %s detached from the caller's 40ms deadline", budget, schedulerAccountTimeout)
	}
}

// TestSlowPluginDoesNotStarveHealthyOnes reproduces the host log burst of
// 2026-10-22T17:20:32, where all twelve plugins' /accounts and /v1/models failed
// with "context deadline exceeded" inside the same second: one plugin held the
// read, the shared 3s round budget (healthzSnapshotTimeout) expired, and every
// plugin behind it — zcode last in the sorted list — was reported as broken
// although its own /accounts handler does no I/O at all.
func TestSlowPluginDoesNotStarveHealthyOnes(t *testing.T) {
	logDir := t.TempDir()
	logger, err := logging.NewLogger(logDir)
	if err != nil {
		t.Fatalf("logging.NewLogger: %v", err)
	}
	t.Cleanup(func() { _ = logger.Close() })

	app, _ := newSchedulerApp(t)
	app.logger = logger

	src := newFanoutSource("slowpoke", "zcode")
	release := make(chan struct{})
	src.block["slowpoke"] = release
	t.Cleanup(func() { close(release) })

	// Read the budget the healthy plugin is given before the round gives up: it
	// must be its own, not the caller's remaining 100ms.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	snap, err := app.snapshotFrom(ctx, src)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.Pools["zcode"] == nil || snap.Pools["slowpoke"] == nil {
		t.Fatalf("a pool was lost instead of degraded: %+v", snap.Pools)
	}
	if budget := src.budgetOf("zcode"); budget != 0 && budget < time.Second {
		t.Fatalf("healthy plugin read budget = %s, want its own %s", budget, schedulerAccountTimeout)
	}
	for _, entry := range readHostLog(t, logDir) {
		if entry["level"] != "warn" || entry["msg"] != "scheduling snapshot cannot read accounts" {
			continue
		}
		t.Fatalf("giving up on the round was logged as a plugin failure: %v", entry)
	}
}

// TestAbandonedReadFillsCacheForNextRound: giving up on the caller's budget must
// not lose the answer. The read keeps running on its own budget, the next round
// joins it instead of calling the plugin again, and the pool is complete one
// round later rather than for two rounds in a row.
func TestAbandonedReadFillsCacheForNextRound(t *testing.T) {
	app, _ := newSchedulerApp(t)
	src := newFanoutSource("zcode")
	src.delay["zcode"] = 300 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	first, err := app.snapshotFrom(ctx, src)
	if err != nil {
		t.Fatalf("first snapshot: %v", err)
	}
	if len(first.Pools["zcode"].Accounts) != 0 {
		t.Fatalf("the first round should have served the fallback, got %+v", first.Pools["zcode"].Accounts)
	}

	second, err := app.snapshotFrom(context.Background(), src)
	if err != nil {
		t.Fatalf("second snapshot: %v", err)
	}
	accounts := second.Pools["zcode"].Accounts
	if len(accounts) != 1 || accounts[0].ID != "acc_zcode" {
		t.Fatalf("second round did not pick up the abandoned read: %+v", accounts)
	}
	if calls := src.callCount("zcode"); calls != 1 {
		t.Fatalf("accounts calls = %d, want the in-flight read reused", calls)
	}
}

// TestConcurrentRoundsDedupeOnePluginRead keeps the fan-out from turning the 10s
// cache expiry into a stampede: rounds that miss the cache together must share
// one plugin call, not issue one each.
func TestConcurrentRoundsDedupeOnePluginRead(t *testing.T) {
	app, _ := newSchedulerApp(t)
	src := newFanoutSource("zcode")
	src.delay["zcode"] = 150 * time.Millisecond

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := app.snapshotFrom(context.Background(), src); err != nil {
				t.Errorf("snapshot: %v", err)
			}
		}()
	}
	wg.Wait()

	if calls := src.callCount("zcode"); calls != 1 {
		t.Fatalf("accounts calls = %d, want 1 (five concurrent rounds stampeded one plugin)", calls)
	}
}

// TestFailedReadStillWarnsWithItsBudget keeps the useful signal: a plugin that
// really runs past its own budget must still be logged, and the line must name
// the budget it exceeded so an operator can tell a plugin failure apart from a
// round that merely gave up waiting.
func TestFailedReadStillWarnsWithItsBudget(t *testing.T) {
	logDir := t.TempDir()
	logger, err := logging.NewLogger(logDir)
	if err != nil {
		t.Fatalf("logging.NewLogger: %v", err)
	}
	t.Cleanup(func() { _ = logger.Close() })

	app, _ := newSchedulerApp(t)
	app.logger = logger

	src := newFanoutSource("zcode")
	// Nothing releases this read, so it ends on its own schedulerAccountTimeout.
	src.block["zcode"] = make(chan struct{})

	snap, err := app.snapshotFrom(context.Background(), src)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(snap.Pools["zcode"].Accounts) != 0 {
		t.Fatalf("a blocked plugin should not look ready: %+v", snap.Pools["zcode"].Accounts)
	}

	deadline := time.Now().Add(schedulerAccountTimeout + 5*time.Second)
	found := false
	for !found && time.Now().Before(deadline) {
		for _, entry := range readHostLog(t, logDir) {
			if entry["level"] == "warn" && entry["msg"] == "scheduling snapshot cannot read accounts" &&
				entry["plugin"] == "zcode" && entry["budget"] == schedulerAccountTimeout.String() {
				found = true
				break
			}
		}
		if !found {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if !found {
		t.Fatal("the plugin that really exceeded its budget was not warned about")
	}
}

// readHostLog returns every host log entry written under dir so far.
func readHostLog(t *testing.T, dir string) []map[string]any {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "logs", "host-*.jsonl"))
	if err != nil {
		t.Fatalf("glob host log: %v", err)
	}
	entries := make([]map[string]any, 0, 8)
	for _, name := range files {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			var entry map[string]any
			if strings.TrimSpace(line) != "" && json.Unmarshal([]byte(line), &entry) == nil {
				entries = append(entries, entry)
			}
		}
	}
	return entries
}
