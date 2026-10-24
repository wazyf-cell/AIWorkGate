package store

import (
	"errors"
	"testing"

	"aiworkgate/internal/model"
)

func TestAccountHealthRoundTripDoesNotTouchOverlay(t *testing.T) {
	s := openTestStore(t)

	// Health first: the row is created with the primary key and the health
	// columns only.
	if err := s.SetAccountHealth(AccountHealth{
		PluginID: "mock", AccountID: "acc1",
		StatusKind: model.StatusKindRateLimited, CooldownUntil: 500,
		CooldownReason: "上游限流", FailureCount: 2,
	}); err != nil {
		t.Fatalf("SetAccountHealth: %v", err)
	}
	health, ok, err := s.GetAccountHealth("mock", "acc1")
	if err != nil || !ok {
		t.Fatalf("GetAccountHealth ok=%v err=%v", ok, err)
	}
	if health.StatusKind != model.StatusKindRateLimited || health.CooldownUntil != 500 ||
		health.CooldownReason != "上游限流" || health.FailureCount != 2 {
		t.Fatalf("health = %+v", health)
	}
	meta, ok, err := s.GetAccountMeta("mock", "acc1")
	if err != nil || !ok {
		t.Fatalf("GetAccountMeta ok=%v err=%v", ok, err)
	}
	if meta.Alias != "" || meta.GroupID != "" || meta.Priority != nil || meta.Weight != nil ||
		meta.EnabledOverride != nil || meta.Note != "" || meta.UpdatedAt != 0 {
		t.Fatalf("health-only insert leaked overlay values: %+v", meta)
	}

	// Overlay write (full upsert) keeps the health tuple and does not erase it.
	priority := 7
	if err := s.UpsertAccountMeta(model.AccountMeta{
		PluginID: "mock", AccountID: "acc1", Alias: "primary", GroupID: "g1",
		Priority: &priority, Note: "note", UpdatedAt: 111,
	}); err != nil {
		t.Fatalf("UpsertAccountMeta: %v", err)
	}
	health, _, _ = s.GetAccountHealth("mock", "acc1")
	if health.StatusKind != model.StatusKindRateLimited || health.CooldownUntil != 500 || health.FailureCount != 2 {
		t.Fatalf("overlay upsert clobbered health: %+v", health)
	}

	// Health write keeps every overlay column.
	if err := s.SetAccountHealth(AccountHealth{
		PluginID: "mock", AccountID: "acc1",
		StatusKind: model.StatusKindSessionDead, CooldownUntil: 9999999999,
		CooldownReason: "登录态已失效", FailureCount: 3,
	}); err != nil {
		t.Fatalf("second SetAccountHealth: %v", err)
	}
	meta, _, _ = s.GetAccountMeta("mock", "acc1")
	if meta.Alias != "primary" || meta.GroupID != "g1" || meta.Priority == nil || *meta.Priority != 7 ||
		meta.Note != "note" || meta.UpdatedAt != 111 {
		t.Fatalf("health write clobbered overlay: %+v", meta)
	}
	// The overlay readers surface the same health tuple.
	if meta.StatusKind != model.StatusKindSessionDead || meta.CooldownUntil != 9999999999 || meta.FailureCount != 3 {
		t.Fatalf("AccountMeta health = %+v", meta)
	}

	// Narrow overlay writers keep health too.
	if err := s.SetAccountAlias("mock", "acc1", "renamed"); err != nil {
		t.Fatalf("SetAccountAlias: %v", err)
	}
	if err := s.SetAccountGroup("mock", "acc1", ""); err != nil {
		t.Fatalf("SetAccountGroup: %v", err)
	}
	health, _, _ = s.GetAccountHealth("mock", "acc1")
	if health.StatusKind != model.StatusKindSessionDead || health.FailureCount != 3 {
		t.Fatalf("alias/group write clobbered health: %+v", health)
	}

	// Clearing health leaves the overlay alone.
	if err := s.ClearAccountHealth("mock", "acc1"); err != nil {
		t.Fatalf("ClearAccountHealth: %v", err)
	}
	health, ok, _ = s.GetAccountHealth("mock", "acc1")
	if !ok || health.StatusKind != "" || health.CooldownUntil != 0 || health.CooldownReason != "" || health.FailureCount != 0 {
		t.Fatalf("after clear health = %+v ok=%v", health, ok)
	}
	meta, _, _ = s.GetAccountMeta("mock", "acc1")
	if meta.Alias != "renamed" || meta.Priority == nil || *meta.Priority != 7 {
		t.Fatalf("clear clobbered overlay: %+v", meta)
	}

	// Clearing an account without a row is a no-op, and never inserts one.
	if err := s.ClearAccountHealth("mock", "missing"); err != nil {
		t.Fatalf("ClearAccountHealth missing: %v", err)
	}
	if _, ok, _ := s.GetAccountMeta("mock", "missing"); ok {
		t.Fatal("clear created an account_meta row")
	}
}

