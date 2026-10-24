package schedule

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"aiworkgate/internal/hosts"
	"aiworkgate/internal/model"
	"aiworkgate/internal/store"
)

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

type pluginCall struct {
	plugin  string
	account string
}

type fakePlugins struct {
	mu         sync.Mutex
	statuses   []model.PluginStatus
	accounts   map[string][]model.Account
	accountErr map[string]error
	times      map[string]string
	checkinFn  func(pluginID, accountID string) (model.CheckinResult, error)
	calls      []pluginCall
	accountHit int
}

func (f *fakePlugins) List() []model.PluginStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]model.PluginStatus(nil), f.statuses...)
}

func (f *fakePlugins) Accounts(_ context.Context, pluginID string) ([]model.Account, error) {
	f.mu.Lock()
	f.accountHit++
	err := f.accountErr[pluginID]
	accounts := append([]model.Account(nil), f.accounts[pluginID]...)
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return accounts, nil
}

func (f *fakePlugins) Checkin(_ context.Context, pluginID, accountID string) (model.CheckinResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, pluginCall{plugin: pluginID, account: accountID})
	fn := f.checkinFn
	f.mu.Unlock()
	if fn != nil {
		return fn(pluginID, accountID)
	}
	return model.CheckinResult{Result: model.CheckinOK}, nil
}

func (f *fakePlugins) CheckinTime(pluginID string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	value, ok := f.times[pluginID]
	return value, ok
}

func (f *fakePlugins) checkinCalls() []pluginCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pluginCall(nil), f.calls...)
}

func (f *fakePlugins) accountsHit() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.accountHit
}

type fakeA struct {
	mu      sync.Mutex
	path    string
	tasks   []string
	exeArgs []string
	results map[string]hosts.TaskResult
	errs    map[string]error
}

func (a *fakeA) ExePath() string { return a.path }

func (a *fakeA) RunTask(_ context.Context, exePath, taskKey string) (hosts.TaskResult, error) {
	a.mu.Lock()
	a.tasks = append(a.tasks, taskKey)
	a.exeArgs = append(a.exeArgs, exePath)
	result, hasResult := a.results[taskKey]
	err := a.errs[taskKey]
	a.mu.Unlock()
	if err != nil {
		return result, err
	}
	if !hasResult {
		result = hosts.TaskResult{Task: taskKey, OK: true, Summary: "ok=1 already=0 failed=0"}
	}
	return result, nil
}

func (a *fakeA) taskKeys() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.tasks...)
}

type notifyEvent struct {
	kind  string
	title string
	body  string
}

type fakeNotifier struct {
	mu     sync.Mutex
	events []notifyEvent
}

func (n *fakeNotifier) Notify(kind, title, body string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.events = append(n.events, notifyEvent{kind: kind, title: title, body: body})
}

func (n *fakeNotifier) snapshot() []notifyEvent {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]notifyEvent(nil), n.events...)
}

func (n *fakeNotifier) kinds() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]string, 0, len(n.events))
	for _, e := range n.events {
		out = append(out, e.kind)
	}
	return out
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(at time.Time) *fakeClock { return &fakeClock{t: at} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = at
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type fakeSleeper struct {
	mu     sync.Mutex
	clock  *fakeClock
	delays []time.Duration
}

func (s *fakeSleeper) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.delays = append(s.delays, d)
	s.mu.Unlock()
	if s.clock != nil {
		s.clock.Advance(d)
	}
	return nil
}

func (s *fakeSleeper) recorded() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.delays...)
}

func (s *fakeSleeper) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delays = nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

var testDay = time.Date(2026, 10, 20, 0, 0, 0, 0, time.Local)

func at(hour, minute int) time.Time {
	return time.Date(testDay.Year(), testDay.Month(), testDay.Day(), hour, minute, 0, 0, time.Local)
}

func openScheduleStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	return s
}

func newTestManager(t *testing.T, plugins Plugins, st *store.Store, clock *fakeClock, notifier *fakeNotifier) (*Manager, *fakeSleeper) {
	t.Helper()
	sleeper := &fakeSleeper{clock: clock}
	m := &Manager{
		Plugins: plugins,
		Store:   st,
		Notify:  notifier,
		Now:     clock.Now,
		Sleep:   sleeper.Sleep,
	}
	return m, sleeper
}

