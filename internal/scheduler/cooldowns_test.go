package scheduler

import (
	"sort"
	"sync"
	"testing"
	"time"
)

// fakeCooldowns is an in-memory CooldownStore that records calls.
type fakeCooldowns struct {
	mu      sync.Mutex
	rows    map[cooldownRow]CooldownRecord
	loads   int
	saves   int
	deletes int
}

func newFakeCooldowns() *fakeCooldowns {
	return &fakeCooldowns{rows: map[cooldownRow]CooldownRecord{}}
}

func (f *fakeCooldowns) LoadCooldowns(now int64) ([]CooldownRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads++
	out := make([]CooldownRecord, 0, len(f.rows))
	for key, row := range f.rows {
		if row.Until <= now {
			delete(f.rows, key)
			continue
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PluginID != out[j].PluginID {
			return out[i].PluginID < out[j].PluginID
		}
		if out[i].AccountID != out[j].AccountID {
			return out[i].AccountID < out[j].AccountID
		}
		return out[i].Model < out[j].Model
	})
	return out, nil
}

func (f *fakeCooldowns) SaveCooldown(record CooldownRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saves++
	f.rows[cooldownRow{PluginID: record.PluginID, AccountID: record.AccountID, Model: record.Model}] = record
	return nil
}

func (f *fakeCooldowns) DeleteCooldown(pluginID, accountID, modelID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes++
	delete(f.rows, cooldownRow{PluginID: pluginID, AccountID: accountID, Model: modelID})
	return nil
}

func (f *fakeCooldowns) row(pluginID, accountID, model string) (CooldownRecord, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[cooldownRow{PluginID: pluginID, AccountID: accountID, Model: model}]
	return row, ok
}

// TestRestoreCooldownsContinuesBackoff proves that a restart restores account
// and model cooldowns, ignores expired rows and keeps the 429 strike counter so
// the next rate limit continues the doubling instead of restarting at 30s.
func TestRestoreCooldownsContinuesBackoff(t *testing.T) {
	now := testBase
	clk := &clock{at: now}
	store := newFakeCooldowns()
	store.rows[cooldownRow{PluginID: "p", AccountID: "a"}] = CooldownRecord{
		PluginID: "p", AccountID: "a", Until: now + 30, Reason: "status 500",
	}
	store.rows[cooldownRow{PluginID: "p", AccountID: "b", Model: "m1"}] = CooldownRecord{
		PluginID: "p", AccountID: "b", Model: "m1", Until: now + 30, Reason: "status 429", Strikes: 2,
	}
	// Already expired: must not come back after the restart.
	store.rows[cooldownRow{PluginID: "p", AccountID: "b", Model: "m2"}] = CooldownRecord{
		PluginID: "p", AccountID: "b", Model: "m2", Until: now - 1, Reason: "status 429", Strikes: 5,
	}
	store.rows[cooldownRow{PluginID: "p", AccountID: "c"}] = CooldownRecord{
		PluginID: "p", AccountID: "c", Until: now - 1, Reason: "status 500",
	}

	var (
		logs   int
		fields map[string]any
	)
	s := New(Options{
		Now:       clk.now,
		Cooldowns: store,
		Log: func(msg string, kv map[string]any) {
			logs++
			fields = kv
		},
	})
	defer s.Close()
	s.RestoreCooldowns()
	s.RestoreCooldowns() // idempotent: one load, one log line

	if store.loads != 1 {
		t.Fatalf("loads = %d, want 1", store.loads)
	}
	if logs != 1 {
		t.Fatalf("log lines = %d, want 1", logs)
	}
	if fields["count"] != 2 || fields["accounts"] != 1 || fields["models"] != 1 {
		t.Fatalf("restore log fields = %+v", fields)
	}

	keyA := AccountKey{PluginID: "p", AccountID: "a"}
	keyB := AccountKey{PluginID: "p", AccountID: "b"}
	keyC := AccountKey{PluginID: "p", AccountID: "c"}
	if _, ok := s.accountCD[keyC]; ok {
		t.Fatal("expired account cooldown restored")
	}
	if _, ok := s.modelCD[modelCooldownKey{Key: keyB, Model: "m2"}]; ok {
		t.Fatal("expired model cooldown restored")
	}
	if got := s.accountCD[keyA]; got != now+30 {
		t.Fatalf("account cooldown until = %d, want %d", got, now+30)
	}
	if got := s.modelCD[modelCooldownKey{Key: keyB, Model: "m1"}]; got.Strikes != 2 || got.Until != now+30 {
		t.Fatalf("restored model state = %+v", got)
	}

	// Both a and b are cooling down for m1, so c serves it.
	a, b, c := account("a"), account("b"), account("c")
	p := pool("p", a, b, c)
	p.Strategy = StrategyFixed
	snap := snapOf(p)
	if got := pick(t, s, snap, "p/m1"); got.AccountID != "c" {
		t.Fatalf("m1 = %s, want c while both cooldowns are live", got.AccountID)
	}
	// m2 was expired, so b is eligible again for it.
	if got := pick(t, s, snap, "p/m2"); got.AccountID != "b" {
		t.Fatalf("m2 = %s, want b (expired cooldown ignored)", got.AccountID)
	}

	// The restored strikes=2 keeps doubling: the next 429 is strike 3 -> 120s.
	clk.advance(31)
	s.Report(keyB, "m1", 429, 0, 0)
	if got := s.modelCD[modelCooldownKey{Key: keyB, Model: "m1"}]; got.Strikes != 3 || got.Until != clk.at+120 {
		t.Fatalf("next 429 state = %+v, want strikes 3 until %d", got, clk.at+120)
	}
}

