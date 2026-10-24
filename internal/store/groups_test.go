package store

import (
	"encoding/json"
	"strings"
	"testing"

	"aiworkgate/internal/model"
)

func TestAccountGroupsAndUngrouping(t *testing.T) {
	s := openTestStore(t)

	if err := s.UpsertAccountGroup(model.AccountGroup{ID: "grp_work", Name: "工作", Color: "#fff", Sort: 1}); err != nil {
		t.Fatalf("create work: %v", err)
	}
	if err := s.UpsertAccountGroup(model.AccountGroup{ID: "grp_home", Name: "个人", Sort: 2}); err != nil {
		t.Fatalf("create home: %v", err)
	}
	// Renaming keeps the id.
	if err := s.UpsertAccountGroup(model.AccountGroup{ID: "grp_home", Name: "私人", Sort: 2}); err != nil {
		t.Fatalf("rename home: %v", err)
	}

	groups, err := s.ListAccountGroups()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(groups) != 2 || groups[0].ID != "grp_work" || groups[0].Name != "工作" || groups[1].Name != "私人" {
		t.Fatalf("groups = %+v", groups)
	}

	// Membership counts come from account_meta.
	if err := s.SetAccountsGroup([]model.AccountRef{
		{PluginID: "mock", AccountID: "acc_1"},
		{PluginID: "mock", AccountID: "acc_2"},
	}, "grp_work"); err != nil {
		t.Fatalf("set accounts group: %v", err)
	}
	group, ok, err := s.GetAccountGroup("grp_work")
	if err != nil || !ok || group.AccountCount != 2 {
		t.Fatalf("work = %+v, %v, %v", group, ok, err)
	}

	// Full replace moves accounts out of their previous group.
	if err := s.ReplaceGroupMembers("grp_home", []model.AccountRef{{PluginID: "mock", AccountID: "acc_2"}}); err != nil {
		t.Fatalf("replace members: %v", err)
	}
	meta, _, _ := s.GetAccountMeta("mock", "acc_2")
	if meta.GroupID != "grp_home" {
		t.Fatalf("acc_2 group = %q", meta.GroupID)
	}
	meta, _, _ = s.GetAccountMeta("mock", "acc_1")
	if meta.GroupID != "grp_work" {
		t.Fatalf("acc_1 group = %q", meta.GroupID)
	}

	// Replacing with an empty list only ungroups that group's members.
	if err := s.ReplaceGroupMembers("grp_home", nil); err != nil {
		t.Fatalf("clear members: %v", err)
	}
	meta, _, _ = s.GetAccountMeta("mock", "acc_2")
	if meta.GroupID != "" {
		t.Fatalf("acc_2 group = %q, want ungrouped", meta.GroupID)
	}

	// Deleting a group ungroups its members but keeps the accounts.
	if err := s.SetAccountGroup("mock", "acc_3", "grp_work"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAccountGroup("grp_work"); err != nil {
		t.Fatalf("delete group: %v", err)
	}
	if _, ok, _ := s.GetAccountGroup("grp_work"); ok {
		t.Fatal("group survived deletion")
	}
	for _, accountID := range []string{"acc_1", "acc_3"} {
		meta, ok, err := s.GetAccountMeta("mock", accountID)
		if err != nil || !ok {
			t.Fatalf("%s meta missing: %v %v", accountID, ok, err)
		}
		if meta.GroupID != "" {
			t.Fatalf("%s group = %q, want NULL", accountID, meta.GroupID)
		}
	}
	// Deleting an unknown group is a no-op.
	if err := s.DeleteAccountGroup("grp_missing"); err != nil {
		t.Fatalf("delete unknown group: %v", err)
	}

	// Duplicate names are rejected.
	if err := s.UpsertAccountGroup(model.AccountGroup{ID: "grp_x", Name: "私人"}); err == nil {
		t.Fatal("duplicate group name accepted")
	}
}

func TestSaveAccountDraft(t *testing.T) {
	s := openTestStore(t)

	if err := s.UpsertAccountGroup(model.AccountGroup{ID: "grp_existing", Name: "Existing"}); err != nil {
		t.Fatal(err)
	}

	alias := " 主号 "
	groupID := "grp_new"
	priority := 5
	weight := 2.5
	override := false
	draft := model.AccountDraft{
		Groups: []model.GroupDraft{
			{ID: "grp_new", Name: "New", Color: "#000", Sort: intPtr(3)},
			{ID: "grp_existing", Name: "Renamed", Sort: intPtr(1)},
		},
		Accounts: []model.AccountDraftItem{{
			PluginID: "mock", AccountID: "acc_1",
			Alias: &alias, GroupID: &groupID, Priority: &priority, Weight: &weight,
			EnabledOverride: &override,
			Egress:          &model.EgressDraft{Mode: model.EgressProxy, ProxyID: "px_1"},
		}},
		Memberships: []model.MembershipDraft{
			{PluginID: "mock", AccountID: "acc_1", Member: true},
			{PluginID: "mock", AccountID: "acc_2", Member: false},
		},
	}
	if err := s.UpsertPoolMember(model.PoolMember{PluginID: "mock", AccountID: "acc_2", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	applied, err := s.SaveAccountDraft(draft)
	if err != nil {
		t.Fatalf("save draft: %v", err)
	}
	if applied.Accounts != 1 || applied.Groups != 2 || applied.Memberships != 2 {
		t.Fatalf("applied = %+v", applied)
	}

	meta, ok, _ := s.GetAccountMeta("mock", "acc_1")
	if !ok {
		t.Fatal("account meta missing")
	}
	if meta.Alias != "主号" || meta.GroupID != "grp_new" {
		t.Fatalf("meta = %+v", meta)
	}
	if meta.Priority == nil || *meta.Priority != 5 || meta.Weight == nil || *meta.Weight != 2.5 {
		t.Fatalf("priority/weight = %+v", meta)
	}
	if meta.EnabledOverride == nil || *meta.EnabledOverride {
		t.Fatalf("enabled_override = %+v", meta.EnabledOverride)
	}
	egress, err := s.ListAccountEgress("mock")
	if err != nil || len(egress) != 1 || egress[0].Mode != model.EgressProxy || egress[0].ProxyID != "px_1" {
		t.Fatalf("egress = %+v, %v", egress, err)
	}
	members, err := s.ListPoolMembers("mock")
	if err != nil || len(members) != 1 || members[0].AccountID != "acc_1" {
		t.Fatalf("members = %+v, %v", members, err)
	}
	groups, _ := s.ListAccountGroups()
	if len(groups) != 2 || groups[0].Name != "Renamed" || groups[0].Sort != 1 {
		t.Fatalf("groups = %+v", groups)
	}

	// Deleting a group in the same draft frees its name for a new group and
	// ungroups its members.
	applied, err = s.SaveAccountDraft(model.AccountDraft{
		DeletedGroups: []string{"grp_existing"},
		Groups:        []model.GroupDraft{{ID: "grp_other", Name: "Renamed"}},
	})
	if err != nil || applied.Groups != 1 {
		t.Fatalf("save after delete = %+v, %v", applied, err)
	}
	if _, ok, _ := s.GetAccountGroup("grp_existing"); ok {
		t.Fatal("deleted group still present")
	}
}

func TestSaveAccountDraftRollback(t *testing.T) {
	s := openTestStore(t)

	if err := s.UpsertAccountMeta(model.AccountMeta{PluginID: "mock", AccountID: "acc_1", Alias: "keep"}); err != nil {
		t.Fatal(err)
	}
	alias := "changed"
	bad := model.AccountDraft{
		Groups: []model.GroupDraft{
			{ID: "grp_a", Name: "Same"},
			{ID: "grp_b", Name: "Same"},
		},
		Accounts: []model.AccountDraftItem{{
			PluginID: "mock", AccountID: "acc_1", Alias: &alias,
		}},
	}
	_, err := s.SaveAccountDraft(bad)
	if err == nil {
		t.Fatal("duplicate group name accepted")
	}
	if !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("error = %v, want a unique constraint failure", err)
	}
	meta, _, _ := s.GetAccountMeta("mock", "acc_1")
	if meta.Alias != "keep" {
		t.Fatalf("alias = %q, rollback did not run", meta.Alias)
	}
	if groups, _ := s.ListAccountGroups(); len(groups) != 0 {
		t.Fatalf("groups survived rollback: %+v", groups)
	}

	// An unknown group reference is rejected before anything is written.
	missing := "grp_missing"
	_, err = s.SaveAccountDraft(model.AccountDraft{
		Accounts: []model.AccountDraftItem{{
			PluginID: "mock", AccountID: "acc_1", GroupID: &missing, Alias: &alias,
		}},
	})
	if err == nil {
		t.Fatal("unknown group accepted")
	}
	meta, _, _ = s.GetAccountMeta("mock", "acc_1")
	if meta.Alias != "keep" {
		t.Fatalf("alias = %q after rejected draft", meta.Alias)
	}
	if _, ok, _ := s.GetAccountMeta("mock", "acc_2"); ok {
		t.Fatal("row leaked from rejected draft")
	}
}

func TestPoolTables(t *testing.T) {
	s := openTestStore(t)

	// Members.
	if err := s.UpsertPoolMember(model.PoolMember{PluginID: "mock", AccountID: "acc_2", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertPoolMember(model.PoolMember{PluginID: "mock", AccountID: "acc_1", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	members, err := s.ListPoolMembers("mock")
	if err != nil || len(members) != 2 {
		t.Fatalf("members = %+v, %v", members, err)
	}
	if members[0].AccountID != "acc_1" || members[0].Enabled || !members[1].Enabled {
		t.Fatalf("members = %+v", members)
	}
	if err := s.ReplacePoolMembers("mock", []model.AccountRef{{PluginID: "mock", AccountID: "acc_9"}}); err != nil {
		t.Fatal(err)
	}
	if members, _ := s.ListPoolMembers("mock"); len(members) != 1 || members[0].AccountID != "acc_9" {
		t.Fatalf("members after replace = %+v", members)
	}
	if err := s.DeletePoolMember("mock", "acc_9"); err != nil {
		t.Fatal(err)
	}
	if members, _ := s.ListPoolMembers("mock"); len(members) != 0 {
		t.Fatalf("members after delete = %+v", members)
	}

	// Settings.
	if _, ok, err := s.GetPoolSettings("mock"); err != nil || ok {
		t.Fatalf("unknown settings ok=%v err=%v", ok, err)
	}
	settings := model.PoolSettings{
		PluginID: "mock", UpstreamEnabled: true, Strategy: "credit_first", PoolPriority: 10,
		AccountConcurrency: 2, PoolStickyTTL: 600, ConvStickyTTL: 900, HedgeMS: 3000,
		DefaultReasoning: "low", BackgroundDowngrade: true, ToolExec: true, ModelSyncTime: "05:45",
	}
	if err := s.UpsertPoolSettings(settings); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetPoolSettings("mock")
	if err != nil || !ok || !got.UpstreamEnabled || got.Strategy != "credit_first" ||
		got.PoolPriority != 10 || got.AccountConcurrency != 2 || got.PoolStickyTTL != 600 ||
		got.ConvStickyTTL != 900 || got.HedgeMS != 3000 || got.DefaultReasoning != "low" ||
		!got.BackgroundDowngrade || !got.ToolExec || got.LongctxDowngrade || got.ModelSyncTime != "05:45" {
		t.Fatalf("settings = %+v, %v, %v", got, ok, err)
	}
	if list, err := s.ListPoolSettings(); err != nil || len(list) != 1 {
		t.Fatalf("list settings = %+v, %v", list, err)
	}

	// Model bindings.
	binding := model.ModelBinding{
		LogicalModel: "deepseek-v4-pro",
		DisplayName:  "DeepSeek V4",
		Items: []model.ModelBindingItem{
			{SourcePlugin: "codearts", SourceModel: "deepseek-v4-pro"},
			{SourcePlugin: "officeace", SourceModel: "glm-5.3", Position: 1},
		},
	}
	if err := s.ReplaceModelBinding(binding); err != nil {
		t.Fatal(err)
	}
	gotBinding, ok, err := s.GetModelBinding("deepseek-v4-pro")
	if err != nil || !ok || len(gotBinding.Items) != 2 {
		t.Fatalf("binding = %+v, %v, %v", gotBinding, ok, err)
	}
	if gotBinding.Items[0].SourcePlugin != "codearts" || gotBinding.Items[1].Position != 1 {
		t.Fatalf("binding items = %+v", gotBinding.Items)
	}
	if err := s.ReplaceModelBinding(model.ModelBinding{
		LogicalModel: "deepseek-v4-pro",
		Items:        []model.ModelBindingItem{{SourcePlugin: "mock", SourceModel: "mock-chat"}},
	}); err != nil {
		t.Fatal(err)
	}
	if gotBinding, _, _ := s.GetModelBinding("deepseek-v4-pro"); len(gotBinding.Items) != 1 || gotBinding.Items[0].SourcePlugin != "mock" {
		t.Fatalf("binding after replace = %+v", gotBinding)
	}
	if list, err := s.ListModelBindings(); err != nil || len(list) != 1 {
		t.Fatalf("bindings = %+v, %v", list, err)
	}
	if err := s.DeleteModelBinding("deepseek-v4-pro"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.GetModelBinding("deepseek-v4-pro"); ok {
		t.Fatal("binding survived deletion")
	}
	if n, _ := s.countRows("model_binding_items"); n != 0 {
		t.Fatalf("binding items survived deletion: %d", n)
	}

	// Model catalog.
	catalog := []model.ModelCatalogEntry{
		{ModelID: "glm-5.3", DisplayName: "GLM 5.3", Vendor: "Zhipu", Multiplier: 2, Thinking: []string{"off", "on"}, ContextLength: 202752, Vision: true},
		{ModelID: "free-model", Free: true},
	}
	if err := s.ReplaceModelCatalog("codearts", catalog); err != nil {
		t.Fatal(err)
	}
	entries, err := s.ListModelCatalog("codearts")
	if err != nil || len(entries) != 2 {
		t.Fatalf("catalog = %+v, %v", entries, err)
	}
	if entries[0].ModelID != "free-model" || !entries[0].Free {
		t.Fatalf("catalog[0] = %+v", entries[0])
	}
	if entries[1].Multiplier != 2 || !entries[1].Vision || len(entries[1].Thinking) != 2 || entries[1].ContextLength != 202752 {
		t.Fatalf("catalog[1] = %+v", entries[1])
	}
	if err := s.ReplaceModelCatalog("codearts", catalog[:1]); err != nil {
		t.Fatal(err)
	}
	if entries, _ := s.ListModelCatalog("codearts"); len(entries) != 1 {
		t.Fatalf("catalog after replace = %+v", entries)
	}
	if err := s.DeleteModelCatalog("codearts"); err != nil {
		t.Fatal(err)
	}
	if entries, _ := s.ListModelCatalog("codearts"); len(entries) != 0 {
		t.Fatalf("catalog after delete = %+v", entries)
	}
}

func TestStickyUsageAndAudit(t *testing.T) {
	s := openTestStore(t)

	// Sticky bindings.
	if err := s.UpsertStickyBinding(model.StickyBinding{Scope: model.StickyScopeConversation, Key: "c1", PluginID: "mock", AccountID: "acc_1", ExpiresAt: 200}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertStickyBinding(model.StickyBinding{Scope: model.StickyScopePool, Key: "p1", PluginID: "mock", AccountID: "acc_2", ExpiresAt: 100}); err != nil {
		t.Fatal(err)
	}
	// Upsert on the same (scope, key) replaces the target.
	if err := s.UpsertStickyBinding(model.StickyBinding{Scope: model.StickyScopeConversation, Key: "c1", PluginID: "mock", AccountID: "acc_3", ExpiresAt: 300}); err != nil {
		t.Fatal(err)
	}
	all, err := s.ListStickyBindings("")
	if err != nil || len(all) != 2 {
		t.Fatalf("sticky = %+v, %v", all, err)
	}
	if all[0].Key != "c1" || all[0].AccountID != "acc_3" {
		t.Fatalf("sticky[0] = %+v", all[0])
	}
	if conv, _ := s.ListStickyBindings(model.StickyScopeConversation); len(conv) != 1 {
		t.Fatalf("conv sticky = %+v", conv)
	}
	removed, err := s.DeleteExpiredStickyBindings(150)
	if err != nil || removed != 1 {
		t.Fatalf("expired removed = %d, %v", removed, err)
	}
	if err := s.DeleteStickyForAccount("mock", "acc_3"); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.ListStickyBindings(""); len(all) != 0 {
		t.Fatalf("sticky survived = %+v", all)
	}
	if err := s.DeleteStickyBinding(model.StickyScopePool, "missing"); err != nil {
		t.Fatal(err)
	}

	// Usage rollup.
	row := model.UsageDailyRow{
		Day: "2026-10-20", PluginID: "mock", AccountID: "acc_1", Model: "mock-chat",
		Requests: 3, Errors: 1, PromptTokens: 100, CompletionTokens: 50,
		Credits: 1.5, LatencyMSSum: 900, TTFBMSSum: 200, TTFBCount: 3,
	}
	if err := s.UpsertUsageDaily(row); err != nil {
		t.Fatal(err)
	}
	row.Requests = 4
	if err := s.UpsertUsageDaily(row); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListUsageDaily("2026-10-20", "2026-10-20", "mock")
	if err != nil || len(rows) != 1 {
		t.Fatalf("usage = %+v, %v", rows, err)
	}
	if rows[0].Requests != 4 || rows[0].Credits != 1.5 || rows[0].TTFBCount != 3 {
		t.Fatalf("usage row = %+v", rows[0])
	}
	if rows, _ := s.ListUsageDaily("2026-10-21", "", ""); len(rows) != 0 {
		t.Fatalf("out-of-range usage = %+v", rows)
	}
	if err := s.UpsertUsageDaily(model.UsageDailyRow{Day: "", PluginID: "mock", Model: "m"}); err == nil {
		t.Fatal("usage row without day accepted")
	}
	if removed, err := s.DeleteUsageDailyBefore("2026-10-21"); err != nil || removed != 1 {
		t.Fatalf("usage cleanup = %d, %v", removed, err)
	}

	// Audit trail.
	id, err := s.AppendAudit(model.AuditEntry{Category: model.AuditAccount, Action: "rename", Target: "mock/acc_1", Result: "ok"})
	if err != nil || id == 0 {
		t.Fatalf("append audit = %d, %v", id, err)
	}
	if _, err := s.AppendAudit(model.AuditEntry{Category: model.AuditGroup, Action: "delete", Target: "grp_1"}); err != nil {
		t.Fatal(err)
	}
	entries, err := s.ListAudit("", 10)
	if err != nil || len(entries) != 2 {
		t.Fatalf("audit = %+v, %v", entries, err)
	}
	if entries[0].Action != "delete" || entries[0].TS == "" {
		t.Fatalf("audit[0] = %+v", entries[0])
	}
	onlyAccounts, err := s.ListAudit(model.AuditAccount, 10)
	if err != nil || len(onlyAccounts) != 1 || onlyAccounts[0].Action != "rename" {
		t.Fatalf("filtered audit = %+v, %v", onlyAccounts, err)
	}
	if entries, _ := s.ListAudit("", 1); len(entries) != 1 {
		t.Fatalf("limited audit = %+v", entries)
	}
}

func TestAPILogAndQuotaNewColumns(t *testing.T) {
	s := openTestStore(t)

	credits := 0.0
	rec := model.APIRequestLog{
		TS: "2026-10-20T10:00:00+08:00", ReqID: "r1", Source: "plugin:mock",
		Model: "mock/mock-chat", LogicalModel: "chat", Account: "acc_1",
		ConversationID: "conv-1", Status: 200, FirstTokenAt: "2026-10-20T10:00:01+08:00",
		Credits: &credits,
	}
	if err := s.InsertAPIRequestLog(rec); err != nil {
		t.Fatalf("insert: %v", err)
	}
	logs, _, err := s.QueryAPIRequestLogs(Filter{})
	if err != nil || len(logs) != 1 {
		t.Fatalf("logs = %+v, %v", logs, err)
	}
	got := logs[0]
	if got.LogicalModel != "chat" || got.ConversationID != "conv-1" || got.FirstTokenAt == "" {
		t.Fatalf("new columns = %+v", got)
	}
	if got.Credits == nil || *got.Credits != 0 {
		t.Fatalf("credits = %v, want explicit 0 (not NULL)", got.Credits)
	}

	// Without a credits value the column stays NULL.
	if err := s.InsertAPIRequestLog(model.APIRequestLog{TS: "2026-10-20T11:00:00+08:00", ReqID: "r2", Source: "a"}); err != nil {
		t.Fatal(err)
	}
	logs, _, _ = s.QueryAPIRequestLogs(Filter{})
	for _, row := range logs {
		if row.ReqID == "r2" && row.Credits != nil {
			t.Fatalf("credits = %v, want NULL", *row.Credits)
		}
	}

	if err := s.SaveQuotaSnapshot(model.QuotaSnapshot{
		PluginID: "mock", AccountID: "acc_1", Remaining: 5, Total: 10, ExpiresAt: 1795000000, Plan: "Pro", At: 10,
	}); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	snaps, err := s.QueryQuotaSnapshots("mock", "acc_1", 0)
	if err != nil || len(snaps) != 1 {
		t.Fatalf("snapshots = %+v, %v", snaps, err)
	}
	if snaps[0].ExpiresAt != 1795000000 || snaps[0].Plan != "Pro" {
		t.Fatalf("snapshot = %+v", snaps[0])
	}
}

func TestSaveAccountDraftPartialUpdate(t *testing.T) {
	s := openTestStore(t)

	alias := "keep-me"
	priority := 7
	if err := s.UpsertAccountMeta(model.AccountMeta{
		PluginID: "mock", AccountID: "acc_1", Alias: alias, Priority: &priority,
	}); err != nil {
		t.Fatal(err)
	}

	// A draft that only carries group_id leaves alias and priority alone.
	draft := model.AccountDraft{}
	if err := json.Unmarshal([]byte(`{"accounts":[{"plugin_id":"mock","account_id":"acc_1","group_id":null}]}`), &draft); err != nil {
		t.Fatalf("decode draft: %v", err)
	}
	if !draft.Accounts[0].GroupIDSet() || draft.Accounts[0].AliasSet() {
		t.Fatalf("presence flags = %+v", draft.Accounts[0])
	}
	if _, err := s.SaveAccountDraft(draft); err != nil {
		t.Fatalf("save: %v", err)
	}
	meta, _, _ := s.GetAccountMeta("mock", "acc_1")
	if meta.Alias != "keep-me" || meta.Priority == nil || *meta.Priority != 7 {
		t.Fatalf("partial draft overwrote fields: %+v", meta)
	}

	// An explicit null alias clears it while an absent weight stays NULL.
	draft = model.AccountDraft{}
	if err := json.Unmarshal([]byte(`{"accounts":[{"plugin_id":"mock","account_id":"acc_1","alias":null,"weight":2}]}`), &draft); err != nil {
		t.Fatalf("decode draft: %v", err)
	}
	if !draft.Accounts[0].AliasSet() || draft.Accounts[0].Alias != nil || !draft.Accounts[0].WeightSet() {
		t.Fatalf("null alias flags = %+v", draft.Accounts[0])
	}
	if _, err := s.SaveAccountDraft(draft); err != nil {
		t.Fatalf("save null alias: %v", err)
	}
	meta, _, _ = s.GetAccountMeta("mock", "acc_1")
	if meta.Alias != "" {
		t.Fatalf("alias = %q, want cleared", meta.Alias)
	}
	if meta.Weight == nil || *meta.Weight != 2 {
		t.Fatalf("weight = %v, want 2", meta.Weight)
	}
	if meta.Priority == nil || *meta.Priority != 7 {
		t.Fatalf("priority = %v, want preserved", meta.Priority)
	}
}