func makeStatus(id string, enabled bool, capabilities []string, checkinTime string, pollMinutes int) model.PluginStatus {
	return model.PluginStatus{
		Enabled: enabled,
		Manifest: model.Manifest{
			ID:           id,
			Capabilities: capabilities,
			Checkin:      model.CheckinConfig{DefaultTime: checkinTime},
			Quota:        model.QuotaConfig{PollMinutes: intPtr(pollMinutes)},
		},
	}
}

func makeAccount(id string, enabled bool, remaining, total float64) model.Account {
	return model.Account{
		ID:      id,
		Label:   "label-" + id,
		Enabled: enabled,
		Status:  model.AccountOK,
		Quota:   model.Quota{Remaining: remaining, Total: total},
	}
}

func mustRecords(t *testing.T, st *store.Store, pluginID, from, to string) []model.CheckinRecord {
	t.Helper()
	records, err := st.QueryCheckinRecords(pluginID, from, to)
	if err != nil {
		t.Fatalf("QueryCheckinRecords: %v", err)
	}
	return records
}

func recordSuccess(t *testing.T, st *store.Store, pluginID, accountID, date, result string) {
	t.Helper()
	_, err := st.UpsertCheckinRecord(model.CheckinRecord{
		PluginID:  pluginID,
		AccountID: accountID,
		Date:      date,
		Result:    result,
		At:        testDay.Add(8 * time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("UpsertCheckinRecord: %v", err)
	}
}

// ---------------------------------------------------------------------------
// due detection
// ---------------------------------------------------------------------------

func TestDueDetectionAndIdempotency(t *testing.T) {
	clock := newFakeClock(at(8, 59))
	before, after := 10.0, 12.0
	plugins := &fakePlugins{
		statuses: []model.PluginStatus{makeStatus("p1", true, []string{model.CapCheckin}, "09:00", 0)},
		accounts: map[string][]model.Account{"p1": {makeAccount("acc1", true, 0, 0)}},
		checkinFn: func(_, _ string) (model.CheckinResult, error) {
			return model.CheckinResult{Result: model.CheckinOK, CreditsBefore: &before, CreditsAfter: &after}, nil
		},
	}
	st := openScheduleStore(t)
	notifier := &fakeNotifier{}
	m, sleeper := newTestManager(t, plugins, st, clock, notifier)

	m.runPass(context.Background(), false)
	if got := len(plugins.checkinCalls()); got != 0 {
		t.Fatalf("before due time check-ins = %d, want 0", got)
	}

	clock.Set(at(9, 0))
	m.runPass(context.Background(), false)
	if got := len(plugins.checkinCalls()); got != 1 {
		t.Fatalf("at due time check-ins = %d, want 1", got)
	}
	records := mustRecords(t, st, "p1", "2026-10-20", "2026-10-20")
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	rec := records[0]
	if rec.Result != model.CheckinOK || rec.AccountID != "acc1" || rec.Date != "2026-10-20" {
		t.Fatalf("record = %+v", rec)
	}
	if rec.CreditsBefore == nil || *rec.CreditsBefore != before || rec.CreditsAfter == nil || *rec.CreditsAfter != after {
		t.Fatalf("credits before/after not recorded: %+v", rec)
	}
	if rec.At != clock.Now().Unix() {
		t.Fatalf("record At = %d, want %d", rec.At, clock.Now().Unix())
	}

	// Same day, already a success: the pass must not check in again.
	m.runPass(context.Background(), false)
	if got := len(plugins.checkinCalls()); got != 1 {
		t.Fatalf("after success check-ins = %d, want 1", got)
	}
	if got := len(notifier.snapshot()); got != 0 {
		t.Fatalf("notifications = %v, want none", got)
	}
	if got := len(sleeper.recorded()); got != 0 {
		t.Fatalf("sleeps = %v, want none", got)
	}
}

func TestCheckinTimeOverrideAndManifestDefault(t *testing.T) {
	clock := newFakeClock(at(9, 30))
	plugins := &fakePlugins{
		statuses: []model.PluginStatus{
			makeStatus("p1", true, []string{model.CapCheckin}, "09:00", 0),
			makeStatus("p2", true, []string{model.CapCheckin}, "11:00", 0),
		},
		accounts: map[string][]model.Account{
			"p1": {makeAccount("acc1", true, 0, 0)},
			"p2": {makeAccount("acc2", true, 0, 0)},
		},
		times: map[string]string{"p1": "10:00"},
	}
	st := openScheduleStore(t)
	m, _ := newTestManager(t, plugins, st, clock, &fakeNotifier{})

	m.runPass(context.Background(), false)
	if got := len(plugins.checkinCalls()); got != 0 {
		t.Fatalf("09:30 check-ins = %d, want 0 (p1 override is 10:00, p2 is 11:00)", got)
	}

	clock.Set(at(10, 0))
	m.runPass(context.Background(), false)
	calls := plugins.checkinCalls()
	if len(calls) != 1 || calls[0].plugin != "p1" {
		t.Fatalf("10:00 check-ins = %+v, want only p1", calls)
	}

	clock.Set(at(11, 0))
	m.runPass(context.Background(), false)
	calls = plugins.checkinCalls()
	if len(calls) != 2 || calls[1].plugin != "p2" {
		t.Fatalf("11:00 check-ins = %+v, want p1 then p2", calls)
	}
}

func TestCheckinTimeDefaultsToNine(t *testing.T) {
	clock := newFakeClock(at(8, 59))
	plugins := &fakePlugins{
		statuses: []model.PluginStatus{makeStatus("p1", true, []string{model.CapCheckin}, "", 0)},
		accounts: map[string][]model.Account{"p1": {makeAccount("acc1", true, 0, 0)}},
	}
	st := openScheduleStore(t)
	m, _ := newTestManager(t, plugins, st, clock, &fakeNotifier{})

	m.runPass(context.Background(), false)
	if got := len(plugins.checkinCalls()); got != 0 {
		t.Fatalf("08:59 check-ins = %d, want 0", got)
	}
	clock.Set(at(9, 0))
	m.runPass(context.Background(), false)
	if got := len(plugins.checkinCalls()); got != 1 {
		t.Fatalf("09:00 check-ins = %d, want 1", got)
	}
}

func TestIdempotentSkipPerAccount(t *testing.T) {
	clock := newFakeClock(at(10, 0))
	plugins := &fakePlugins{
		statuses: []model.PluginStatus{makeStatus("p1", true, []string{model.CapCheckin}, "09:00", 0)},
		accounts: map[string][]model.Account{"p1": {
			makeAccount("acc_ok", true, 0, 0),
			makeAccount("acc_already", true, 0, 0),
			makeAccount("acc_disabled", false, 0, 0),
			makeAccount("acc_missing", true, 0, 0),
		}},
	}
	st := openScheduleStore(t)
	recordSuccess(t, st, "p1", "acc_ok", "2026-10-20", model.CheckinOK)
	recordSuccess(t, st, "p1", "acc_already", "2026-10-20", model.CheckinAlready)
	m, _ := newTestManager(t, plugins, st, clock, &fakeNotifier{})

	m.runPass(context.Background(), false)
	calls := plugins.checkinCalls()
	if len(calls) != 1 || calls[0].account != "acc_missing" {
		t.Fatalf("check-ins = %+v, want only acc_missing", calls)
	}

	m.runPass(context.Background(), false)
	if got := len(plugins.checkinCalls()); got != 1 {
		t.Fatalf("second pass check-ins = %d, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// retries
// ---------------------------------------------------------------------------

func TestRetryThenFail(t *testing.T) {
	clock := newFakeClock(at(10, 0))
	attempts := 0
	plugins := &fakePlugins{
		statuses: []model.PluginStatus{makeStatus("p1", true, []string{model.CapCheckin}, "09:00", 0)},
		accounts: map[string][]model.Account{"p1": {makeAccount("acc1", true, 0, 0)}},
		checkinFn: func(_, _ string) (model.CheckinResult, error) {
			attempts++
			return model.CheckinResult{
				Result:  model.CheckinFail,
				Message: fmt.Sprintf("failure %d", attempts),
			}, nil
		},
	}
	st := openScheduleStore(t)
	notifier := &fakeNotifier{}
	m, sleeper := newTestManager(t, plugins, st, clock, notifier)

	m.runPass(context.Background(), false)

	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3 (initial + 2 retries)", attempts)
	}
	if got := len(plugins.checkinCalls()); got != 3 {
		t.Fatalf("checkin calls = %d, want 3", got)
	}
	delays := sleeper.recorded()
	if len(delays) != 2 || delays[0] != 5*time.Second || delays[1] != 20*time.Second {
		t.Fatalf("retry delays = %v, want [5s 20s]", delays)
	}
	kinds := notifier.kinds()
	if len(kinds) != 1 || kinds[0] != KindCheckinFailed {
		t.Fatalf("notifications = %v, want one checkin_failed", kinds)
	}
	records := mustRecords(t, st, "p1", "2026-10-20", "2026-10-20")
	if len(records) != 1 || records[0].Result != model.CheckinFail {
		t.Fatalf("records = %+v, want one fail row", records)
	}
	if records[0].Message != "failure 3" {
		t.Fatalf("record message = %q, want the last attempt", records[0].Message)
	}
}

func TestRetryThenOK(t *testing.T) {
	clock := newFakeClock(at(10, 0))
	attempts := 0
	after := 42.0
	plugins := &fakePlugins{
		statuses: []model.PluginStatus{makeStatus("p1", true, []string{model.CapCheckin}, "09:00", 0)},
		accounts: map[string][]model.Account{"p1": {makeAccount("acc1", true, 0, 0)}},
		checkinFn: func(_, _ string) (model.CheckinResult, error) {
			attempts++
			if attempts == 1 {
				return model.CheckinResult{Result: model.CheckinFail, Message: "transient"}, nil
			}
			return model.CheckinResult{Result: model.CheckinOK, CreditsAfter: &after}, nil
		},
	}
	st := openScheduleStore(t)
	notifier := &fakeNotifier{}
	m, sleeper := newTestManager(t, plugins, st, clock, notifier)

	m.runPass(context.Background(), false)

	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	delays := sleeper.recorded()
	if len(delays) != 1 || delays[0] != 5*time.Second {
		t.Fatalf("retry delays = %v, want [5s]", delays)
	}
	if got := len(notifier.snapshot()); got != 0 {
		t.Fatalf("notifications = %d, want none", got)
	}
	records := mustRecords(t, st, "p1", "2026-10-20", "2026-10-20")
	if len(records) != 1 || records[0].Result != model.CheckinOK {
		t.Fatalf("records = %+v, want one ok row", records)
	}
	if records[0].CreditsAfter == nil || *records[0].CreditsAfter != after {
		t.Fatalf("credits after = %v, want %v", records[0].CreditsAfter, after)
	}
}

func TestAlreadyIsSuccessWithoutRetry(t *testing.T) {
	clock := newFakeClock(at(10, 0))
	plugins := &fakePlugins{
		statuses: []model.PluginStatus{makeStatus("p1", true, []string{model.CapCheckin}, "09:00", 0)},
		accounts: map[string][]model.Account{"p1": {makeAccount("acc1", true, 0, 0)}},
		checkinFn: func(_, _ string) (model.CheckinResult, error) {
			return model.CheckinResult{Result: model.CheckinAlready}, nil
		},
	}
	st := openScheduleStore(t)
	notifier := &fakeNotifier{}
	m, sleeper := newTestManager(t, plugins, st, clock, notifier)

	m.runPass(context.Background(), false)

	if got := len(plugins.checkinCalls()); got != 1 {
		t.Fatalf("checkin calls = %d, want 1", got)
	}
	if got := len(sleeper.recorded()); got != 0 {
		t.Fatalf("sleeps = %v, want none", got)
	}
	records := mustRecords(t, st, "p1", "2026-10-20", "2026-10-20")
	if len(records) != 1 || records[0].Result != model.CheckinAlready {
		t.Fatalf("records = %+v, want one already row", records)
	}
	if got := len(notifier.snapshot()); got != 0 {
		t.Fatalf("notifications = %d, want none", got)
	}
}

// ---------------------------------------------------------------------------
// quota
// ---------------------------------------------------------------------------

func TestQuotaThresholdOncePerDay(t *testing.T) {
	clock := newFakeClock(at(10, 0))
	plugins := &fakePlugins{
		statuses: []model.PluginStatus{makeStatus("p1", true, []string{model.CapQuota}, "", 10)},
		accounts: map[string][]model.Account{"p1": {
			makeAccount("low_ratio", true, 5, 100),
			makeAccount("low_absolute", true, 50, 0),
			makeAccount("healthy", true, 200, 1000),
			makeAccount("boundary", true, 100, 0),
		}},
	}
	st := openScheduleStore(t)
	notifier := &fakeNotifier{}
	m, _ := newTestManager(t, plugins, st, clock, notifier)

	m.runPass(context.Background(), false)
	if got := plugins.accountsHit(); got != 1 {
		t.Fatalf("accounts calls = %d, want 1", got)
	}
	lowEvents := 0
	for _, e := range notifier.snapshot() {
		if e.kind == KindQuotaLow {
			lowEvents++
		}
	}
	if lowEvents != 2 {
		t.Fatalf("quota_low notifications = %d, want 2 (%v)", lowEvents, notifier.kinds())
	}
	snapshots, err := st.QueryQuotaSnapshots("p1", "", 0)
	if err != nil {
		t.Fatalf("QueryQuotaSnapshots: %v", err)
	}
	if len(snapshots) != 4 {
		t.Fatalf("snapshots = %d, want 4", len(snapshots))
	}

	// Same minute: the poll interval suppresses the second Accounts call and
	// the per-day guard suppresses repeat notifications.
	m.runPass(context.Background(), false)
	if got := plugins.accountsHit(); got != 1 {
		t.Fatalf("accounts calls within poll interval = %d, want 1", got)
	}
	if got := len(notifier.snapshot()); got != 2 {
		t.Fatalf("notifications within day = %d, want 2", got)
	}

	// Past the poll interval but same day: poll again, no new notification.
	clock.Advance(11 * time.Minute)
	m.runPass(context.Background(), false)
	if got := plugins.accountsHit(); got != 2 {
		t.Fatalf("accounts calls after interval = %d, want 2", got)
	}
	lowEvents = 0
	for _, e := range notifier.snapshot() {
		if e.kind == KindQuotaLow {
			lowEvents++
		}
	}
	if lowEvents != 2 {
		t.Fatalf("quota_low within day = %d, want 2", lowEvents)
	}

	// Next day: notify once more per low account.
	clock.Advance(24 * time.Hour)
	m.runPass(context.Background(), false)
	lowEvents = 0
	for _, e := range notifier.snapshot() {
		if e.kind == KindQuotaLow {
			lowEvents++
		}
	}
	if lowEvents != 4 {
		t.Fatalf("quota_low after next day = %d, want 4", lowEvents)
	}
}

func TestQuotaLowRules(t *testing.T) {
	cases := []struct {
		name string
		q    model.Quota
		want bool
	}{
		{"ratio below", model.Quota{Remaining: 9.99, Total: 100}, true},
		{"ratio at threshold", model.Quota{Remaining: 10, Total: 100}, false},
		{"ratio above", model.Quota{Remaining: 10.01, Total: 100}, false},
		{"absolute below", model.Quota{Remaining: 99.99, Total: 0}, true},
		{"absolute at threshold", model.Quota{Remaining: 100, Total: 0}, false},
		{"absolute above", model.Quota{Remaining: 150, Total: 0}, false},
		{"unlimited with zero counters", model.Quota{Unlimited: true, Remaining: 0, Total: 0}, false},
		{"unlimited with a tiny remainder", model.Quota{Unlimited: true, Remaining: 1, Total: 100}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := quotaLow(tc.q); got != tc.want {
				t.Fatalf("quotaLow(%+v) = %v, want %v", tc.q, got, tc.want)
			}
		})
	}
}

func TestPluginOfflineNotifiedOncePerDay(t *testing.T) {
	clock := newFakeClock(at(10, 0))
	plugins := &fakePlugins{
		statuses:   []model.PluginStatus{makeStatus("p1", true, []string{model.CapCheckin}, "09:00", 0)},
		accountErr: map[string]error{"p1": errors.New("plugin offline")},
	}
	st := openScheduleStore(t)
	notifier := &fakeNotifier{}
	m, _ := newTestManager(t, plugins, st, clock, notifier)

	m.runPass(context.Background(), false)
	m.runPass(context.Background(), false)
	kinds := notifier.kinds()
	if len(kinds) != 1 || kinds[0] != KindPluginOffline {
		t.Fatalf("notifications = %v, want one plugin_offline", kinds)
	}
}

// ---------------------------------------------------------------------------
// manual check-in all
// ---------------------------------------------------------------------------

func TestCheckinAllOrderAndRecording(t *testing.T) {
	clock := newFakeClock(at(10, 0))
	after := 12.0
	plugins := &fakePlugins{
		statuses: []model.PluginStatus{
			makeStatus("zeta", true, []string{model.CapCheckin}, "09:00", 0),
			makeStatus("alpha", true, []string{model.CapCheckin}, "09:00", 0),
			makeStatus("beta", true, []string{model.CapQuota}, "", 5),
		},
		accounts: map[string][]model.Account{
			"zeta":  {makeAccount("z1", true, 0, 0), makeAccount("z2", false, 0, 0)},
			"alpha": {makeAccount("a1", true, 0, 0)},
			"beta":  {makeAccount("b1", true, 0, 0)},
		},
		checkinFn: func(pluginID, accountID string) (model.CheckinResult, error) {
			if pluginID == "zeta" {
				return model.CheckinResult{Result: model.CheckinFail, Message: "boom"}, nil
			}
			return model.CheckinResult{Result: model.CheckinOK, CreditsAfter: &after}, nil
		},
	}
	host := &fakeA{path: `C:\A\A.exe`}
	st := openScheduleStore(t)
	notifier := &fakeNotifier{}
	m, sleeper := newTestManager(t, plugins, st, clock, notifier)
	m.A = host

	results := m.CheckinAll(context.Background())

	// REVERSED on purpose: the manual "sign all" used to append A's three
	// `A.exe --task-run` check-ins after the plugin pass. The user signs A in
	// himself and asked for A to be left out of this action, so only plugins
	// declaring the checkin capability are signed now. If A ever runs here
	// again, this assertion is the regression guard.
	want := []struct {
		source  string
		account string
		result  string
	}{
		{"plugin:zeta", "z1", model.CheckinFail},
		{"plugin:alpha", "a1", model.CheckinOK},
	}
	if len(results) != len(want) {
		t.Fatalf("results = %+v, want %d items (plugins only, no A)", results, len(want))
	}
	for _, got := range results {
		if got.Source == SourceA {
			t.Fatalf("manual sign-all produced an A result %+v; A must be excluded", got)
		}
	}
	for i, w := range want {
		got := results[i]
		if got.Source != w.source || got.Account != w.account || got.Result != w.result {
			t.Fatalf("result[%d] = %+v, want {%s %s %s}", i, got, w.source, w.account, w.result)
		}
	}
	if results[0].Message != "boom" {
		t.Fatalf("zeta message = %q, want boom", results[0].Message)
	}
	if results[1].CreditsAfter == nil || *results[1].CreditsAfter != after {
		t.Fatalf("alpha credits after = %v, want %v", results[1].CreditsAfter, after)
	}

	// No retry for the manual action, disabled accounts skipped.
	calls := plugins.checkinCalls()
	if len(calls) != 2 || calls[0].account != "z1" || calls[1].account != "a1" {
		t.Fatalf("manual check-ins = %+v, want z1 then a1 only", calls)
	}
	if got := len(sleeper.recorded()); got != 0 {
		t.Fatalf("manual sleeps = %v, want none", got)
	}

	// Both plugin outcomes are recorded; A tasks stay in A's own calendar.
	records := mustRecords(t, st, "", "", "")
	if len(records) != 2 {
		t.Fatalf("records = %+v, want 2 plugin rows", records)
	}
	byAccount := map[string]model.CheckinRecord{}
	for _, rec := range records {
		byAccount[rec.AccountID] = rec
	}
	if rec, ok := byAccount["z1"]; !ok || rec.Result != model.CheckinFail || rec.Date != "2026-10-20" {
		t.Fatalf("z1 record = %+v (ok=%v)", rec, ok)
	}
	if rec, ok := byAccount["a1"]; !ok || rec.Result != model.CheckinOK {
		t.Fatalf("a1 record = %+v (ok=%v)", rec, ok)
	}

	// A must not be invoked by the manual action any more (see the want table
	// above). The fakeA is installed on purpose so a regression shows up here
	// rather than silently in production.
	tasks := host.taskKeys()
	if len(tasks) != 0 {
		t.Fatalf("A tasks = %v, want none: the manual sign-all must not run A", tasks)
	}
	if got := len(notifier.snapshot()); got != 0 {
		t.Fatalf("manual notifications = %v, want none", notifier.kinds())
	}
}

// With every check-in-capable plugin disabled and A not configured, the manual
// action has nothing to do. A is not consulted at all any more, so this also
// guards that an unconfigured/disabled A never produces a result row.
func TestCheckinAllWithoutA(t *testing.T) {
	clock := newFakeClock(at(10, 0))
	plugins := &fakePlugins{
		statuses: []model.PluginStatus{makeStatus("p1", false, []string{model.CapCheckin}, "09:00", 0)},
	}
	st := openScheduleStore(t)
	m, _ := newTestManager(t, plugins, st, clock, &fakeNotifier{})
	m.A = &fakeA{path: ""}

	if results := m.CheckinAll(context.Background()); len(results) != 0 {
		t.Fatalf("results = %+v, want none (plugin disabled, A not configured)", results)
	}
}

// ---------------------------------------------------------------------------
// catch-up and loop lifecycle
// ---------------------------------------------------------------------------

func TestCatchUpRunsBeforeConfiguredTime(t *testing.T) {
	clock := newFakeClock(at(8, 0))
	plugins := &fakePlugins{
		statuses: []model.PluginStatus{makeStatus("p1", true, []string{model.CapCheckin}, "09:00", 0)},
		accounts: map[string][]model.Account{"p1": {
			makeAccount("done", true, 0, 0),
			makeAccount("missing", true, 0, 0),
		}},
	}
	st := openScheduleStore(t)
	recordSuccess(t, st, "p1", "done", "2026-10-20", model.CheckinOK)
	m, _ := newTestManager(t, plugins, st, clock, &fakeNotifier{})

	m.runPass(context.Background(), true)
	calls := plugins.checkinCalls()
	if len(calls) != 1 || calls[0].account != "missing" {
		t.Fatalf("catch-up check-ins = %+v, want only missing", calls)
	}

	// A normal pass at the same early hour must wait for 09:00.
	m.runPass(context.Background(), false)
	if got := len(plugins.checkinCalls()); got != 1 {
		t.Fatalf("normal pass check-ins = %d, want 1", got)
	}
}

func TestStartStopIdempotent(t *testing.T) {
	clock := newFakeClock(at(10, 0))
	called := make(chan struct{}, 1)
	plugins := &fakePlugins{
		statuses: []model.PluginStatus{makeStatus("p1", true, []string{model.CapCheckin}, "09:00", 0)},
		accounts: map[string][]model.Account{"p1": {makeAccount("acc1", true, 0, 0)}},
		checkinFn: func(_, _ string) (model.CheckinResult, error) {
			select {
			case called <- struct{}{}:
			default:
			}
			return model.CheckinResult{Result: model.CheckinOK}, nil
		},
	}
	st := openScheduleStore(t)
	m, _ := newTestManager(t, plugins, st, clock, &fakeNotifier{})

	m.Start(context.Background())
	m.Start(context.Background()) // idempotent
	select {
	case <-called:
	case <-time.After(5 * time.Second):
		t.Fatal("catch-up pass did not run")
	}
	m.Stop()
	m.Stop() // idempotent
	if got := len(plugins.checkinCalls()); got != 1 {
		t.Fatalf("check-ins after Start/Stop = %d, want 1", got)
	}
}

func TestRetryDelaysFromSettings(t *testing.T) {
	clock := newFakeClock(at(10, 0))
	attempts := 0
	plugins := &fakePlugins{
		statuses: []model.PluginStatus{makeStatus("p1", true, []string{model.CapCheckin}, "09:00", 0)},
		accounts: map[string][]model.Account{"p1": {makeAccount("acc1", true, 0, 0)}},
		checkinFn: func(_, _ string) (model.CheckinResult, error) {
			attempts++
			return model.CheckinResult{Result: model.CheckinFail}, nil
		},
	}
	st := openScheduleStore(t)
	m, sleeper := newTestManager(t, plugins, st, clock, &fakeNotifier{})
	m.RetryDelays = []time.Duration{time.Second}

	m.runPass(context.Background(), false)

	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2 (one configured retry)", attempts)
	}
	if got := sleeper.recorded(); len(got) != 1 || got[0] != time.Second {
		t.Fatalf("retry delays = %v, want [1s]", got)
	}
}

func TestEmptyRetryDelaysDisableRetries(t *testing.T) {
	clock := newFakeClock(at(10, 0))
	attempts := 0
	plugins := &fakePlugins{
		statuses: []model.PluginStatus{makeStatus("p1", true, []string{model.CapCheckin}, "09:00", 0)},
		accounts: map[string][]model.Account{"p1": {makeAccount("acc1", true, 0, 0)}},
		checkinFn: func(_, _ string) (model.CheckinResult, error) {
			attempts++
			return model.CheckinResult{Result: model.CheckinFail}, nil
		},
	}
	st := openScheduleStore(t)
	m, sleeper := newTestManager(t, plugins, st, clock, &fakeNotifier{})
	m.RetryDelays = []time.Duration{}

	m.runPass(context.Background(), false)

	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (retries disabled)", attempts)
	}
	if got := sleeper.recorded(); len(got) != 0 {
		t.Fatalf("retry delays = %v, want none", got)
	}
}

func intPtr(v int) *int { return &v }

func TestQuotaPollInterval(t *testing.T) {
	if got, ok := quotaPollInterval(model.QuotaConfig{}); !ok || got != 30*time.Minute {
		t.Fatalf("omitted poll_minutes = %v, %v; want 30m, true", got, ok)
	}
	if got, ok := quotaPollInterval(model.QuotaConfig{PollMinutes: intPtr(0)}); ok || got != 0 {
		t.Fatalf("poll_minutes 0 = %v, %v; want disabled", got, ok)
	}
	if got, ok := quotaPollInterval(model.QuotaConfig{PollMinutes: intPtr(5)}); !ok || got != 5*time.Minute {
		t.Fatalf("poll_minutes 5 = %v, %v; want 5m, true", got, ok)
	}
}

func TestFailedAccountNotRetriedEveryPass(t *testing.T) {
	clock := newFakeClock(at(10, 0))
	attempts := 0
	plugins := &fakePlugins{
		statuses: []model.PluginStatus{makeStatus("p1", true, []string{model.CapCheckin}, "09:00", 0)},
		accounts: map[string][]model.Account{"p1": {makeAccount("acc1", true, 0, 0)}},
		checkinFn: func(_, _ string) (model.CheckinResult, error) {
			attempts++
			return model.CheckinResult{Result: model.CheckinFail, Message: "offline"}, nil
		},
	}
	st := openScheduleStore(t)
	m, sleeper := newTestManager(t, plugins, st, clock, &fakeNotifier{})

	m.runPass(context.Background(), false)
	if attempts != 3 {
		t.Fatalf("first pass attempts = %d, want 3", attempts)
	}
	sleeper.reset()
	m.runPass(context.Background(), false)
	if attempts != 3 {
		t.Fatalf("second pass retried an exhausted account: attempts = %d", attempts)
	}
	if got := sleeper.recorded(); len(got) != 0 {
		t.Fatalf("second pass slept for retries: %v", got)
	}
	// A manual sign-all still tries once, regardless of the daily guard.
	m.CheckinAll(context.Background())
	if attempts != 4 {
		t.Fatalf("manual check-in attempts = %d, want one more", attempts)
	}
}
