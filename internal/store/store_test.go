package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aiworkgate/internal/model"
)

// testTempDir is t.TempDir with a retrying cleanup: SQLite may still hold the
// WAL/SHM handles for a moment after Close, which makes RemoveAll fail with
// "The directory is not empty" on Windows.
func testTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "aiworkgate-store-test-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() {
		for i := 0; i < 50; i++ {
			if err := os.RemoveAll(dir); err == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		_ = os.RemoveAll(dir)
	})
	return dir
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(testTempDir(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

func TestOpenMigrateAndPragmas(t *testing.T) {
	dir := testTempDir(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if got, want := s.Path(), filepath.Join(dir, dbFileName); got != want {
		t.Fatalf("Path = %q, want %q", got, want)
	}

	db := s.DB()
	var journal string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if journal != "wal" {
		t.Fatalf("journal_mode = %q, want wal", journal)
	}
	var sync int
	if err := db.QueryRow(`PRAGMA synchronous`).Scan(&sync); err != nil {
		t.Fatalf("synchronous: %v", err)
	}
	if sync != 1 { // 1 == NORMAL
		t.Fatalf("synchronous = %d, want 1 (NORMAL)", sync)
	}
	var busy int
	if err := db.QueryRow(`PRAGMA busy_timeout`).Scan(&busy); err != nil {
		t.Fatalf("busy_timeout: %v", err)
	}
	if busy != busyTimeoutMS {
		t.Fatalf("busy_timeout = %d, want %d", busy, busyTimeoutMS)
	}

	version, err := s.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if version != len(migrations) {
		t.Fatalf("user_version = %d, want %d", version, len(migrations))
	}

	// Migrate must be idempotent.
	if err := s.Migrate(); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if v, _ := s.SchemaVersion(); v != version {
		t.Fatalf("user_version changed to %d", v)
	}

	// Reopening the same directory keeps working.
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if v, _ := s2.SchemaVersion(); v != len(migrations) {
		t.Fatalf("reopened user_version = %d", v)
	}
}

func TestAPIRequestLogs(t *testing.T) {
	s := openTestStore(t)
	base := time.Now().Add(-time.Hour).Truncate(time.Second)

	sources := []string{"a", "plugin:mock", "a", "plugin:mock", "a"}
	for i := 0; i < 5; i++ {
		rec := model.APIRequestLog{
			TS:        base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339),
			ReqID:     "req-" + string(rune('a'+i)),
			Source:    sources[i],
			Model:     "m-" + string(rune('0'+i%2)),
			KeyID:     "key-1",
			KeyName:   "default",
			Stream:    i%2 == 0,
			Status:    200,
			LatencyMS: int64(100 + i),
		}
		if i == 4 {
			rec.Status = 500
			rec.Error = "upstream exploded"
		}
		if err := s.InsertAPIRequestLog(rec); err != nil {
			t.Fatalf("InsertAPIRequestLog(%d): %v", i, err)
		}
	}

	all, total, err := s.QueryAPIRequestLogs(Filter{})
	if err != nil {
		t.Fatalf("QueryAPIRequestLogs: %v", err)
	}
	if total != 5 || len(all) != 5 {
		t.Fatalf("all = %d rows, total %d; want 5/5", len(all), total)
	}
	if all[0].ReqID != "req-e" || all[4].ReqID != "req-a" {
		t.Fatalf("order = %s..%s; want newest first", all[0].ReqID, all[4].ReqID)
	}
	if !all[0].Stream {
		t.Fatalf("stream flag not round-tripped")
	}
	if all[0].Error != "upstream exploded" {
		t.Fatalf("error = %q", all[0].Error)
	}

	// Filter by source.
	plug, n, err := s.QueryAPIRequestLogs(Filter{Source: "plugin:mock"})
	if err != nil {
		t.Fatalf("filter source: %v", err)
	}
	if n != 2 || len(plug) != 2 {
		t.Fatalf("source filter = %d/%d, want 2/2", len(plug), n)
	}

	// Filter by model + status (row e has status 500 and model m-0).
	byModel, n, err := s.QueryAPIRequestLogs(Filter{Model: "m-0", Status: 500})
	if err != nil {
		t.Fatalf("filter model: %v", err)
	}
	if n != 1 || len(byModel) != 1 || byModel[0].ReqID != "req-e" {
		t.Fatalf("model+status filter = %+v", byModel)
	}

	// Filter by key id.
	if _, n, err := s.QueryAPIRequestLogs(Filter{KeyID: "key-1"}); err != nil || n != 5 {
		t.Fatalf("key filter n = %d, err %v", n, err)
	}
	if _, n, err := s.QueryAPIRequestLogs(Filter{KeyID: "nope"}); err != nil || n != 0 {
		t.Fatalf("unknown key filter n = %d, err %v", n, err)
	}

	// Time window: from base+1min up to base+2min => 2 rows (inclusive bounds).
	from := base.Add(time.Minute).Unix()
	to := base.Add(2 * time.Minute).Unix()
	win, n, err := s.QueryAPIRequestLogs(Filter{From: from, To: to})
	if err != nil {
		t.Fatalf("filter window: %v", err)
	}
	if n != 2 || len(win) != 2 {
		t.Fatalf("window = %d/%d, want 2/2", len(win), n)
	}

	// Pagination: newest first, limit 2 offset 1.
	page, n, err := s.QueryAPIRequestLogs(Filter{Limit: 2, Offset: 1})
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	if n != 5 || len(page) != 2 {
		t.Fatalf("page = %d rows total %d, want 2/5", len(page), n)
	}
	if page[0].ReqID != "req-d" || page[1].ReqID != "req-c" {
		t.Fatalf("page ids = %s,%s; want req-d,req-c", page[0].ReqID, page[1].ReqID)
	}

	// Cleanup everything older than base+2min (two rows).
	removed, err := s.CleanupAPIRequestLogs(base.Add(2 * time.Minute).Unix())
	if err != nil {
		t.Fatalf("CleanupAPIRequestLogs: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	if _, n, err := s.QueryAPIRequestLogs(Filter{}); err != nil || n != 3 {
		t.Fatalf("after cleanup n = %d, err %v", n, err)
	}
	if _, err := s.CleanupAPIRequestLogs(0); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("cleanup with 0 = %v, want ErrInvalidArgument", err)
	}
}

func TestCheckinRecordUpsert(t *testing.T) {
	s := openTestStore(t)

	cb, ca := 10.5, 20.0
	id1, err := s.UpsertCheckinRecord(model.CheckinRecord{
		PluginID: "mock", AccountID: "acc1", AccountLabel: "first",
		Date: "2026-10-01", Result: model.CheckinOK, CreditsBefore: &cb, CreditsAfter: &ca,
		Message: "ok", At: 100,
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if id1 == 0 {
		t.Fatalf("id = 0")
	}
	if n, err := s.checkinRowCount(); err != nil || n != 1 {
		t.Fatalf("rows = %d, err %v", n, err)
	}

	// Same plugin/account/date must update in place.
	ca2 := 30.0
	id2, err := s.UpsertCheckinRecord(model.CheckinRecord{
		PluginID: "mock", AccountID: "acc1", AccountLabel: "renamed",
		Date: "2026-10-01", Result: model.CheckinAlready, CreditsAfter: &ca2, At: 200,
	})
	if err != nil {
		t.Fatalf("upsert again: %v", err)
	}
	if id2 != id1 {
		t.Fatalf("upsert id = %d, want %d", id2, id1)
	}
	if n, _ := s.checkinRowCount(); n != 1 {
		t.Fatalf("rows after upsert = %d, want 1", n)
	}

	if _, err := s.UpsertCheckinRecord(model.CheckinRecord{PluginID: "mock", AccountID: "acc2", Date: "2026-10-02", Result: model.CheckinFail}); err != nil {
		t.Fatalf("upsert acc2: %v", err)
	}
	if _, err := s.UpsertCheckinRecord(model.CheckinRecord{PluginID: "other", AccountID: "acc1", Date: "2026-10-01", Result: model.CheckinOK}); err != nil {
		t.Fatalf("upsert other: %v", err)
	}
	if _, err := s.UpsertCheckinRecord(model.CheckinRecord{PluginID: "mock", AccountID: "acc1", Date: "2026-09-30", Result: model.CheckinOK}); err != nil {
		t.Fatalf("upsert older: %v", err)
	}

	recs, err := s.QueryCheckinRecords("mock", "", "")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("mock rows = %d, want 3", len(recs))
	}
	if recs[0].Date != "2026-10-02" {
		t.Fatalf("first date = %s, want 2026-10-02", recs[0].Date)
	}
	// Locate the upserted row and check its latest values.
	var found bool
	for _, r := range recs {
		if r.AccountID == "acc1" && r.Date == "2026-10-01" {
			found = true
			if r.Result != model.CheckinAlready || r.AccountLabel != "renamed" {
				t.Fatalf("upserted row = %+v", r)
			}
			if r.CreditsBefore != nil {
				t.Fatalf("credits_before = %v, want nil after overwrite", *r.CreditsBefore)
			}
			if r.CreditsAfter == nil || *r.CreditsAfter != 30 {
				t.Fatalf("credits_after = %v, want 30", r.CreditsAfter)
			}
			if r.At != 200 {
				t.Fatalf("at = %d, want 200", r.At)
			}
		}
	}
	if !found {
		t.Fatalf("upserted row missing")
	}

	// Bounded query.
	bounded, err := s.QueryCheckinRecords("mock", "2026-10-01", "2026-10-02")
	if err != nil {
		t.Fatalf("bounded query: %v", err)
	}
	if len(bounded) != 2 {
		t.Fatalf("bounded = %d rows, want 2", len(bounded))
	}

	if _, err := s.UpsertCheckinRecord(model.CheckinRecord{PluginID: "mock", Date: "2026-10-03"}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("missing account_id = %v, want ErrInvalidArgument", err)
	}

	removed, err := s.CleanupCheckinRecords("2026-10-01")
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if removed != 1 {
		t.Fatalf("cleanup removed = %d, want 1", removed)
	}
	if _, err := s.CleanupCheckinRecords(""); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty cleanup date = %v", err)
	}
}

func TestQuotaSnapshots(t *testing.T) {
	s := openTestStore(t)

	snaps := []model.QuotaSnapshot{
		{PluginID: "mock", AccountID: "acc1", Remaining: 9, Total: 10, At: 1000},
		{PluginID: "mock", AccountID: "acc1", Remaining: 8, Total: 10, At: 2000},
		{PluginID: "mock", AccountID: "acc2", Remaining: 1, Total: 5, At: 3000},
	}
	for _, snap := range snaps {
		if err := s.SaveQuotaSnapshot(snap); err != nil {
			t.Fatalf("SaveQuotaSnapshot: %v", err)
		}
	}
	// Same (plugin, account, at) replaces.
	if err := s.SaveQuotaSnapshot(model.QuotaSnapshot{PluginID: "mock", AccountID: "acc1", Remaining: 7, Total: 10, At: 2000}); err != nil {
		t.Fatalf("replace snapshot: %v", err)
	}

	acc1, err := s.QueryQuotaSnapshots("mock", "acc1", 0)
	if err != nil {
		t.Fatalf("query acc1: %v", err)
	}
	if len(acc1) != 2 {
		t.Fatalf("acc1 snapshots = %d, want 2", len(acc1))
	}
	if acc1[0].At != 2000 || acc1[0].Remaining != 7 {
		t.Fatalf("newest acc1 = %+v", acc1[0])
	}

	all, err := s.QueryQuotaSnapshots("mock", "", 0)
	if err != nil {
		t.Fatalf("query all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("all snapshots = %d, want 3", len(all))
	}

	since, err := s.QueryQuotaSnapshots("mock", "", 2000)
	if err != nil {
		t.Fatalf("query since: %v", err)
	}
	if len(since) != 2 {
		t.Fatalf("since snapshots = %d, want 2", len(since))
	}

	removed, err := s.CleanupQuotaSnapshots(2000)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if removed != 1 {
		t.Fatalf("cleanup removed = %d, want 1", removed)
	}
}

func TestClientKeys(t *testing.T) {
	s := openTestStore(t)

	if err := s.UpsertClientKey("key-1", "main", "agk_aaa", "hash-1", true, 100); err != nil {
		t.Fatalf("upsert key-1: %v", err)
	}
	if err := s.UpsertClientKey("key-2", "second", "agk_bbb", "hash-2", false, 200); err != nil {
		t.Fatalf("upsert key-2: %v", err)
	}

	list, err := s.ListClientKeys()
	if err != nil {
		t.Fatalf("ListClientKeys: %v", err)
	}
	if len(list) != 2 || list[0].ID != "key-2" {
		t.Fatalf("list = %+v", list)
	}
	if list[0].Enabled {
		t.Fatalf("key-2 should be disabled")
	}

	hash, ok, err := s.ClientKeyHashByID("key-1")
	if err != nil || !ok || hash != "hash-1" {
		t.Fatalf("hash by id = %q/%v/%v", hash, ok, err)
	}
	if _, ok, err := s.ClientKeyHashByID("missing"); err != nil || ok {
		t.Fatalf("missing hash ok=%v err=%v", ok, err)
	}

	id, ok, err := s.ClientKeyIDByHash("hash-2")
	if err != nil || !ok || id != "key-2" {
		t.Fatalf("id by hash = %q/%v/%v", id, ok, err)
	}
	if _, ok, err := s.ClientKeyIDByHash("nope"); err != nil || ok {
		t.Fatalf("missing id ok=%v err=%v", ok, err)
	}

	if err := s.TouchClientKey("key-1", 500); err != nil {
		t.Fatalf("TouchClientKey: %v", err)
	}
	list, _ = s.ListClientKeys()
	for _, k := range list {
		if k.ID == "key-1" && k.LastUsedAt != 500 {
			t.Fatalf("last_used_at = %d, want 500", k.LastUsedAt)
		}
	}

	// Touches within 30s are dropped so a busy client does not rewrite the row
	// on every request; a later touch still lands.
	if err := s.TouchClientKey("key-1", 520); err != nil {
		t.Fatalf("throttled TouchClientKey: %v", err)
	}
	if got := lastUsedOf(t, s, "key-1"); got != 500 {
		t.Fatalf("throttled touch rewrote last_used_at = %d, want 500", got)
	}
	if err := s.TouchClientKey("key-1", 531); err != nil {
		t.Fatalf("TouchClientKey: %v", err)
	}
	if got := lastUsedOf(t, s, "key-1"); got != 531 {
		t.Fatalf("last_used_at = %d, want 531", got)
	}

	// Re-upsert keeps last_used_at and updates metadata.
	if err := s.UpsertClientKey("key-1", "renamed", "agk_aaa", "hash-1b", true, 0); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if h, _, _ := s.ClientKeyHashByID("key-1"); h != "hash-1b" {
		t.Fatalf("hash after update = %q", h)
	}
	list, _ = s.ListClientKeys()
	for _, k := range list {
		if k.ID == "key-1" {
			if k.Name != "renamed" || k.CreatedAt != 100 || k.LastUsedAt != 531 {
				t.Fatalf("after update = %+v", k)
			}
		}
	}

	if err := s.DeleteClientKey("key-1"); err != nil {
		t.Fatalf("DeleteClientKey: %v", err)
	}
	if list, _ := s.ListClientKeys(); len(list) != 1 {
		t.Fatalf("after delete = %d keys", len(list))
	}
	if err := s.UpsertClientKey("", "x", "", "h", true, 0); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty id = %v", err)
	}
}

func TestProxies(t *testing.T) {
	s := openTestStore(t)

	p := model.Proxy{ID: "px_1", Name: "HK", Type: "http", Host: "1.2.3.4", Port: 8080, Username: "u", Enabled: true}
	if err := s.UpsertProxy(p, "enc-secret"); err != nil {
		t.Fatalf("UpsertProxy: %v", err)
	}
	got, enc, err := s.GetProxy("px_1")
	if err != nil {
		t.Fatalf("GetProxy: %v", err)
	}
	if got.Name != "HK" || got.Port != 8080 || !got.Enabled || enc != "enc-secret" {
		t.Fatalf("proxy = %+v enc=%q", got, enc)
	}
	if got.Password != "" {
		t.Fatalf("password must not be read back into model")
	}

	// Updating without a new password keeps the stored blob.
	p.Name = "HK-2"
	if err := s.UpsertProxy(p, ""); err != nil {
		t.Fatalf("UpsertProxy update: %v", err)
	}
	if _, enc, _ := s.GetProxy("px_1"); enc != "enc-secret" {
		t.Fatalf("password blob lost: %q", enc)
	}

	p2 := model.Proxy{ID: "px_2", Name: "JP", Type: "socks5", Host: "5.6.7.8", Port: 1080}
	if err := s.UpsertProxy(p2, ""); err != nil {
		t.Fatalf("UpsertProxy px_2: %v", err)
	}

	if err := s.UpdateProxyHealth("px_1", 1000, "9.9.9.9", 42, ""); err != nil {
		t.Fatalf("UpdateProxyHealth: %v", err)
	}
	got, _, _ = s.GetProxy("px_1")
	if got.LastCheckAt != 1000 || got.LastExitIP != "9.9.9.9" || got.LastLatencyMS != 42 || got.LastError != "" {
		t.Fatalf("health = %+v", got)
	}
	if err := s.UpdateProxyHealth("px_2", 1100, "", 0, "dial timeout"); err != nil {
		t.Fatalf("UpdateProxyHealth error path: %v", err)
	}

	list, err := s.ListProxies()
	if err != nil {
		t.Fatalf("ListProxies: %v", err)
	}
	if len(list) != 2 || list[0].ID != "px_1" {
		t.Fatalf("list = %+v", list)
	}

	if _, _, err := s.GetProxy("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing proxy err = %v, want ErrNotFound", err)
	}

	if err := s.DeleteProxy("px_1"); err != nil {
		t.Fatalf("DeleteProxy: %v", err)
	}
	if list, _ := s.ListProxies(); len(list) != 1 {
		t.Fatalf("after delete = %d", len(list))
	}
}

func TestAccountEgress(t *testing.T) {
	s := openTestStore(t)

	if err := s.SetAccountEgress(model.AccountEgress{PluginID: "mock", AccountID: "acc2", Mode: model.EgressProxy, ProxyID: "px_1"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := s.SetAccountEgress(model.AccountEgress{PluginID: "mock", AccountID: "acc1", Mode: model.EgressBlocked}); err != nil {
		t.Fatalf("set acc1: %v", err)
	}
	if err := s.SetAccountEgress(model.AccountEgress{PluginID: "mock", AccountID: "acc2", Mode: model.EgressDirect}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if err := s.SetAccountEgress(model.AccountEgress{PluginID: "other", AccountID: "acc1", Mode: model.EgressDirect}); err != nil {
		t.Fatalf("set other: %v", err)
	}

	list, err := s.ListAccountEgress("mock")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("mock egress = %d, want 2", len(list))
	}
	if list[0].AccountID != "acc1" || list[0].Mode != model.EgressBlocked {
		t.Fatalf("acc1 = %+v", list[0])
	}
	if list[1].AccountID != "acc2" || list[1].Mode != model.EgressDirect {
		t.Fatalf("acc2 = %+v", list[1])
	}

	if err := s.DeleteAccountEgress("mock", "acc2"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	list, _ = s.ListAccountEgress("mock")
	if len(list) != 1 || list[0].AccountID != "acc1" {
		t.Fatalf("after delete = %+v", list)
	}
	if _, err := s.ListAccountEgress(""); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty plugin = %v", err)
	}
}

func TestPluginSettings(t *testing.T) {
	s := openTestStore(t)

	if _, _, _, _, _, ok, err := s.GetPluginSettings("mock"); err != nil || ok {
		t.Fatalf("unknown plugin ok=%v err=%v", ok, err)
	}

	if err := s.UpsertPluginSettings("mock", true, model.StartupOnDemand, 4, "08:30", true, 111); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	enabled, mode, conc, checkin, strict, ok, err := s.GetPluginSettings("mock")
	if err != nil || !ok {
		t.Fatalf("get ok=%v err=%v", ok, err)
	}
	if !enabled || mode != model.StartupOnDemand || conc != 4 || checkin != "08:30" || !strict {
		t.Fatalf("settings = %v %q %d %q %v", enabled, mode, conc, checkin, strict)
	}

	// installed_at survives updates that pass 0; updated_at moves.
	if err := s.UpsertPluginSettings("mock", false, model.StartupAlways, 2, "09:00", false, 0); err != nil {
		t.Fatalf("update: %v", err)
	}
	list, err := s.ListPluginSettings()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("list = %d", len(list))
	}
	got := list[0]
	if got.PluginID != "mock" || got.Enabled || got.Mode != model.StartupAlways || got.Concurrency != 2 || got.CheckinTime != "09:00" || got.Strict {
		t.Fatalf("list row = %+v", got)
	}
	if got.InstalledAt != 111 {
		t.Fatalf("installed_at = %d, want 111", got.InstalledAt)
	}
	if got.UpdatedAt == 0 {
		t.Fatalf("updated_at not set")
	}
}

func TestRulePacks(t *testing.T) {
	s := openTestStore(t)

	if _, ok, err := s.GetRulePack("wb"); err != nil || ok {
		t.Fatalf("missing pack ok=%v err=%v", ok, err)
	}
	if err := s.UpsertRulePack("wb-moderation", `{"name":"wb"}`); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.UpsertRulePack("zz", `{"name":"zz"}`); err != nil {
		t.Fatalf("upsert zz: %v", err)
	}
	if err := s.UpsertRulePack("wb-moderation", `{"name":"wb2"}`); err != nil {
		t.Fatalf("replace: %v", err)
	}

	content, ok, err := s.GetRulePack("wb-moderation")
	if err != nil || !ok || content != `{"name":"wb2"}` {
		t.Fatalf("get = %q/%v/%v", content, ok, err)
	}

	list, err := s.ListRulePacks()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 || list[0].Name != "wb-moderation" || list[0].UpdatedAt == 0 {
		t.Fatalf("list = %+v", list)
	}

	if err := s.DeleteRulePack("wb-moderation"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok, _ := s.GetRulePack("wb-moderation"); ok {
		t.Fatalf("pack still present")
	}
	if err := s.DeleteRulePack("absent"); err != nil {
		t.Fatalf("delete absent: %v", err)
	}
	if err := s.UpsertRulePack("", "x"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty name = %v", err)
	}
}

func TestKV(t *testing.T) {
	s := openTestStore(t)

	if _, ok, err := s.GetKV("missing"); err != nil || ok {
		t.Fatalf("missing kv ok=%v err=%v", ok, err)
	}
	if err := s.SetKV("ui.theme", "dark"); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	v, ok, err := s.GetKV("ui.theme")
	if err != nil || !ok || v != "dark" {
		t.Fatalf("GetKV = %q/%v/%v", v, ok, err)
	}
	if err := s.SetKV("ui.theme", `{"dark":true}`); err != nil {
		t.Fatalf("SetKV replace: %v", err)
	}
	if v, _, _ := s.GetKV("ui.theme"); v != `{"dark":true}` {
		t.Fatalf("replaced value = %q", v)
	}
	if err := s.DeleteKV("ui.theme"); err != nil {
		t.Fatalf("DeleteKV: %v", err)
	}
	if _, ok, _ := s.GetKV("ui.theme"); ok {
		t.Fatalf("value still present")
	}
	if err := s.SetKV("", "x"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty key = %v", err)
	}
}

func TestResetHelper(t *testing.T) {
	s := openTestStore(t)
	if err := s.InsertAPIRequestLog(model.APIRequestLog{Source: "a"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s.resetForTests(); err != nil {
		t.Fatalf("resetForTests: %v", err)
	}
	if _, n, err := s.QueryAPIRequestLogs(Filter{}); err != nil || n != 0 {
		t.Fatalf("after reset n=%d err=%v", n, err)
	}
}

func TestInsertDefaultsTimestamp(t *testing.T) {
	s := openTestStore(t)
	if err := s.InsertAPIRequestLog(model.APIRequestLog{ReqID: "r", Source: "a"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, _, err := s.QueryAPIRequestLogs(Filter{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	if _, err := time.Parse(time.RFC3339, rows[0].TS); err != nil {
		t.Fatalf("ts %q not RFC3339: %v", rows[0].TS, err)
	}
}

// rewindSchema drops user_version back so a real upgrade path runs; the steps
// themselves are idempotent, so only the version needs to move.
func rewindSchema(t *testing.T, s *Store, version int) {
	t.Helper()
	if _, err := s.DB().Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	if v, err := s.SchemaVersion(); err != nil || v != version {
		t.Fatalf("user_version = %d (%v) after rewind, want %d", v, err, version)
	}
}

// backupNames lists the pre-migration backups in a data dir. The WAL copies
// belong to those backups, so they are not counted on their own.
func backupNames(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, dbFileName+".bak-v*"))
	if err != nil {
		t.Fatalf("glob backups: %v", err)
	}
	var names []string
	for _, m := range matches {
		if !strings.HasSuffix(m, "-wal") {
			names = append(names, filepath.Base(m))
		}
	}
	return names
}

// readBackupKV queries one kv row out of a backup copy, proving it is a real,
// restorable database rather than a truncated file.
func readBackupKV(t *testing.T, path, key string) string {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.ToSlash(path))
	if err != nil {
		t.Fatalf("open backup %s: %v", filepath.Base(path), err)
	}
	defer db.Close()
	var value string
	if err := db.QueryRow("SELECT COALESCE(content, '') FROM kv WHERE key = ?", key).Scan(&value); err != nil {
		t.Fatalf("query backup %s: %v", filepath.Base(path), err)
	}
	return value
}

func TestMigrateBacksUpDatabaseBeforeUpgrade(t *testing.T) {
	dir := testTempDir(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	from := len(migrations) - 1
	to := len(migrations)
	if err := s.SetKV("backup.key", "dark"); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	rewindSchema(t, s, from)
	if got := backupNames(t, dir); len(got) != 0 {
		t.Fatalf("backups exist before the upgrade: %v", got)
	}

	if err := s.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if v, err := s.SchemaVersion(); err != nil || v != to {
		t.Fatalf("user_version = %d (%v), want %d", v, err, to)
	}

	want := fmt.Sprintf("%s.bak-v%d-v%d", dbFileName, from, to)
	got := backupNames(t, dir)
	if len(got) != 1 || got[0] != want {
		t.Fatalf("backups after the upgrade = %v, want [%s]", got, want)
	}
	backup := filepath.Join(dir, want)
	if st, err := os.Stat(backup); err != nil {
		t.Fatalf("backup missing: %v", err)
	} else if st.Size() == 0 {
		t.Fatal("backup is empty")
	}
	// The store stays open in WAL mode, so the log belongs to the snapshot too.
	if _, err := os.Stat(backup + "-wal"); err != nil {
		t.Errorf("wal copy of the backup missing: %v", err)
	}

	if value, ok, err := s.GetKV("backup.key"); err != nil || !ok || value != "dark" {
		t.Fatalf("live row after upgrade = %q/%v/%v", value, ok, err)
	}
	if value := readBackupKV(t, backup, "backup.key"); value != "dark" {
		t.Fatalf("backup row = %q, want dark", value)
	}
}

func TestMigrateDoesNotDuplicateOrOverwriteBackup(t *testing.T) {
	dir := testTempDir(t)
	from := len(migrations) - 1
	to := len(migrations)

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.SetKV("backup.key", "dark"); err != nil {
		s.Close()
		t.Fatalf("SetKV: %v", err)
	}
	rewindSchema(t, s, from)
	if err := s.Migrate(); err != nil {
		s.Close()
		t.Fatalf("Migrate: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopens at the current version run no step, so they add no backup.
	for attempt := 1; attempt <= 2; attempt++ {
		reopened, err := Open(dir)
		if err != nil {
			t.Fatalf("Open attempt %d: %v", attempt, err)
		}
		if value, ok, err := reopened.GetKV("backup.key"); err != nil || !ok || value != "dark" {
			reopened.Close()
			t.Fatalf("row after reopen %d = %q/%v/%v", attempt, value, ok, err)
		}
		if err := reopened.Close(); err != nil {
			t.Fatalf("Close attempt %d: %v", attempt, err)
		}
	}
	if got := backupNames(t, dir); len(got) != 1 {
		t.Fatalf("backups = %v, want the single original", got)
	}

	// A forced rewind to the same pair must reuse, never overwrite, the snapshot.
	again, err := Open(dir)
	if err != nil {
		t.Fatalf("Open after reopens: %v", err)
	}
	defer again.Close()
	if err := again.SetKV("backup.key", "light"); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	rewindSchema(t, again, from)
	if err := again.Migrate(); err != nil {
		t.Fatalf("Migrate after rewind: %v", err)
	}
	if got := backupNames(t, dir); len(got) != 1 {
		t.Fatalf("backups = %v, want the single original", got)
	}
	backup := filepath.Join(dir, fmt.Sprintf("%s.bak-v%d-v%d", dbFileName, from, to))
	if value := readBackupKV(t, backup, "backup.key"); value != "dark" {
		t.Fatalf("the existing backup was overwritten with %q, want dark", value)
	}
}

func TestMigrateSkipsBackupForFreshDatabase(t *testing.T) {
	dir := testTempDir(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if v, err := s.SchemaVersion(); err != nil || v != len(migrations) {
		t.Fatalf("user_version = %d (%v), want %d", v, err, len(migrations))
	}
	// A database with no tables yet has no state worth protecting.
	if got := backupNames(t, dir); len(got) != 0 {
		t.Fatalf("a brand new database created backups: %v", got)
	}
}

// lastUsedOf reads one key's last_used_at through the public list.
func lastUsedOf(t *testing.T, s *Store, id string) int64 {
	t.Helper()
	list, err := s.ListClientKeys()
	if err != nil {
		t.Fatalf("ListClientKeys: %v", err)
	}
	for _, k := range list {
		if k.ID == id {
			return k.LastUsedAt
		}
	}
	t.Fatalf("client key %s not found", id)
	return 0
}
