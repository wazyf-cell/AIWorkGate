package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"aiworkgate/internal/model"
)

const testBase = int64(1_700_000_000)

// clock is a mutable test clock.
type clock struct{ at int64 }

func (c *clock) now() time.Time        { return time.Unix(c.at, 0) }
func (c *clock) advance(seconds int64) { c.at += seconds }

// memSticky is an in-memory StickyStore.
type memSticky struct {
	rows map[stickyKey]StickyBinding
}

func newMemSticky() *memSticky { return &memSticky{rows: map[stickyKey]StickyBinding{}} }

func (m *memSticky) ListSticky(scope string) ([]StickyBinding, error) {
	out := []StickyBinding{}
	for k, v := range m.rows {
		if k.Scope == scope {
			out = append(out, v)
		}
	}
	return out, nil
}

func (m *memSticky) SaveSticky(b StickyBinding) error {
	m.rows[stickyKey{Scope: b.Scope, Key: b.Key}] = b
	return nil
}

func (m *memSticky) DeleteSticky(scope, key string) error {
	delete(m.rows, stickyKey{Scope: scope, Key: key})
	return nil
}

func (m *memSticky) DeleteExpiredSticky(now int64) (int64, error) {
	var n int64
	for k, v := range m.rows {
		if v.ExpiresAt <= now {
			delete(m.rows, k)
			n++
		}
	}
	return n, nil
}

func account(id string) Account {
	return Account{
		ID: id, Label: id, Enabled: true, CredentialPresent: true,
		Priority: 100, Weight: 1,
	}
}

func pool(id string, accounts ...Account) *Pool {
	return &Pool{
		PluginID: id, Name: id, APIVersion: 2, UpstreamEnabled: true,
		Strategy: StrategyFixed, PoolPriority: 0, AccountConcurrency: 0,
		Accounts: accounts,
		Models:   map[string]model.ModelInfo{"m1": {ID: "m1"}, "m2": {ID: "m2"}},
	}
}

func snapOf(pools ...*Pool) *Snapshot {
	s := &Snapshot{Pools: map[string]*Pool{}, Bindings: map[string]Binding{}, InterPool: InterPoolSmart}
	for _, p := range pools {
		s.Pools[p.PluginID] = p
	}
	return s
}

func pick(t *testing.T, s *Scheduler, snap *Snapshot, modelID string) Decision {
	t.Helper()
	decision, err := s.Pick(context.Background(), snap, Request{Model: modelID})
	if err != nil {
		t.Fatalf("Pick(%s): %v", modelID, err)
	}
	return decision
}

func TestInPoolStrategies(t *testing.T) {
	now := testBase
	a := account("a")
	a.ExpiresAt = now + 3*86400
	a.Remaining, a.Total, a.Priority = 100, 1000, 20
	b := account("b")
	b.ExpiresAt = now + 86400
	b.Remaining, b.Total, b.Priority = 50, 1000, 10
	c := account("c")
	c.Remaining, c.Total, c.Priority = 900, 1000, 5

	mk := func(strategy string, rnd float64, withRand bool) *Scheduler {
		opts := Options{Now: func() time.Time { return time.Unix(now, 0) }}
		if withRand {
			opts.Rand = func() float64 { return rnd }
		}
		return New(opts)
	}

	cases := []struct {
		name     string
		strategy string
		rand     float64
		withRand bool
		want     string
	}{
		{"credit_first", StrategyCreditFirst, 0, false, "b"},
		{"remaining_first", StrategyRemainingFirst, 0, false, "c"},
		{"fixed", StrategyFixed, 0, false, "c"},
		{"random first", StrategyRandom, 0, true, "a"},
		{"random last", StrategyRandom, 0.99, true, "c"},
		{"weighted3", StrategyWeighted3, 0.5, true, "b"},
		{"p2c", StrategyP2C, 0, true, "b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := pool("p", a, b, c)
			p.Strategy = tc.strategy
			s := mk(tc.strategy, tc.rand, tc.withRand)
			got := pick(t, s, snapOf(p), "p/m1")
			if got.AccountID != tc.want {
				t.Fatalf("strategy %s picked %s, want %s (reasons %v)", tc.strategy, got.AccountID, tc.want, got.Reasons)
			}
		})
	}
}