// TestReportPersistsCooldowns covers the buffered writer: bursts coalesce per
// key, success clears the persisted rows, and Close flushes pending writes.
func TestReportPersistsCooldowns(t *testing.T) {
	clk := &clock{at: testBase}
	store := newFakeCooldowns()
	s := New(Options{Now: clk.now, Cooldowns: store})
	key := AccountKey{PluginID: "p", AccountID: "a"}

	// Two 429s before the debounce window produce one coalesced upsert.
	s.Report(key, "m1", 429, 0, 0)
	s.Report(key, "m1", 429, 0, 0)
	s.FlushCooldowns()
	row, ok := store.row("p", "a", "m1")
	if !ok || row.Strikes != 2 || row.Until != testBase+60 || row.Reason != "status 429" {
		t.Fatalf("persisted 429 = %+v (present %v)", row, ok)
	}
	if store.saves != 1 {
		t.Fatalf("saves = %d, want 1 coalesced write", store.saves)
	}

	// 5xx persists the account-wide row.
	s.Report(key, "m1", 503, 0, 0)
	s.FlushCooldowns()
	row, ok = store.row("p", "a", "")
	if !ok || row.Until != testBase+60 || row.Reason != "status 503" {
		t.Fatalf("persisted 5xx = %+v (present %v)", row, ok)
	}

	// Success clears both rows.
	s.Report(key, "m1", 200, 50, 0)
	s.FlushCooldowns()
	if _, ok := store.row("p", "a", "m1"); ok {
		t.Fatal("model cooldown row survived a success")
	}
	if _, ok := store.row("p", "a", ""); ok {
		t.Fatal("account cooldown row survived a success")
	}

	// Close flushes without an explicit FlushCooldowns.
	s.Report(key, "m1", 429, 0, 0)
	s.Close()
	s.Close() // idempotent
	row, ok = store.row("p", "a", "m1")
	if !ok || row.Strikes != 1 || row.Until != testBase+30 {
		t.Fatalf("persisted on close = %+v (present %v)", row, ok)
	}
}

// TestSelectableCount mirrors A's healthz predicate: disabled, credential-less,
// depleted, expired and cooling accounts are not selectable; total counts every
// snapshot account.
func TestSelectableCount(t *testing.T) {
	now := testBase
	ok := account("ok")
	disabled := account("disabled")
	disabled.Enabled = false
	noCred := account("nocr")
	noCred.CredentialPresent = false
	depleted := account("depleted")
	depleted.Remaining, depleted.Total = 0, 100
	expired := account("expired")
	expired.Remaining, expired.Total, expired.ExpiresAt = 10, 100, now-1
	cooling := account("cooling")
	cooling.CooldownUntil = now + 60

	p := pool("p", ok, disabled, noCred, depleted, expired, cooling)
	off := pool("off", account("idle"))
	off.UpstreamEnabled = false

	s := New(Options{Now: func() time.Time { return time.Unix(now, 0) }})
	selectable, total := s.SelectableCount(snapOf(p, off))
	if selectable != 1 || total != 7 {
		t.Fatalf("selectable=%d total=%d, want 1/7", selectable, total)
	}

	// A 5xx cooldown hides the last selectable account.
	s.Report(AccountKey{PluginID: "p", AccountID: "ok"}, "m1", 500, 0, 0)
	if selectable, total := s.SelectableCount(snapOf(p, off)); selectable != 0 || total != 7 {
		t.Fatalf("after 5xx selectable=%d total=%d, want 0/7", selectable, total)
	}
}

// TestSelectableCountExcludesUnservablePluginStatus is the health half of the
// 2026-10-23 review finding: a plugin that reports "expired" with an empty quota
// block (minimax: quota={}) carried no data the credit filter could reject, so
// the account counted as selectable. The plugin status must veto the account.
// "invalid" is emitted when the plugin itself refuses the credential (codearts
// chat_accounts.go, officeace lifecycle.go); "ok", "rate_limited" and "unknown"
// stay governed by Enabled/cooldowns, not by this filter.
func TestSelectableCountExcludesUnservablePluginStatus(t *testing.T) {
	now := testBase
	expired := account("expired-status")
	expired.Status = "expired" // quota stays empty: the credit filter fails open
	invalid := account("invalid-status")
	invalid.Status = "invalid"
	rateLimited := account("rate-limited")
	rateLimited.Status = "rate_limited"
	unknown := account("unknown-status")
	unknown.Status = "unknown"

	s := New(Options{Now: func() time.Time { return time.Unix(now, 0) }})
	selectable, total := s.SelectableCount(snapOf(pool("p", expired, invalid, rateLimited, unknown)))
	if selectable != 2 || total != 4 {
		t.Fatalf("selectable=%d total=%d, want 2/4 (only expired/invalid are unservable)", selectable, total)
	}
}
