package schedule

import (
	"context"
	"errors"
	"testing"
	"time"

	"aiworkgate/internal/model"
	"aiworkgate/internal/store"
)

// ---------------------------------------------------------------------------
// cooldown table
// ---------------------------------------------------------------------------

func TestCooldownUntilForKind(t *testing.T) {
	now := at(12, 0) // 2026-10-20 12:00 local
	nowUnix := now.Unix()
	nextDevice := time.Date(2026, 10, 21, 0, 5, 0, 0, time.Local).Unix()

	cases := []struct {
		name     string
		kind     string
		resetAt  int64
		failures int
		want     int64
	}{
		{"session_dead is permanent", model.StatusKindSessionDead, 0, 0, PermanentCooldownUntil},
		{"banned is permanent", model.StatusKindBanned, 0, 0, PermanentCooldownUntil},
		{"plan_limit is 12h", model.StatusKindPlanLimit, 0, 0, now.Add(12 * time.Hour).Unix()},
		{"device_signed waits for 00:05", model.StatusKindDeviceSigned, 0, 0, nextDevice},
		{"rate_limited uses reset_at", model.StatusKindRateLimited, nowUnix + 90, 0, nowUnix + 90},
		{"quota uses reset_at", model.StatusKindQuota, nowUnix + 600, 0, nowUnix + 600},
		{"rate_limited backs off 30m", model.StatusKindRateLimited, 0, 0, now.Add(30 * time.Minute).Unix()},
		{"rate_limited doubles", model.StatusKindRateLimited, 0, 1, now.Add(60 * time.Minute).Unix()},
		{"rate_limited doubles again", model.StatusKindRateLimited, 0, 2, now.Add(120 * time.Minute).Unix()},
		{"rate_limited caps at 6h", model.StatusKindRateLimited, 0, 4, now.Add(6 * time.Hour).Unix()},
		{"quota caps at 6h", model.StatusKindQuota, 0, 20, now.Add(6 * time.Hour).Unix()},
		{"stale reset_at falls back to backoff", model.StatusKindRateLimited, nowUnix - 60, 0, now.Add(30 * time.Minute).Unix()},
		{"unknown kind does not cool", "", 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cooldownUntilFor(tc.kind, now, tc.resetAt, tc.failures); got != tc.want {
				t.Fatalf("cooldownUntilFor(%q) = %d, want %d", tc.kind, got, tc.want)
			}
		})
	}
}

func TestDeviceResetUsesNextLocalDay(t *testing.T) {
	// 23:59 still resolves to the next 00:05, not to the current day.
	now := time.Date(2026, 10, 20, 23, 59, 0, 0, time.Local)
	want := time.Date(2026, 10, 21, 0, 5, 0, 0, time.Local).Unix()
	if got := nextDeviceReset(now); got != want {
		t.Fatalf("nextDeviceReset = %d, want %d", got, want)
	}
}

// ---------------------------------------------------------------------------
// fallback classification
// ---------------------------------------------------------------------------