func TestInterPoolSmartAndFixed(t *testing.T) {
	now := testBase
	low := pool("p-low", account("a1"))
	low.Models["m1"] = model.ModelInfo{ID: "m1", Multiplier: 2}
	low.PoolPriority = 1
	low.Accounts[0].ExpiresAt = now + 10*86400
	low.Accounts[0].Remaining, low.Accounts[0].Total = 1000, 1000

	fast := pool("p-fast", account("a2"))
	fast.Models["m1"] = model.ModelInfo{ID: "m1", Multiplier: 1}
	fast.PoolPriority = 2
	fast.Accounts[0].ExpiresAt = now + 20*86400
	fast.Accounts[0].Remaining, fast.Accounts[0].Total = 100, 1000

	soon := pool("p-soon", account("a3"))
	soon.Models["m1"] = model.ModelInfo{ID: "m1", Multiplier: 1.5}
	soon.PoolPriority = 3
	soon.Accounts[0].ExpiresAt = now + 86400
	soon.Accounts[0].Remaining, soon.Accounts[0].Total = 500, 1000

	snap := snapOf(low, fast, soon)
	snap.Bindings["logical"] = Binding{
		Logical: "logical", Display: "Logical",
		Items: []Source{
			{PluginID: "p-low", Model: "m1"},
			{PluginID: "p-fast", Model: "m1"},
			{PluginID: "p-soon", Model: "m1"},
		},
	}
	s := New(Options{Now: func() time.Time { return time.Unix(now, 0) }})

	// Smart: soonest credit expiry wins.
	if got := pick(t, s, snap, "logical"); got.PluginID != "p-soon" {
		t.Fatalf("smart picked %s, want p-soon (%v)", got.PluginID, got.Reasons)
	}

	// Without an expiry the cheaper multiplier wins.
	soon.Accounts[0].ExpiresAt = 0
	low.Accounts[0].ExpiresAt = 0
	fast.Accounts[0].ExpiresAt = 0
	if got := pick(t, s, snap, "logical"); got.PluginID != "p-fast" {
		t.Fatalf("smart multiplier picked %s, want p-fast (%v)", got.PluginID, got.Reasons)
	}

	// Equal multiplier/expiry: more remaining wins.
	low.Accounts[0].ExpiresAt = now + 20*86400
	fast.Accounts[0].ExpiresAt = now + 20*86400
	soon.Accounts[0].ExpiresAt = now + 20*86400
	low.Models["m1"] = model.ModelInfo{ID: "m1", Multiplier: 1}
	soon.Models["m1"] = model.ModelInfo{ID: "m1", Multiplier: 1}
	if got := pick(t, s, snap, "logical"); got.PluginID != "p-low" {
		t.Fatalf("smart remaining picked %s, want p-low (%v)", got.PluginID, got.Reasons)
	}

	// Fixed keeps the binding order regardless of the scores.
	snap.InterPool = InterPoolFixed
	if got := pick(t, s, snap, "logical"); got.PluginID != "p-low" || got.LogicalModel != "logical" {
		t.Fatalf("fixed picked %+v, want p-low/logical", got)
	}

	// Physical ids bypass the binding entirely.
	if got := pick(t, s, snap, "p-fast/m1"); got.PluginID != "p-fast" || got.LogicalModel != "" {
		t.Fatalf("physical = %+v", got)
	}
}

func TestPoolPriorityBreaksTies(t *testing.T) {
	now := testBase
	p1 := pool("p1", account("a1"))
	p2 := pool("p2", account("a2"))
	for _, p := range []*Pool{p1, p2} {
		p.Accounts[0].ExpiresAt = now + 86400
		p.Accounts[0].Remaining, p.Accounts[0].Total = 100, 1000
	}
	p1.PoolPriority = 20
	p2.PoolPriority = 10
	snap := snapOf(p1, p2)
	snap.Bindings["l"] = Binding{Logical: "l", Items: []Source{
		{PluginID: "p1", Model: "m1"}, {PluginID: "p2", Model: "m1"},
	}}
	s := New(Options{Now: func() time.Time { return time.Unix(now, 0) }})
	if got := pick(t, s, snap, "l"); got.PluginID != "p2" {
		t.Fatalf("priority tie picked %s, want p2", got.PluginID)
	}
}