func TestAccountFailureBumpAndList(t *testing.T) {
	s := openTestStore(t)

	for want := 1; want <= 3; want++ {
		got, err := s.BumpAccountFailure("mock", "acc1")
		if err != nil {
			t.Fatalf("BumpAccountFailure: %v", err)
		}
		if got != want {
			t.Fatalf("bump %d = %d", want, got)
		}
	}
	// Bumping never sets a kind or cooldown.
	health, ok, _ := s.GetAccountHealth("mock", "acc1")
	if !ok || health.FailureCount != 3 || health.StatusKind != "" || health.CooldownUntil != 0 {
		t.Fatalf("health after bumps = %+v", health)
	}
	// A healthy account is not listed.
	if err := s.SetAccountAlias("mock", "healthy", "alias"); err != nil {
		t.Fatalf("SetAccountAlias: %v", err)
	}
	if err := s.SetAccountHealth(AccountHealth{
		PluginID: "mock", AccountID: "acc2",
		StatusKind: model.StatusKindQuota, CooldownUntil: 42, FailureCount: 1,
	}); err != nil {
		t.Fatalf("SetAccountHealth acc2: %v", err)
	}
	list, err := s.ListAccountHealth()
	if err != nil {
		t.Fatalf("ListAccountHealth: %v", err)
	}
	if len(list) != 2 || list[0].AccountID != "acc1" || list[1].AccountID != "acc2" {
		t.Fatalf("health list = %+v", list)
	}
	scoped, err := s.ListAccountHealthForPlugin("mock")
	if err != nil || len(scoped) != 2 {
		t.Fatalf("plugin health = %+v err=%v", scoped, err)
	}
	if _, err := s.ListAccountHealthForPlugin(""); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty plugin = %v, want ErrInvalidArgument", err)
	}

	if _, err := s.BumpAccountFailure("", "acc1"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty plugin bump = %v, want ErrInvalidArgument", err)
	}
	if err := s.SetAccountHealth(AccountHealth{PluginID: "mock"}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("missing account = %v, want ErrInvalidArgument", err)
	}
}

