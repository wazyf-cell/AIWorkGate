package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"aiworkgate/internal/model"
	"aiworkgate/internal/store"
	"aiworkgate/internal/webui"
)

// fakeAccountSource is an accountLister stub; no plugin process is involved.
type fakeAccountSource struct {
	plugins  []model.PluginStatus
	accounts map[string][]model.Account
	errs     map[string]error
}

func (f *fakeAccountSource) List() []model.PluginStatus { return f.plugins }

func (f *fakeAccountSource) Accounts(ctx context.Context, pluginID string) ([]model.Account, error) {
	if err := f.errs[pluginID]; err != nil {
		return nil, err
	}
	return f.accounts[pluginID], nil
}

func openAppWithStore(t *testing.T) (*App, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &App{store: st}, st
}

func TestMergedAccountsOverlayAndCacheFallback(t *testing.T) {
	app, st := openAppWithStore(t)

	priority := 40
	weight := 3.0
	override := false
	if err := st.UpsertAccountMeta(model.AccountMeta{
		PluginID: "mock", AccountID: "acc_1", Alias: "主号", GroupID: "grp_1",
		Priority: &priority, Weight: &weight, EnabledOverride: &override,
	}); err != nil {
		t.Fatalf("upsert meta: %v", err)
	}
	if err := st.SetAccountEgress(model.AccountEgress{
		PluginID: "mock", AccountID: "acc_1", Mode: model.EgressDirect,
	}); err != nil {
		t.Fatalf("set egress: %v", err)
	}

	src := &fakeAccountSource{
		plugins: []model.PluginStatus{{
			Manifest: model.Manifest{ID: "mock"},
			Enabled:  true,
			Running:  true,
		}},
		accounts: map[string][]model.Account{"mock": {
			{ID: "acc_1", Label: "user@example.com", Enabled: true, Status: model.AccountOK,
				Quota:      model.Quota{Remaining: 10, Plan: "Pro"},
				Credential: &model.Credential{Present: true},
				Identity:   &model.Identity{MachineID: "machine-1"}},
			{ID: "acc_2", Label: "second", Enabled: true, Status: model.AccountOK},
		}},
		errs: map[string]error{},
	}

	payload := app.accountsFrom(src, "").(map[string]any)
	entries := payload["accounts"].([]accountEntry)
	if len(entries) != 2 {
		t.Fatalf("entries = %+v", entries)
	}
	first := entries[0]
	if first.PluginID != "mock" || first.Alias != "主号" || first.DisplayName != "主号" {
		t.Fatalf("first = %+v", first)
	}
	if first.GroupID != "grp_1" || first.Priority != 40 || first.Weight != 3 {
		t.Fatalf("overlay = %+v", first)
	}
	if first.EnabledOverride == nil || *first.EnabledOverride {
		t.Fatalf("enabled_override = %v", first.EnabledOverride)
	}
	if first.EnabledEffective {
		t.Fatal("enabled_effective = true, want false with override false")
	}
	if first.Stale {
		t.Fatal("fresh listing marked stale")
	}
	if first.Egress == nil || first.Egress.Mode != model.EgressDirect {
		t.Fatalf("egress = %+v", first.Egress)
	}
	if first.Credential == nil || !first.Credential.Present || first.Quota.Plan != "Pro" {
		t.Fatalf("plugin credential/plan lost: %+v", first.Account)
	}
	second := entries[1]
	if second.DisplayName != "second" || second.Priority != model.DefaultAccountPriority ||
		second.Weight != model.DefaultAccountWeight || !second.EnabledEffective || second.Egress != nil {
		t.Fatalf("second = %+v", second)
	}

	// The successful listing refreshed the cache.
	cached, updatedAt, err := st.ListAccountCache("mock")
	if err != nil || len(cached) != 2 || updatedAt == 0 {
		t.Fatalf("cache = %+v updated=%d err=%v", cached, updatedAt, err)
	}

	// A failing plugin serves the cache with stale=true.
	src.errs["mock"] = errors.New("plugin is not running")
	payload = app.accountsFrom(src, "mock").(map[string]any)
	if payload["stale"] != true {
		t.Fatalf("stale flag = %v", payload["stale"])
	}
	if _, ok := payload["error"]; !ok {
		t.Fatal("offline listing carries no error")
	}
	staleEntries := payload["accounts"].([]accountEntry)
	if len(staleEntries) != 2 || !staleEntries[0].Stale {
		t.Fatalf("stale entries = %+v", staleEntries)
	}
	if staleEntries[0].DisplayName != "主号" || staleEntries[0].EnabledEffective {
		t.Fatalf("overlay lost on stale entries: %+v", staleEntries[0])
	}

	// Without a cache the offline plugin yields an empty list and the error.
	if err := st.DeletePluginAccountCache("mock"); err != nil {
		t.Fatal(err)
	}
	payload = app.accountsFrom(src, "mock").(map[string]any)
	if entries := payload["accounts"].([]accountEntry); len(entries) != 0 {
		t.Fatalf("uncached offline entries = %+v", entries)
	}
}