func TestAccountFiltersAndReasons(t *testing.T) {
	now := testBase
	disabled := account("disabled")
	disabled.Enabled = false
	noCred := account("nocred")
	noCred.CredentialPresent = false
	cooling := account("cooling")
	cooling.CooldownUntil = now + 100
	modelCool := account("modelcool")
	modelCool.ModelCooldowns = map[string]int64{"m1": now + 100}
	outside := account("outside")

	p := pool("p", disabled, noCred, cooling, modelCool, outside)
	p.Strategy = StrategyCreditFirst
	p.Members = map[string]bool{"disabled": true, "nocred": true, "cooling": true, "modelcool": true}
	s := New(Options{Now: func() time.Time { return time.Unix(now, 0) }})
	snap := snapOf(p)

	_, err := s.Pick(context.Background(), snap, Request{Model: "p/m1"})
	if !errors.Is(err, ErrNoCandidate) {
		t.Fatalf("Pick err = %v, want ErrNoCandidate", err)
	}
	for _, want := range []string{"not a pool member", "account disabled", "no credential", "account cooldown", "model cooldown"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}

	// The same model is fine for an account whose cooldown is account-wide only
	// after it expires; excluded accounts are skipped during retries.
	p.Members = nil
	decision, err := s.Pick(context.Background(), snap, Request{
		Model: "p/m1",
		Exclude: map[AccountKey]bool{
			{PluginID: "p", AccountID: "modelcool"}: true,
			{PluginID: "p", AccountID: "cooling"}:   true,
			{PluginID: "p", AccountID: "outside"}:   true,
		},
	})
	if !errors.Is(err, ErrNoCandidate) {
		t.Fatalf("Pick with all excluded = %v, want ErrNoCandidate (decision %+v)", err, decision)
	}

	// m2 is not on model cooldown, so only the account-level filters apply.
	decision = pick(t, s, snap, "p/m2")
	if decision.AccountID != "modelcool" && decision.AccountID != "outside" {
		t.Fatalf("m2 picked %s, want modelcool/outside", decision.AccountID)
	}
}

func TestUpstreamDisabledAndUnknownPlugin(t *testing.T) {
	p := pool("p", account("a"))
	p.UpstreamEnabled = false
	snap := snapOf(p)
	s := New(Options{Now: func() time.Time { return time.Unix(testBase, 0) }})
	if _, err := s.Pick(context.Background(), snap, Request{Model: "p/m1"}); !errors.Is(err, ErrNoCandidate) ||
		!strings.Contains(err.Error(), "upstream disabled") {
		t.Fatalf("err = %v", err)
	}
	if _, err := s.Pick(context.Background(), snap, Request{Model: "ghost/m1"}); !errors.Is(err, ErrNoCandidate) {
		t.Fatalf("unknown plugin err = %v", err)
	}
}

func TestUnknownModel(t *testing.T) {
	s := New(Options{})
	for _, name := range []string{"", "missing", "missing/", "/m1"} {
		_, err := s.Pick(context.Background(), snapOf(pool("p", account("a"))), Request{Model: name})
		if !errors.Is(err, ErrUnknownModel) {
			t.Fatalf("Pick(%q) err = %v, want ErrUnknownModel", name, err)
		}
	}
	if _, err := s.Pick(context.Background(), nil, Request{Model: "p/m1"}); !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("nil snapshot err = %v", err)
	}
}