func TestCheckinDedupColumns(t *testing.T) {
	s := openTestStore(t)

	// A device-scoped success records the key.
	if _, err := s.UpsertCheckinRecord(model.CheckinRecord{
		PluginID: "mock", AccountID: "acc1", Date: "2026-10-20",
		Result: model.CheckinOK, At: 100,
		DedupScope: model.DedupScopeDevice, DeviceKey: "dev-a",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	records, err := s.QueryCheckinRecords("mock", "2026-10-20", "2026-10-20")
	if err != nil || len(records) != 1 {
		t.Fatalf("query = %+v err=%v", records, err)
	}
	if records[0].DedupScope != model.DedupScopeDevice || records[0].DeviceKey != "dev-a" {
		t.Fatalf("dedup columns = %+v", records[0])
	}
	if ok, err := s.DeviceCheckedIn("mock", "2026-10-20", "dev-a"); err != nil || !ok {
		t.Fatalf("DeviceCheckedIn ok=%v err=%v", ok, err)
	}

	// A later failure without a key must neither downgrade the day's success nor
	// erase the recorded key, so the device still counts as signed.
	if _, err := s.UpsertCheckinRecord(model.CheckinRecord{
		PluginID: "mock", AccountID: "acc1", Date: "2026-10-20",
		Result: model.CheckinFail, At: 200,
	}); err != nil {
		t.Fatalf("upsert fail: %v", err)
	}
	records, _ = s.QueryCheckinRecords("mock", "2026-10-20", "2026-10-20")
	if records[0].Result != model.CheckinOK || records[0].DeviceKey != "dev-a" {
		t.Fatalf("success downgraded or key lost on overwrite: %+v", records[0])
	}
	if ok, _ := s.DeviceCheckedIn("mock", "2026-10-20", "dev-a"); !ok {
		t.Fatal("a success followed by a failure no longer counted as a device check-in")
	}

	// A different device has not signed.
	if ok, _ := s.DeviceCheckedIn("mock", "2026-10-20", "dev-b"); ok {
		t.Fatal("unknown device reported as signed")
	}
	// Account-scoped rows never satisfy the device query.
	if _, err := s.UpsertCheckinRecord(model.CheckinRecord{
		PluginID: "mock", AccountID: "acc2", Date: "2026-10-20",
		Result: model.CheckinOK, At: 300,
		DedupScope: model.DedupScopeAccount, DeviceKey: "dev-c",
	}); err != nil {
		t.Fatalf("upsert account scope: %v", err)
	}
	if ok, _ := s.DeviceCheckedIn("mock", "2026-10-20", "dev-c"); ok {
		t.Fatal("account-scoped success counted as a device check-in")
	}

	// LastDeviceKey only considers device-scoped rows, newest first.
	if _, err := s.UpsertCheckinRecord(model.CheckinRecord{
		PluginID: "mock", AccountID: "acc1", Date: "2026-10-19",
		Result: model.CheckinOK, At: 50,
		DedupScope: model.DedupScopeDevice, DeviceKey: "dev-old",
	}); err != nil {
		t.Fatalf("upsert older: %v", err)
	}
	key, ok, err := s.LastDeviceKey("mock", "acc1")
	if err != nil || !ok || key != "dev-a" {
		t.Fatalf("LastDeviceKey = %q/%v/%v, want dev-a", key, ok, err)
	}
	if _, ok, _ := s.LastDeviceKey("mock", "never"); ok {
		t.Fatal("LastDeviceKey found a key for an unknown account")
	}

	// CheckinSucceededOn only counts ok/already. acc1 kept its success through
	// the later failure; a failure-only account must not be reported.
	if ok, err := s.CheckinSucceededOn("mock", "acc1", "2026-10-20"); err != nil || !ok {
		t.Fatalf("preserved success not reported: ok=%v err=%v", ok, err)
	}
	if _, err := s.UpsertCheckinRecord(model.CheckinRecord{
		PluginID: "mock", AccountID: "acc3", Date: "2026-10-20", Result: model.CheckinFail,
	}); err != nil {
		t.Fatalf("upsert acc3: %v", err)
	}
	if ok, err := s.CheckinSucceededOn("mock", "acc3", "2026-10-20"); err != nil || ok {
		t.Fatalf("failed row reported as success: ok=%v err=%v", ok, err)
	}
	if ok, err := s.CheckinSucceededOn("mock", "acc2", "2026-10-20"); err != nil || !ok {
		t.Fatalf("ok row not reported: ok=%v err=%v", ok, err)
	}

	if _, err := s.DeviceCheckedIn("mock", "2026-10-20", ""); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty device = %v, want ErrInvalidArgument", err)
	}
	if _, err := s.CheckinSucceededOn("mock", "", "2026-10-20"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty account = %v, want ErrInvalidArgument", err)
	}
}