func TestRenameAccountAlias(t *testing.T) {
	app, st := openAppWithStore(t)

	if err := app.renameAccountAlias("mock", "acc_1", "  主号  "); err != nil {
		t.Fatalf("rename: %v", err)
	}
	meta, ok, err := st.GetAccountMeta("mock", "acc_1")
	if err != nil || !ok || meta.Alias != "主号" {
		t.Fatalf("meta = %+v, %v, %v", meta, ok, err)
	}
	// Other overlay fields are untouched by a rename.
	if meta.Priority != nil {
		t.Fatalf("priority = %v, want NULL", meta.Priority)
	}

	if err := app.renameAccountAlias("mock", "acc_1", ""); err != nil {
		t.Fatalf("clear alias: %v", err)
	}
	meta, _, _ = st.GetAccountMeta("mock", "acc_1")
	if meta.Alias != "" {
		t.Fatalf("alias = %q, want cleared", meta.Alias)
	}

	if err := app.renameAccountAlias("mock", "acc_1", strings.Repeat("a", 65)); !errors.Is(err, webui.ErrInvalidRequest) {
		t.Fatalf("long alias = %v, want ErrInvalidRequest", err)
	}

	audit, err := st.ListAudit(model.AuditAccount, 10)
	if err != nil || len(audit) != 2 {
		t.Fatalf("audit = %+v, %v", audit, err)
	}
	if audit[0].Action != "rename" || audit[0].Target != "mock/acc_1" {
		t.Fatalf("audit[0] = %+v", audit[0])
	}

	// A host without a store cannot rename.
	if err := (&App{}).renameAccountAlias("mock", "acc_1", "x"); err == nil {
		t.Fatal("rename without a store succeeded")
	}
}

func TestDeriveLoginState(t *testing.T) {
	derived := model.Manifest{
		ID: "officeace", Name: "OfficeAce", APILevel: 2,
		Capabilities: []string{model.CapLoginLocalState, model.CapRefresh, model.CapAccounts},
		Permissions:  model.Permissions{ReadPaths: []string{"%USERPROFILE%\\Office"}},
	}
	state := deriveLoginState(derived, 1234)
	if state.PluginID != "officeace" || state.Name != "OfficeAce" || state.APILevel != 2 {
		t.Fatalf("identity = %+v", state)
	}
	if state.Source != model.LoginSourceDerived {
		t.Fatalf("source = %q", state.Source)
	}
	if len(state.Methods) != 1 || state.Methods[0] != "local_state" {
		t.Fatalf("methods = %+v", state.Methods)
	}
	if !state.ReadsLocalState || !state.RequiresApp || !state.AutoRefresh {
		t.Fatalf("derived flags = %+v", state)
	}
	if state.CheckedAt != 1234 {
		t.Fatalf("checked_at = %d", state.CheckedAt)
	}

	explicit := model.Manifest{
		ID: "codearts", Name: "CodeArts", APILevel: 2,
		Capabilities: []string{model.CapLoginOAuth, model.CapLoginAPIKey, model.CapRefresh},
		Login: &model.LoginConfig{
			Methods:     []string{"oauth"},
			Notes:       "浏览器 OAuth",
			AutoRefresh: true,
		},
	}
	state = deriveLoginState(explicit, 99)
	if state.Source != model.LoginSourceManifest {
		t.Fatalf("source = %q", state.Source)
	}
	if len(state.Methods) != 1 || state.Methods[0] != "oauth" {
		t.Fatalf("manifest methods = %+v", state.Methods)
	}
	if state.ReadsLocalState || state.RequiresApp || !state.AutoRefresh {
		t.Fatalf("manifest flags = %+v", state)
	}
	if state.Notes != "浏览器 OAuth" {
		t.Fatalf("notes = %q", state.Notes)
	}

	// A manifest block without methods falls back to capabilities, and the
	// methods slice is never nil.
	empty := model.Manifest{ID: "mock", Capabilities: []string{model.CapLoginAPIKey}}
	state = deriveLoginState(empty, 1)
	if state.Source != model.LoginSourceDerived || len(state.Methods) != 1 || state.Methods[0] != "api_key" {
		t.Fatalf("empty state = %+v", state)
	}
	bare := deriveLoginState(model.Manifest{ID: "bare"}, 1)
	if bare.Methods == nil {
		t.Fatal("methods must be an empty slice, not nil")
	}
	if bare.Name != "bare" {
		t.Fatalf("name fallback = %q", bare.Name)
	}

	list := loginStatesFrom([]model.PluginStatus{
		{Manifest: derived}, {Manifest: explicit},
	}, 7)
	if len(list) != 2 || list[0].PluginID != "officeace" || list[1].Source != model.LoginSourceManifest {
		t.Fatalf("loginStatesFrom = %+v", list)
	}
}