func TestConversationSticky(t *testing.T) {
	now := testBase
	a := account("a")
	a.Priority = 1
	a.Remaining, a.Total = 1000, 1000
	b := account("b")
	b.Priority = 2
	b.Remaining, b.Total = 100, 1000
	p := pool("p", a, b)
	p.Strategy = StrategyRemainingFirst
	p.ConvStickyTTL = 900
	clk := &clock{at: now}
	store := newMemSticky()
	s := New(Options{Now: clk.now, Sticky: store})
	snap := snapOf(p)

	first, err := s.Pick(context.Background(), snap, Request{Model: "p/m1", ConversationID: "conv-1"})
	if err != nil || first.AccountID != "a" || first.Sticky {
		t.Fatalf("first = %+v, %v", first, err)
	}
	// Make b the better account; the conversation pin must still win. a keeps a
	// sliver of credit so the credit-eligibility filter does not drop it.
	p.Accounts[0].Remaining = 1
	p.Accounts[1].Remaining = 900
	second, err := s.Pick(context.Background(), snap, Request{Model: "p/m1", ConversationID: "conv-1"})
	if err != nil {
		t.Fatal(err)
	}
	if second.AccountID != "a" || !second.Sticky {
		t.Fatalf("sticky = %+v, want a", second)
	}
	if len(store.rows) == 0 {
		t.Fatal("sticky binding not persisted")
	}

	// A retry exclusion skips the pinned account without destroying the pin.
	excluded, err := s.Pick(context.Background(), snap, Request{
		Model:          "p/m1",
		ConversationID: "conv-1",
		Exclude:        map[AccountKey]bool{{PluginID: "p", AccountID: "a"}: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if excluded.AccountID != "b" || excluded.Sticky {
		t.Fatalf("excluded sticky = %+v, want b", excluded)
	}
	if row := store.rows[stickyKey{Scope: StickyScopeConversation, Key: "conv-1"}]; row.AccountID != "a" {
		t.Fatalf("sticky pin was destroyed by a retry: %+v", row)
	}

	// Expiry selects fresh and removes the row.
	clk.advance(901)
	third, err := s.Pick(context.Background(), snap, Request{Model: "p/m1", ConversationID: "conv-1"})
	if err != nil {
		t.Fatal(err)
	}
	if third.AccountID != "b" || third.Sticky {
		t.Fatalf("expired sticky = %+v, want b", third)
	}

	// An invalid pin (disabled account) is dropped and reselected.
	store.rows[stickyKey{Scope: StickyScopeConversation, Key: "conv-2"}] = StickyBinding{
		Scope: StickyScopeConversation, Key: "conv-2", PluginID: "p", AccountID: "a", ExpiresAt: now + 1000,
	}
	s2 := New(Options{Now: clk.now, Sticky: store})
	p.Accounts[0].Enabled = false
	fourth, err := s2.Pick(context.Background(), snap, Request{Model: "p/m1", ConversationID: "conv-2"})
	if err != nil {
		t.Fatal(err)
	}
	if fourth.AccountID != "b" || fourth.Sticky {
		t.Fatalf("invalid sticky = %+v, want b", fourth)
	}
	// The invalid pin was replaced by a fresh pin for the newly selected account.
	row, ok := store.rows[stickyKey{Scope: StickyScopeConversation, Key: "conv-2"}]
	if !ok || row.AccountID != "b" {
		t.Fatalf("sticky row = %+v, %v; want a fresh pin to b", row, ok)
	}
}

func TestPoolSticky(t *testing.T) {
	now := testBase
	a := account("a")
	a.Priority = 1
	a.Remaining, a.Total = 1000, 1000
	b := account("b")
	b.Priority = 2
	b.Remaining, b.Total = 100, 1000
	p := pool("p", a, b)
	p.Strategy = StrategyRemainingFirst
	p.PoolStickyTTL = 600
	clk := &clock{at: now}
	store := newMemSticky()
	s := New(Options{Now: clk.now, Sticky: store})
	snap := snapOf(p)

	first, err := s.Pick(context.Background(), snap, Request{Model: "p/m1", ClientKey: "ck_1"})
	if err != nil || first.AccountID != "a" {
		t.Fatalf("first = %+v, %v", first, err)
	}
	p.Accounts[0].Remaining = 1 // still funded, so the pin is not filtered
	p.Accounts[1].Remaining = 1000
	second, err := s.Pick(context.Background(), snap, Request{Model: "p/m1", ClientKey: "ck_1"})
	if err != nil || second.AccountID != "a" || !second.Sticky {
		t.Fatalf("pool sticky = %+v, %v", second, err)
	}
	// A different physical model is a different pool key.
	third, err := s.Pick(context.Background(), snap, Request{Model: "p/m2", ClientKey: "ck_1"})
	if err != nil || third.AccountID != "b" || third.Sticky {
		t.Fatalf("other model = %+v, %v", third, err)
	}
	// TTL 0 disables the pin.
	p.PoolStickyTTL = 0
	fourth, err := s.Pick(context.Background(), snap, Request{Model: "p/m1", ClientKey: "ck_1"})
	if err != nil || fourth.AccountID != "b" || fourth.Sticky {
		t.Fatalf("disabled ttl = %+v, %v", fourth, err)
	}
}

func TestConcurrencyLimits(t *testing.T) {
	now := testBase
	a := account("a")
	a.Priority = 1
	b := account("b")
	b.Priority = 2
	p := pool("p", a, b)
	p.Strategy = StrategyFixed
	p.AccountConcurrency = 1
	s := New(Options{Now: func() time.Time { return time.Unix(now, 0) }})
	snap := snapOf(p)

	// a is free first.
	if got := pick(t, s, snap, "p/m1"); got.AccountID != "a" {
		t.Fatalf("picked %s", got.AccountID)
	}
	s.Acquire(AccountKey{PluginID: "p", AccountID: "a"})
	if got := pick(t, s, snap, "p/m1"); got.AccountID != "b" {
		t.Fatalf("picked %s, want b when a is full", got.AccountID)
	}
	// Both full: least loaded wins even when fixed would pick a.
	s.Acquire(AccountKey{PluginID: "p", AccountID: "b"})
	s.Acquire(AccountKey{PluginID: "p", AccountID: "b"})
	got := pick(t, s, snap, "p/m1")
	if got.AccountID != "a" {
		t.Fatalf("least loaded = %s, want a", got.AccountID)
	}
	if !strings.Contains(strings.Join(got.Reasons, " "), "least loaded") {
		t.Fatalf("reasons = %v", got.Reasons)
	}
	// 0 disables the cap.
	p.AccountConcurrency = 0
	s.Release(AccountKey{PluginID: "p", AccountID: "a"})
	s.Acquire(AccountKey{PluginID: "p", AccountID: "a"})
	s.Acquire(AccountKey{PluginID: "p", AccountID: "a"})
	if got := pick(t, s, snap, "p/m1"); got.AccountID != "a" {
		t.Fatalf("unlimited picked %s", got.AccountID)
	}
	// In-flight counters never go negative.
	s.Release(AccountKey{PluginID: "p", AccountID: "a"})
	s.Release(AccountKey{PluginID: "p", AccountID: "a"})
	s.Release(AccountKey{PluginID: "p", AccountID: "a"})
	if n := s.InFlight(AccountKey{PluginID: "p", AccountID: "a"}); n != 0 {
		t.Fatalf("in flight = %d", n)
	}
}

func TestReportCooldownsEWMAAndCircuit(t *testing.T) {
	now := testBase
	clk := &clock{at: now}
	a := account("a")
	a.Priority = 1
	b := account("b")
	b.Priority = 2
	p := pool("p", a, b)
	p.Strategy = StrategyFixed
	s := New(Options{Now: clk.now})
	snap := snapOf(p)
	keyA := AccountKey{PluginID: "p", AccountID: "a"}

	// 429 hits one model only.
	s.Report(keyA, "m1", 429, 0, 0)
	if got := pick(t, s, snap, "p/m1"); got.AccountID != "b" {
		t.Fatalf("m1 after 429 = %s, want b", got.AccountID)
	}
	if got := pick(t, s, snap, "p/m2"); got.AccountID != "a" {
		t.Fatalf("m2 after 429 = %s, want a", got.AccountID)
	}
	if got := s.modelCD[modelCooldownKey{Key: keyA, Model: "m1"}].Until; got != now+30 {
		t.Fatalf("429 cooldown until = %d, want %d", got, now+30)
	}
	// Consecutive 429 doubles the model cooldown, capped at 10m.
	s.Report(keyA, "m1", 429, 0, 0)
	if got := s.modelCD[modelCooldownKey{Key: keyA, Model: "m1"}].Until; got != now+60 {
		t.Fatalf("second 429 = %d, want %d", got, now+60)
	}
	for i := 0; i < 10; i++ {
		s.Report(keyA, "m1", 429, 0, 0)
	}
	if state := s.modelCD[modelCooldownKey{Key: keyA, Model: "m1"}]; state.Until != now+cooldown429Max {
		t.Fatalf("capped 429 = %+v", state)
	}

	// 5xx is account-wide and counts toward the circuit breaker.
	clk.at = now + 3600
	s.Report(keyA, "m1", 500, 0, 0)
	if got := pick(t, s, snap, "p/m2"); got.AccountID != "b" {
		t.Fatalf("after 5xx = %s, want b", got.AccountID)
	}
	s.Report(keyA, "m1", 500, 0, 0)
	s.Report(keyA, "m1", 500, 0, 0)
	if !s.CircuitOpen("p") {
		t.Fatal("circuit not open after 3 consecutive 5xx")
	}
	if _, err := s.Pick(context.Background(), snap, Request{Model: "p/m1"}); !errors.Is(err, ErrNoCandidate) ||
		!strings.Contains(err.Error(), "circuit open") {
		t.Fatalf("circuit err = %v", err)
	}
	clk.advance(61)
	// Account cooldowns expired and the circuit closed.
	if got := pick(t, s, snap, "p/m1"); got.AccountID != "a" {
		t.Fatalf("after circuit = %s", got.AccountID)
	}

	// EWMA: first sample initializes, later samples blend with alpha 0.3.
	s.Report(keyA, "m1", 200, 100, 1.5)
	if got := s.EWMALatency(keyA); got != 100 {
		t.Fatalf("ewma = %v", got)
	}
	s.Report(keyA, "m1", 200, 300, 0.5)
	if got := s.EWMALatency(keyA); got != 160 {
		t.Fatalf("ewma = %v, want 160", got)
	}
	if got := s.Credits(keyA); got != 2 {
		t.Fatalf("credits = %v, want 2", got)
	}
}

func TestExplainReasons(t *testing.T) {
	now := testBase
	a := account("a")
	a.Remaining = 10
	b := account("b")
	b.Remaining = 900
	off := account("off")
	off.Enabled = false
	p := pool("p", a, off, b)
	p.Strategy = StrategyRemainingFirst
	s := New(Options{Now: func() time.Time { return time.Unix(now, 0) }})

	expl, err := s.Explain(context.Background(), snapOf(p), Request{Model: "p/m1"})
	if err != nil {
		t.Fatal(err)
	}
	if expl.Decision == nil || expl.Decision.AccountID != "b" {
		t.Fatalf("decision = %+v", expl.Decision)
	}
	var selected, rejected bool
	for _, c := range expl.Candidates {
		if c.Selected && c.AccountID == "b" {
			selected = true
		}
		if c.Rejected && c.AccountID == "off" && c.Reason == "account disabled" {
			rejected = true
		}
	}
	if !selected || !rejected {
		t.Fatalf("candidates = %+v", expl.Candidates)
	}

	// No candidate still explains why.
	p.Accounts = []Account{off}
	expl, err = s.Explain(context.Background(), snapOf(p), Request{Model: "p/m1"})
	if !errors.Is(err, ErrNoCandidate) || expl.Decision != nil {
		t.Fatalf("explain = %+v, %v", expl, err)
	}
	if len(expl.Candidates) == 0 || !strings.Contains(expl.Reasons[0], "no eligible") {
		t.Fatalf("reasons = %+v candidates = %+v", expl.Reasons, expl.Candidates)
	}
}

func TestStickyStorePreloadedAndForget(t *testing.T) {
	now := testBase
	a := account("a")
	a.Priority = 1
	b := account("b")
	b.Priority = 2
	p := pool("p", a, b)
	p.Strategy = StrategyFixed
	p.ConvStickyTTL = 600
	store := newMemSticky()
	_ = store.SaveSticky(StickyBinding{
		Scope: StickyScopeConversation, Key: "c9", PluginID: "p", AccountID: "b", ExpiresAt: now + 100,
	})
	s := New(Options{Now: func() time.Time { return time.Unix(now, 0) }, Sticky: store})
	decision, err := s.Pick(context.Background(), snapOf(p), Request{Model: "p/m1", ConversationID: "c9"})
	if err != nil || decision.AccountID != "b" || !decision.Sticky {
		t.Fatalf("preloaded = %+v, %v", decision, err)
	}
	if len(s.CachedSticky()) != 1 {
		t.Fatalf("cache = %+v", s.CachedSticky())
	}
	s.ForgetAllSticky()
	if len(s.CachedSticky()) != 0 {
		t.Fatalf("cache after forget = %+v", s.CachedSticky())
	}
}

// TestCreditEligibilityPrefersFundedAccounts ports A pool.rs:1135-1164: a
// depleted or expired account must not be chosen while a funded one exists.
func TestCreditEligibilityPrefersFundedAccounts(t *testing.T) {
	now := testBase
	depleted := account("depleted")
	depleted.Priority = 1
	depleted.Remaining, depleted.Total = 0, 100 // known balance, fully spent
	expired := account("expired")
	expired.Priority = 2
	expired.Remaining, expired.Total = 50, 100
	expired.ExpiresAt = now - 60
	funded := account("funded")
	funded.Priority = 3
	funded.Remaining, funded.Total = 100, 100
	funded.ExpiresAt = now + 86400

	p := pool("p", depleted, expired, funded)
	p.Strategy = StrategyFixed // fixed would pick the depleted account first
	s := New(Options{Now: func() time.Time { return time.Unix(now, 0) }})

	expl, err := s.Explain(context.Background(), snapOf(p), Request{Model: "p/m1"})
	if err != nil {
		t.Fatal(err)
	}
	if expl.Decision == nil || expl.Decision.AccountID != "funded" {
		t.Fatalf("decision = %+v, want funded", expl.Decision)
	}
	reasons := map[string]string{}
	for _, c := range expl.Candidates {
		if c.Rejected {
			reasons[c.AccountID] = c.Reason
		}
	}
	if reasons["depleted"] != "credits depleted" {
		t.Fatalf("depleted reason = %q", reasons["depleted"])
	}
	if !strings.HasPrefix(reasons["expired"], "credits expired at ") {
		t.Fatalf("expired reason = %q", reasons["expired"])
	}

	// Fail open: an expired/depleted account alone must still be usable, a total
	// outage is worse than calling an account whose balance may be stale.
	p.Accounts = []Account{depleted}
	if got := pick(t, s, snapOf(p), "p/m1"); got.AccountID != "depleted" {
		t.Fatalf("depleted-only picked %s, want depleted", got.AccountID)
	}
	p.Accounts = []Account{expired}
	if got := pick(t, s, snapOf(p), "p/m1"); got.AccountID != "expired" {
		t.Fatalf("expired-only picked %s, want expired", got.AccountID)
	}

	// An account without any quota data (zero struct) is unknown, not depleted.
	unknown := account("unknown")
	p.Accounts = []Account{unknown, depleted}
	p.Strategy = StrategyFixed
	if got := pick(t, s, snapOf(p), "p/m1"); got.AccountID != "unknown" {
		t.Fatalf("unknown-credit account picked %s, want unknown", got.AccountID)
	}
}

// TestStickyYieldUnderSaturation ports A pool.rs:704-790: a pinned conversation
// yields to an idle account when the pin is at its concurrency cap, and keeps
// the pin when every account is busy.
func TestStickyYieldUnderSaturation(t *testing.T) {
	now := testBase
	a := account("a")
	a.Priority = 1
	b := account("b")
	b.Priority = 2
	p := pool("p", a, b)
	p.Strategy = StrategyFixed
	p.ConvStickyTTL = 900
	p.AccountConcurrency = 1
	clk := &clock{at: now}
	s := New(Options{Now: clk.now, Sticky: newMemSticky()})
	snap := snapOf(p)

	first, err := s.Pick(context.Background(), snap, Request{Model: "p/m1", ConversationID: "conv"})
	if err != nil || first.AccountID != "a" {
		t.Fatalf("first = %+v, %v", first, err)
	}

	// a reaches its cap while b sits idle: the sticky binding yields for this
	// request instead of overrunning a's slot limit.
	s.Acquire(AccountKey{PluginID: "p", AccountID: "a"})
	yielded, err := s.Pick(context.Background(), snap, Request{Model: "p/m1", ConversationID: "conv"})
	if err != nil {
		t.Fatal(err)
	}
	if yielded.AccountID != "b" || yielded.Sticky {
		t.Fatalf("saturated sticky = %+v, want b without stickiness", yielded)
	}
	if !strings.Contains(strings.Join(yielded.Reasons, " "), "sticky yield") {
		t.Fatalf("reasons = %v, want a sticky yield note", yielded.Reasons)
	}

	// Every account at the cap: waiting on the pinned account beats breaking
	// affinity, so stickiness is kept.
	s.Acquire(AccountKey{PluginID: "p", AccountID: "b"})
	held, err := s.Pick(context.Background(), snap, Request{Model: "p/m1", ConversationID: "conv"})
	if err != nil {
		t.Fatal(err)
	}
	if held.AccountID != "b" || !held.Sticky {
		t.Fatalf("all-busy sticky = %+v, want the pinned b", held)
	}
	if !strings.Contains(strings.Join(held.Reasons, " "), "sticky fallback") {
		t.Fatalf("reasons = %v, want a sticky fallback note", held.Reasons)
	}
}

func TestSmartNoExpiryIsLast(t *testing.T) {
	now := testBase
	noExpiry := pool("p-none", account("a1"))
	noExpiry.Accounts[0].Remaining, noExpiry.Accounts[0].Total = 10000, 10000
	dated := pool("p-date", account("a2"))
	dated.Accounts[0].ExpiresAt = now + 30*86400
	dated.Accounts[0].Remaining, dated.Accounts[0].Total = 1, 1000
	snap := snapOf(noExpiry, dated)
	snap.Bindings["l"] = Binding{Logical: "l", Items: []Source{
		{PluginID: "p-none", Model: "m1"}, {PluginID: "p-date", Model: "m1"},
	}}
	s := New(Options{Now: func() time.Time { return time.Unix(now, 0) }})
	if got := pick(t, s, snap, "l"); got.PluginID != "p-date" {
		t.Fatalf("picked %s, want the pool with an expiry", got.PluginID)
	}
}

// A logical model name may contain a slash (e.g. "vendor/model"). When the
// prefix is not an installed pool, the binding must win over prefix parsing;
// otherwise /v1/models advertises a name no request can route.
func TestResolveModelKeepsSlashBindingWhenPrefixIsNotAPool(t *testing.T) {
	snap := &Snapshot{
		Bindings: map[string]Binding{
			"vendor/model": {Logical: "vendor/model", Items: []Source{{PluginID: "p1", Model: "m1"}}},
		},
		Pools: map[string]*Pool{},
	}
	logical, sources, err := resolveModel(snap, "vendor/model")
	if err != nil {
		t.Fatalf("resolveModel: %v", err)
	}
	if logical != "vendor/model" || len(sources) != 1 || sources[0].PluginID != "p1" || sources[0].Model != "m1" {
		t.Fatalf("resolved = %q %+v", logical, sources)
	}
}

// An installed plugin still owns its prefix, even if a same-named binding
// exists; the gateway's matchPlugin path takes the same precedence.
func TestResolveModelPrefersInstalledPluginPrefix(t *testing.T) {
	snap := &Snapshot{
		Bindings: map[string]Binding{
			"p1/m1": {Logical: "p1/m1", Items: []Source{{PluginID: "p2", Model: "other"}}},
		},
		Pools: map[string]*Pool{"p1": {PluginID: "p1"}},
	}
	_, sources, err := resolveModel(snap, "p1/m1")
	if err != nil {
		t.Fatalf("resolveModel: %v", err)
	}
	if len(sources) != 1 || sources[0].PluginID != "p1" || sources[0].Model != "m1" {
		t.Fatalf("sources = %+v, want the plugin prefix", sources)
	}
}

// An unlimited plan reports no meaningful Remaining/Total but still carries a
// real subscription expiry. The zero balance must not be read as depleted.
func TestCreditIneligibleKeepsUnlimitedPlans(t *testing.T) {
	now := int64(1_000)
	if got := creditIneligibleReason(Account{Unlimited: true, Remaining: 0, ExpiresAt: now + 100}, now); got != "" {
		t.Fatalf("unlimited unlimited plan = %q, want eligible", got)
	}
	if got := creditIneligibleReason(Account{Unlimited: true, Remaining: 0, ExpiresAt: now - 1}, now); got == "" {
		t.Fatal("an expired unlimited plan must stay ineligible")
	}
	if got := creditIneligibleReason(Account{Remaining: 0, Total: 10, ExpiresAt: now + 100}, now); got == "" {
		t.Fatal("a metered zero balance must still be treated as depleted")
	}
}
