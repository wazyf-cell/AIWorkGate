package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Credential storage and the "configuration error" paths. These tests must never
// leave a token in the clear, and must never let the plugin pretend to be
// configured when it is not.

func TestNewStoreRejectsEmptyDataDir(t *testing.T) {
	// A plugin with nowhere to keep its sealed file must fail at startup rather
	// than write credentials next to the executable.
	if _, err := newStore("   "); err == nil {
		t.Fatal("expected an error for an empty data dir")
	}
}

func TestStoreStartsEmptyAndNotConfigured(t *testing.T) {
	dir := t.TempDir()
	st, err := newStore(dir)
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	if st.configured() {
		t.Fatal("a fresh store must not report itself configured")
	}
	if got := st.list(); len(got) != 0 {
		t.Fatalf("accounts = %d, want 0", len(got))
	}
	if _, statErr := os.Stat(filepath.Join(dir, accountsFile)); !os.IsNotExist(statErr) {
		t.Fatal("no accounts.dat must be written before an account is stored")
	}
}

func TestStoreRejectsAccountWithoutToken(t *testing.T) {
	st, err := newStore(t.TempDir())
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	if err := st.upsert(newAccount(accountData{UserID: "u1", Region: regionCN})); err == nil {
		t.Fatal("expected a refusal when accessToken is empty")
	}
	// The refusal must be honest, not a silent wipe of a working login.
	if st.configured() {
		t.Fatal("store must still be unconfigured after a refused write")
	}
}

func TestStoreUpsertSealsAndReloads(t *testing.T) {
	dir := t.TempDir()
	st, err := newStore(dir)
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	a := newAccount(accountData{
		AccessToken: "Bearer test-access-token-not-a-real-secret",
		UserID:      "uid-1",
		UserName:    "tester",
		Region:      regionCN,
	})
	if err := st.upsert(a); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !st.configured() {
		t.Fatal("store must be configured after a successful write")
	}

	raw, err := os.ReadFile(filepath.Join(dir, accountsFile))
	if err != nil {
		t.Fatalf("read %s: %v", accountsFile, err)
	}
	// The point of DPAPI: the token must not appear in the file at all.
	if strings.Contains(string(raw), "test-access-token") {
		t.Fatal("the access token is readable in the sealed file")
	}
	if strings.Contains(string(raw), "uid-1") {
		t.Fatal("the user id is readable in the sealed file")
	}

	// A second store over the same directory must unseal what the first wrote.
	st2, err := newStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	list := st2.list()
	if len(list) != 1 {
		t.Fatalf("reloaded accounts = %d, want 1", len(list))
	}
	got := list[0].snapshot()
	if got.UserID != "uid-1" || got.UserName != "tester" {
		t.Fatalf("reloaded account = %+v", got)
	}
	// A device id must be minted on write, because the upstream fingerprints it.
	if strings.TrimSpace(got.DeviceID) == "" {
		t.Fatal("deviceId must be minted when it is absent")
	}
}

func TestStoreUpsertReplacesSameRegionAccount(t *testing.T) {
	st, _ := newStore(t.TempDir())
	mk := func(tok string) *account {
		return newAccount(accountData{AccessToken: tok, UserID: "uid-1", Region: regionCN})
	}
	if err := st.upsert(mk("t1")); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if err := st.upsert(mk("t2")); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if got := len(st.list()); got != 1 {
		t.Fatalf("accounts = %d, want 1 (same region+uid replaces)", got)
	}
	// The same uid in another region is a different account.
	if err := st.upsert(newAccount(accountData{AccessToken: "t3", UserID: "uid-1", Region: regionGlobal})); err != nil {
		t.Fatalf("global upsert: %v", err)
	}
	if got := len(st.list()); got != 2 {
		t.Fatalf("accounts = %d, want 2 after adding the global region", got)
	}
}

func TestStoreRemoveDropsAccount(t *testing.T) {
	st, _ := newStore(t.TempDir())
	if err := st.upsert(newAccount(accountData{AccessToken: "t", UserID: "u", Region: regionCN})); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !st.remove("u") {
		t.Fatal("remove must report success for a stored account")
	}
	if st.remove("u") {
		t.Fatal("remove must report false for an absent account")
	}
	if st.configured() {
		t.Fatal("store must be unconfigured after removing its only account")
	}
}

func TestStoreRejectsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, accountsFile), []byte("not a sealed blob"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// A file that cannot be unsealed must be a hard error: continuing would
	// silently look like "no accounts" and hide a real problem.
	if _, err := newStore(dir); err == nil {
		t.Fatal("expected an error for an unreadable sealed file")
	}
}

func TestStateKeySeparatesRegions(t *testing.T) {
	if stateKey(regionCN, "u") == stateKey(regionGlobal, "u") {
		t.Fatal("the same uid in two regions must not share scheduler state")
	}
	if got := stateKey("", "u"); got != "cn/u" {
		t.Fatalf("stateKey(\"\") = %q, want cn/u", got)
	}
}

func TestPoolCooldownAndReenable(t *testing.T) {
	st, _ := newStore(t.TempDir())
	a := newAccount(accountData{AccessToken: "t", UserID: "u", Region: regionCN})
	if err := st.upsert(a); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if st.pick(nil) == nil {
		t.Fatal("a healthy account must be picked")
	}
	st.cooldown(a, "hard_credit", 3600, "余额不足")
	if st.pick(nil) != nil {
		t.Fatal("a cooling account must not be picked")
	}
	if c := st.counts(); c.Cooling != 1 || c.Healthy != 0 {
		t.Fatalf("counts = %+v, want one cooling", c)
	}
	st.reenable(a)
	if st.pick(nil) == nil {
		t.Fatal("a re-enabled account must be pickable again")
	}
}

func TestPoolDisableHidesAccountFromPick(t *testing.T) {
	st, _ := newStore(t.TempDir())
	a := newAccount(accountData{AccessToken: "t", UserID: "u", Region: regionCN})
	if err := st.upsert(a); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	st.disable(a, "session dead")
	if st.pick(nil) != nil {
		t.Fatal("a disabled account must not be picked")
	}
	rows := st.statusList()
	if len(rows) != 1 || !rows[0].Disabled {
		t.Fatalf("statusList = %+v, want one disabled row", rows)
	}
}

func TestPoolPickHonoursTriedSet(t *testing.T) {
	st, _ := newStore(t.TempDir())
	for _, uid := range []string{"a", "b"} {
		if err := st.upsert(newAccount(accountData{AccessToken: "t", UserID: uid, Region: regionCN})); err != nil {
			t.Fatalf("upsert %s: %v", uid, err)
		}
	}
	tried := map[string]bool{"a": true, "b": true}
	if st.pick(tried) != nil {
		t.Fatal("pick must return nil when every account is already tried")
	}
}

func TestSanitizeRegionFallsBack(t *testing.T) {
	cases := map[string]string{
		"cn": regionCN, "global": regionGlobal, "all": "all",
		"CN": regionCN, "": regionCN, "bogus": regionGlobal,
	}
	for in, want := range cases {
		def := regionCN
		if in == "bogus" {
			def = regionGlobal
		}
		if got := sanitizeRegion(in, def); got != want {
			t.Errorf("sanitizeRegion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCredentialsIntakeAcceptsFlatAndCamelCase(t *testing.T) {
	// The intake handler is covered end to end in handlers_test.go; here we only
	// pin the JSON contract of the sealed body so a field rename cannot slip in.
	doc := accountData{AccessToken: "t", UserID: "u", Region: regionCN}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{"accessToken", "userId", "region"} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("sealed JSON is missing %q: %s", key, raw)
		}
	}
}