func TestCheckinRecording(t *testing.T) {
	app, st := openAppWithStore(t)

	before, after := 100.0, 120.0
	result := normalizeCheckinResult(model.CheckinResult{
		Result:        model.CheckinOK,
		CreditsBefore: &before,
		CreditsAfter:  &after,
		Message:       "+20",
	})
	if result.Result != model.CheckinOK {
		t.Fatalf("normalized = %+v", result)
	}
	if got := checkinMessage(result); got != "+20" {
		t.Fatalf("message = %q", got)
	}
	if got := checkinMessage(model.CheckinResult{Result: model.CheckinOK, CreditsBefore: &before, CreditsAfter: &after}); got != "+20.0" {
		t.Fatalf("credit delta message = %q", got)
	}

	app.recordCheckin("mock", "acc_1", "label-1", result)
	today := time.Now().Format("2006-01-02")
	records, err := st.QueryCheckinRecords("mock", today, today)
	if err != nil || len(records) != 1 {
		t.Fatalf("records = %+v, %v", records, err)
	}
	rec := records[0]
	if rec.Result != model.CheckinOK || rec.AccountLabel != "label-1" || rec.CreditsAfter == nil || *rec.CreditsAfter != 120 {
		t.Fatalf("record = %+v", rec)
	}
	if rec.Date != today {
		t.Fatalf("date = %q, want %q", rec.Date, today)
	}

	// The same day upserts in place.
	app.recordCheckin("mock", "acc_1", "label-2", model.CheckinResult{Result: model.CheckinAlready})
	records, _ = st.QueryCheckinRecords("mock", today, today)
	if len(records) != 1 || records[0].Result != model.CheckinAlready || records[0].AccountLabel != "label-2" {
		t.Fatalf("records after upsert = %+v", records)
	}

	// Unexpected plugin answers are folded into fail and recorded as such.
	app.recordCheckin("mock", "acc_2", "", model.CheckinResult{Result: "banana"})
	records, _ = st.QueryCheckinRecords("mock", today, today)
	if len(records) != 2 {
		t.Fatalf("records = %+v", records)
	}
	for _, row := range records {
		if row.AccountID == "acc_2" && (row.Result != model.CheckinFail || !strings.Contains(row.Message, "banana")) {
			t.Fatalf("normalized record = %+v", row)
		}
	}

	// Recording without a store is a no-op.
	(&App{}).recordCheckin("mock", "acc_3", "", model.CheckinResult{Result: model.CheckinOK})
}