func TestFailureKindTable(t *testing.T) {
	cases := []struct {
		name   string
		result model.CheckinResult
		want   string
	}{
		{"plugin kind wins", model.CheckinResult{Result: model.CheckinFail, Kind: "session_dead", Message: "whatever"}, model.StatusKindSessionDead},
		{"plugin kind is case folded", model.CheckinResult{Result: model.CheckinFail, Kind: " Quota "}, model.StatusKindQuota},
		{"unknown plugin kind falls back", model.CheckinResult{Result: model.CheckinFail, Kind: "weird", Message: "plugins: http 401: unauthorized"}, model.StatusKindSessionDead},
		{"normal is not a failure kind", model.CheckinResult{Result: model.CheckinFail, Kind: "normal", Message: "boom"}, ""},
		{"http 401", model.CheckinResult{Result: model.CheckinFail, Message: "plugins: http 401: unauthorized"}, model.StatusKindSessionDead},
		{"http 403", model.CheckinResult{Result: model.CheckinFail, Message: "HTTP 403: forbidden"}, model.StatusKindSessionDead},
		{"http 429", model.CheckinResult{Result: model.CheckinFail, Message: "HTTP 429: too many requests"}, model.StatusKindRateLimited},
		{"balance wording", model.CheckinResult{Result: model.CheckinFail, Message: "签到失败：余额不足"}, model.StatusKindQuota},
		{"quota wording", model.CheckinResult{Result: model.CheckinFail, Message: "额度不足，请充值"}, model.StatusKindQuota},
		{"english quota wording", model.CheckinResult{Result: model.CheckinFail, Message: "insufficient quota for this request"}, model.StatusKindQuota},
		{"random number is not a status", model.CheckinResult{Result: model.CheckinFail, Message: "failed after 4010 ms"}, ""},
		{"plain failure stays unattributed", model.CheckinResult{Result: model.CheckinFail, Message: "network timeout"}, ""},
		{"empty message stays unattributed", model.CheckinResult{Result: model.CheckinFail}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := failureKind(tc.result); got != tc.want {
				t.Fatalf("failureKind(%+v) = %q, want %q", tc.result, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// scheduler behaviour
// ---------------------------------------------------------------------------

func TestCoolingAccountIsSkippedWithoutCallingPlugin(t *testing.T) {
	clock := newFakeClock(at(10, 0))
	plugins := &fakePlugins{
		statuses: []model.PluginStatus{makeStatus("p1", true, []string{model.CapCheckin}, "09:00", 0)},
		accounts: map[string][]model.Account{"p1": {makeAccount("acc1", true, 0, 0)}},
	}
	st := openScheduleStore(t)
	m, _ := newTestManager(t, plugins, st, clock, &fakeNotifier{})
	if err := st.SetAccountHealth(store.AccountHealth{
		PluginID: "p1", AccountID: "acc1",
		StatusKind: model.StatusKindRateLimited, CooldownUntil: clock.Now().Add(time.Hour).Unix(),
		CooldownReason: "上游限流", FailureCount: 1,
	}); err != nil {
		t.Fatalf("SetAccountHealth: %v", err)
	}

	m.runPass(context.Background(), false)
	if calls := plugins.checkinCalls(); len(calls) != 0 {
		t.Fatalf("plugin called for a cooling account: %+v", calls)
	}
	records := mustRecords(t, st, "p1", "2026-10-20", "2026-10-20")
	if len(records) != 1 || records[0].Result != model.CheckinSkipped {
		t.Fatalf("records = %+v, want one skipped row", records)
	}
	if records[0].Message != "冷却中：上游限流" {
		t.Fatalf("skip reason = %q", records[0].Message)
	}
	// A second scheduled pass does not even re-read the plugin and does not
	// rewrite the skipped row.
	m.runPass(context.Background(), false)
	if calls := plugins.checkinCalls(); len(calls) != 0 {
		t.Fatalf("second pass called the plugin: %+v", calls)
	}

	// The manual sign-all reports the skip too.
	out := m.CheckinAll(context.Background())
	if len(out) != 1 || out[0].Result != model.CheckinSkipped || out[0].Message != "冷却中：上游限流" {
		t.Fatalf("manual results = %+v", out)
	}
	if calls := plugins.checkinCalls(); len(calls) != 0 {
		t.Fatalf("manual sign-all called a cooling account: %+v", calls)
	}

	// Once the cooldown expires the scheduled pass tries again and a success
	// clears the health tuple.
	clock.Advance(2 * time.Hour)
	m.runPass(context.Background(), false)
	if calls := plugins.checkinCalls(); len(calls) != 1 {
		t.Fatalf("calls after cooldown = %d, want 1", len(calls))
	}
	health, ok, _ := st.GetAccountHealth("p1", "acc1")
	if ok && (health.StatusKind != "" || health.CooldownUntil != 0 || health.FailureCount != 0) {
		t.Fatalf("success did not clear health: %+v", health)
	}
}

func TestDeviceDedupSkipsKnownDevice(t *testing.T) {
	clock := newFakeClock(at(10, 0))
	plugins := &fakePlugins{
		statuses: []model.PluginStatus{makeStatus("p1", true, []string{model.CapCheckin}, "09:00", 0)},
		accounts: map[string][]model.Account{"p1": {
			makeAccount("acc_a", true, 0, 0),
			makeAccount("acc_b", true, 0, 0),
		}},
	}
	st := openScheduleStore(t)
	m, _ := newTestManager(t, plugins, st, clock, &fakeNotifier{})

	// Today acc_a signed on device D.
	if _, err := st.UpsertCheckinRecord(model.CheckinRecord{
		PluginID: "p1", AccountID: "acc_a", Date: "2026-10-20", Result: model.CheckinOK,
		At:         clock.Now().Add(-time.Hour).Unix(),
		DedupScope: model.DedupScopeDevice, DeviceKey: "dev-d",
	}); err != nil {
		t.Fatalf("seed acc_a: %v", err)
	}
	// acc_b already reported the same device on an earlier day, so the host
	// knows it shares the device before calling the plugin.
	if _, err := st.UpsertCheckinRecord(model.CheckinRecord{
		PluginID: "p1", AccountID: "acc_b", Date: "2026-10-19", Result: model.CheckinOK,
		At:         clock.Now().Add(-25 * time.Hour).Unix(),
		DedupScope: model.DedupScopeDevice, DeviceKey: "dev-d",
	}); err != nil {
		t.Fatalf("seed acc_b: %v", err)
	}

	m.runPass(context.Background(), false)
	calls := plugins.checkinCalls()
	if len(calls) != 0 {
		t.Fatalf("plugin called although the device already signed: %+v", calls)
	}
	records := mustRecords(t, st, "p1", "2026-10-20", "2026-10-20")
	byAccount := map[string]model.CheckinRecord{}
	for _, rec := range records {
		byAccount[rec.AccountID] = rec
	}
	skip, ok := byAccount["acc_b"]
	if !ok || skip.Result != model.CheckinSkipped || skip.Message != "设备当日已签" {
		t.Fatalf("acc_b record = %+v", skip)
	}
	if skip.DedupScope != model.DedupScopeDevice || skip.DeviceKey != "dev-d" {
		t.Fatalf("skip lost the device identity: %+v", skip)
	}
	// The device win by acc_a stays a success.
	if byAccount["acc_a"].Result != model.CheckinOK {
		t.Fatalf("acc_a record = %+v", byAccount["acc_a"])
	}
}

func TestPluginKindIsAdoptedAndStopsRetries(t *testing.T) {
	clock := newFakeClock(at(10, 0))
	attempts := 0
	plugins := &fakePlugins{
		statuses: []model.PluginStatus{makeStatus("p1", true, []string{model.CapCheckin}, "09:00", 0)},
		accounts: map[string][]model.Account{"p1": {makeAccount("acc1", true, 0, 0)}},
		checkinFn: func(_, _ string) (model.CheckinResult, error) {
			attempts++
			return model.CheckinResult{
				Result:  model.CheckinFail,
				Kind:    model.StatusKindSessionDead,
				Message: "登录态已失效",
			}, nil
		},
	}
	st := openScheduleStore(t)
	m, sleeper := newTestManager(t, plugins, st, clock, &fakeNotifier{})

	m.runPass(context.Background(), false)
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (classified failures are not retried)", attempts)
	}
	if got := sleeper.recorded(); len(got) != 0 {
		t.Fatalf("retried a classified failure: %v", got)
	}
	health, ok, _ := st.GetAccountHealth("p1", "acc1")
	if !ok || health.StatusKind != model.StatusKindSessionDead || health.CooldownUntil != PermanentCooldownUntil {
		t.Fatalf("health = %+v ok=%v", health, ok)
	}
	if health.FailureCount != 1 {
		t.Fatalf("failure_count = %d, want 1", health.FailureCount)
	}
	if health.CooldownReason == "" {
		t.Fatal("cooldown_reason is empty")
	}
	records := mustRecords(t, st, "p1", "2026-10-20", "2026-10-20")
	if len(records) != 1 || records[0].Result != model.CheckinFail {
		t.Fatalf("records = %+v", records)
	}

	// Subsequent passes stay parked.
	m.runPass(context.Background(), false)
	if attempts != 1 {
		t.Fatalf("parked account was called again: attempts = %d", attempts)
	}

	// A login clears the health tuple and releases the account.
	m.ClearAccountHealth("p1", "acc1")
	m.runPass(context.Background(), false)
	if attempts != 2 {
		t.Fatalf("attempts after clearing health = %d, want 2", attempts)
	}
}

func TestManualResultCarriesSkipAndHealthIsClearedBySuccess(t *testing.T) {
	clock := newFakeClock(at(10, 0))
	plugins := &fakePlugins{
		statuses: []model.PluginStatus{makeStatus("p1", true, []string{model.CapCheckin}, "09:00", 0)},
		accounts: map[string][]model.Account{"p1": {makeAccount("acc1", true, 0, 0)}},
	}
	st := openScheduleStore(t)
	m, _ := newTestManager(t, plugins, st, clock, &fakeNotifier{})
	if err := st.SetAccountHealth(store.AccountHealth{
		PluginID: "p1", AccountID: "acc1",
		StatusKind: model.StatusKindSessionDead, CooldownUntil: PermanentCooldownUntil,
		CooldownReason: "登录态已失效，请重新登录", FailureCount: 3,
	}); err != nil {
		t.Fatalf("SetAccountHealth: %v", err)
	}
	out := m.CheckinAll(context.Background())
	if len(out) != 1 || out[0].Result != model.CheckinSkipped {
		t.Fatalf("manual results = %+v", out)
	}
	if out[0].Message != "冷却中：登录态已失效，请重新登录" {
		t.Fatalf("manual skip message = %q", out[0].Message)
	}

	// Clear (as a login would) and run manually: success wipes the tuple.
	m.ClearAccountHealth("p1", "acc1")
	out = m.CheckinAll(context.Background())
	if len(out) != 1 || out[0].Result != model.CheckinOK {
		t.Fatalf("manual results after clear = %+v", out)
	}
	health, ok, _ := st.GetAccountHealth("p1", "acc1")
	if ok && (health.StatusKind != "" || health.FailureCount != 0 || health.CooldownUntil != 0) {
		t.Fatalf("health after success = %+v", health)
	}
}

func TestUnclassifiedFailureOnlyCounts(t *testing.T) {
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
	m, _ := newTestManager(t, plugins, st, clock, &fakeNotifier{})

	m.runPass(context.Background(), false)
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
	health, ok, _ := st.GetAccountHealth("p1", "acc1")
	if !ok || health.FailureCount != 3 || health.StatusKind != "" || health.CooldownUntil != 0 {
		t.Fatalf("health = %+v ok=%v, want 3 failures and no cooldown", health, ok)
	}
	records := mustRecords(t, st, "p1", "2026-10-20", "2026-10-20")
	if len(records) != 1 || records[0].Result != model.CheckinFail {
		t.Fatalf("records = %+v", records)
	}
}

func TestResetAtAndExponentialCooldown(t *testing.T) {
	clock := newFakeClock(at(10, 0))
	resetAt := clock.Now().Add(90 * time.Minute).Unix()
	plugins := &fakePlugins{
		statuses: []model.PluginStatus{makeStatus("p1", true, []string{model.CapCheckin}, "09:00", 0)},
		accounts: map[string][]model.Account{"p1": {makeAccount("acc1", true, 0, 0)}},
		checkinFn: func(_, _ string) (model.CheckinResult, error) {
			return model.CheckinResult{
				Result:  model.CheckinFail,
				Kind:    model.StatusKindRateLimited,
				ResetAt: resetAt,
				Message: "HTTP 429: slow down",
			}, nil
		},
	}
	st := openScheduleStore(t)
	m, _ := newTestManager(t, plugins, st, clock, &fakeNotifier{})

	m.runPass(context.Background(), false)
	health, _, _ := st.GetAccountHealth("p1", "acc1")
	if health.StatusKind != model.StatusKindRateLimited || health.CooldownUntil != resetAt {
		t.Fatalf("reset_at not honoured: %+v", health)
	}

	// A second classification without reset_at backs off exponentially from the
	// stored failure count (30m * 2^1 here).
	if err := st.SetAccountHealth(store.AccountHealth{PluginID: "p1", AccountID: "acc1", FailureCount: 1}); err != nil {
		t.Fatalf("reset health: %v", err)
	}
	m.unpark("p1", "acc1")
	plugins.checkinFn = func(_, _ string) (model.CheckinResult, error) {
		return model.CheckinResult{
			Result:  model.CheckinFail,
			Kind:    model.StatusKindQuota,
			Message: "余额不足",
		}, nil
	}
	m.runPass(context.Background(), false)
	health, _, _ = st.GetAccountHealth("p1", "acc1")
	if health.FailureCount != 2 {
		t.Fatalf("failure_count = %d, want 2", health.FailureCount)
	}
	want := clock.Now().Add(60 * time.Minute).Unix()
	if health.CooldownUntil != want {
		t.Fatalf("cooldown_until = %d, want %d", health.CooldownUntil, want)
	}
}

func TestPluginTransportErrorIsNotAttributed(t *testing.T) {
	clock := newFakeClock(at(10, 0))
	attempts := 0
	plugins := &fakePlugins{
		statuses: []model.PluginStatus{makeStatus("p1", true, []string{model.CapCheckin}, "09:00", 0)},
		accounts: map[string][]model.Account{"p1": {makeAccount("acc1", true, 0, 0)}},
		checkinFn: func(_, _ string) (model.CheckinResult, error) {
			attempts++
			// A "401" from the plugin rpc is a host/plugin token problem, not a
			// dead upstream session.
			return model.CheckinResult{}, errors.New("plugins: http 401: bad token")
		},
	}
	st := openScheduleStore(t)
	m, _ := newTestManager(t, plugins, st, clock, &fakeNotifier{})

	m.runPass(context.Background(), false)
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
	health, ok, _ := st.GetAccountHealth("p1", "acc1")
	if !ok || health.StatusKind != "" || health.CooldownUntil != 0 {
		t.Fatalf("transport error was attributed: %+v", health)
	}
	if health.FailureCount != 3 {
		t.Fatalf("failure_count = %d, want 3", health.FailureCount)
	}
}