func TestAccountSaveDraftValidation(t *testing.T) {
	app, st := openAppWithStore(t)

	applied, err := app.accountSave(model.AccountDraft{
		Groups: []model.GroupDraft{{Name: "Work"}},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if applied.Groups != 1 {
		t.Fatalf("applied = %+v", applied)
	}
	groups, err := st.ListAccountGroups()
	if err != nil || len(groups) != 1 {
		t.Fatalf("groups = %+v, %v", groups, err)
	}
	if !strings.HasPrefix(groups[0].ID, "grp_") {
		t.Fatalf("group id = %q, want a minted grp_ id", groups[0].ID)
	}

	// Duplicate names are rejected before the transaction and nothing applies.
	if _, err := app.accountSave(model.AccountDraft{
		Groups: []model.GroupDraft{{Name: "Dup"}, {Name: "Dup"}},
	}); !errors.Is(err, webui.ErrInvalidRequest) {
		t.Fatalf("duplicate names = %v, want ErrInvalidRequest", err)
	}
	if groups, _ := st.ListAccountGroups(); len(groups) != 1 {
		t.Fatalf("rejected draft wrote groups: %+v", groups)
	}

	// Dangling group references are rejected too.
	missing := "grp_missing"
	if _, err := app.accountSave(model.AccountDraft{
		Accounts: []model.AccountDraftItem{{
			PluginID: "mock", AccountID: "acc_1", GroupID: &missing,
		}},
	}); !errors.Is(err, webui.ErrInvalidRequest) {
		t.Fatalf("dangling group = %v, want ErrInvalidRequest", err)
	}
	if _, ok, _ := st.GetAccountMeta("mock", "acc_1"); ok {
		t.Fatal("rejected draft wrote account meta")
	}

	// A valid draft applies the overlay and audits one summary row.
	alias := "主号"
	groupID := groups[0].ID
	if _, err := app.accountSave(model.AccountDraft{
		Accounts: []model.AccountDraftItem{{
			PluginID: "mock", AccountID: "acc_1", Alias: &alias, GroupID: &groupID,
		}},
		Memberships: []model.MembershipDraft{{PluginID: "mock", AccountID: "acc_1", Member: true}},
	}); err != nil {
		t.Fatalf("save valid draft: %v", err)
	}
	meta, ok, _ := st.GetAccountMeta("mock", "acc_1")
	if !ok || meta.Alias != "主号" || meta.GroupID != groupID {
		t.Fatalf("meta = %+v", meta)
	}
	audit, err := st.ListAudit(model.AuditAccount, 10)
	if err != nil || len(audit) != 2 {
		t.Fatalf("audit = %+v, %v", audit, err)
	}
	if audit[0].Action != "save" || audit[0].Result != "ok" {
		t.Fatalf("audit[0] = %+v", audit[0])
	}
}

func TestAccountListingCarriesHealth(t *testing.T) {
	app, st := openAppWithStore(t)

	if err := st.SetAccountHealth(store.AccountHealth{
		PluginID: "mock", AccountID: "acc_1",
		StatusKind: model.StatusKindSessionDead, CooldownUntil: 9999999999,
		CooldownReason: "登录态已失效，请重新登录", FailureCount: 2,
	}); err != nil {
		t.Fatalf("SetAccountHealth: %v", err)
	}
	src := &fakeAccountSource{
		plugins: []model.PluginStatus{{Manifest: model.Manifest{ID: "mock"}, Enabled: true, Running: true}},
		accounts: map[string][]model.Account{"mock": {
			{ID: "acc_1", Label: "first", Enabled: true, Status: model.AccountOK},
			{ID: "acc_2", Label: "second", Enabled: true, Status: model.AccountOK},
		}},
		errs: map[string]error{},
	}

	entries := app.accountsFrom(src, "").(map[string]any)["accounts"].([]accountEntry)
	if len(entries) != 2 {
		t.Fatalf("entries = %+v", entries)
	}
	first := entries[0]
	if first.StatusKind != model.StatusKindSessionDead || first.CooldownUntil != 9999999999 ||
		first.CooldownReason != "登录态已失效，请重新登录" {
		t.Fatalf("health missing from the entry: %+v", first)
	}
	if second := entries[1]; second.StatusKind != model.StatusKindNormal || second.CooldownUntil != 0 || second.CooldownReason != "" {
		t.Fatalf("healthy account carries health: %+v", second)
	}

	// The JSON contract exposes the three fields for every account, including
	// the healthy one.
	raw, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)
	for _, want := range []string{
		`"status_kind":"session_dead"`,
		`"cooldown_until":9999999999`,
		`"cooldown_reason":"登录态已失效，请重新登录"`,
		`"status_kind":"normal"`,
		`"cooldown_until":0`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("payload missing %s: %s", want, body)
		}
	}
}
