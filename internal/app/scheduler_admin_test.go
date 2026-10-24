package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"aiworkgate/internal/model"
	"aiworkgate/internal/scheduler"
	"aiworkgate/internal/store"
	"aiworkgate/internal/webui"
)

// fakeSchedulerSource is a schedulerSource test double; no plugin runs. Reads
// arrive from the snapshot round concurrently, so every field is guarded by mu.
type fakeSchedulerSource struct {
	mu       sync.Mutex
	plugins  []model.PluginStatus
	accounts map[string][]model.Account
	errs     map[string]error
	models   map[string][]model.ModelInfo
	calls    map[string]int
}

func newFakeSchedulerSource() *fakeSchedulerSource {
	return &fakeSchedulerSource{
		accounts: map[string][]model.Account{},
		errs:     map[string]error{},
		models:   map[string][]model.ModelInfo{},
		calls:    map[string]int{},
	}
}

func (f *fakeSchedulerSource) List() []model.PluginStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]model.PluginStatus(nil), f.plugins...)
}

func (f *fakeSchedulerSource) Accounts(ctx context.Context, pluginID string) ([]model.Account, error) {
	f.mu.Lock()
	f.calls["accounts:"+pluginID]++
	err := f.errs[pluginID]
	accounts := append([]model.Account(nil), f.accounts[pluginID]...)
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return accounts, nil
}

func (f *fakeSchedulerSource) Models(ctx context.Context, pluginID string) ([]model.ModelInfo, error) {
	f.mu.Lock()
	f.calls["models:"+pluginID]++
	models := append([]model.ModelInfo(nil), f.models[pluginID]...)
	f.mu.Unlock()
	return models, nil
}

func (f *fakeSchedulerSource) callCount(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[key]
}

func newSchedulerApp(t *testing.T) (*App, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	app := &App{store: st, dispatch: scheduler.New(scheduler.Options{Sticky: stickyStoreAdapter{store: st}})}
	return app, st
}

func TestSchedulerSnapshotFromSettingsAndCache(t *testing.T) {
	app, st := newSchedulerApp(t)

	if err := st.UpsertPoolSettings(model.PoolSettings{
		PluginID: model.GlobalPoolID, UpstreamEnabled: true,
		Strategy: scheduler.StrategyRemainingFirst, AccountConcurrency: 2,
		PoolStickyTTL: 600, ConvStickyTTL: 300, HedgeMS: 3000,
		DefaultReasoning: "medium", BackgroundDowngrade: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertPoolSettings(model.PoolSettings{
		PluginID: "mock", UpstreamEnabled: true, Strategy: scheduler.StrategyP2C,
		PoolPriority: 7, AccountConcurrency: 1,
		PoolStickyTTL: 120, ConvStickyTTL: 60,
	}); err != nil {
		t.Fatal(err)
	}
	priority, weight := 5, 3.0
	if err := st.UpsertAccountMeta(model.AccountMeta{
		PluginID: "mock", AccountID: "acc_1", Alias: "主号",
		Priority: &priority, Weight: &weight,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplacePoolMembers("mock", []model.AccountRef{{AccountID: "acc_1"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceModelBinding(model.ModelBinding{
		LogicalModel: "deepseek", DisplayName: "DeepSeek",
		Items: []model.ModelBindingItem{
			{Position: 0, SourcePlugin: "mock", SourceModel: "glm"},
			{Position: 1, SourcePlugin: "other", SourceModel: "x"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceModelCatalog("mock", []model.ModelCatalogEntry{
		{ModelID: "glm", DisplayName: "GLM", Vendor: "Zhipu", Multiplier: 2.5, ContextLength: 200_000},
		{ModelID: "flash", DisplayName: "FLASH", Multiplier: 0.5, Free: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetKV(schedulerInterPoolKey, scheduler.InterPoolFixed); err != nil {
		t.Fatal(err)
	}
	// Offline plugin snapshot for the fallback path.
	if err := st.ReplaceAccountCache("other", []model.Account{{
		ID: "acc_cached", Label: "cached", Enabled: true,
		Credential: &model.Credential{Present: true},
		Quota:      model.Quota{Remaining: 42, Total: 100},
	}}); err != nil {
		t.Fatal(err)
	}

	src := newFakeSchedulerSource()
	src.plugins = []model.PluginStatus{
		{
			Manifest: model.Manifest{ID: "mock", Name: "Mock", APILevel: 2},
			Enabled:  true, Running: true,
		},
		{
			Manifest: model.Manifest{
				ID: "other", Name: "Other", APILevel: 2,
				Models: []model.ModelInfo{{ID: "x", Name: "X", ContextLength: 4096}},
			},
			Enabled: true, Running: true,
		},
		{
			Manifest: model.Manifest{ID: "off", Name: "Off", APILevel: 1},
			Enabled:  false, Running: false,
		},
		{
			Manifest: model.Manifest{ID: "stopped", Name: "Stopped", APILevel: 2},
			Enabled:  true, Running: false,
		},
	}
	src.accounts["mock"] = []model.Account{
		{
			ID: "acc_1", Label: "user@example.com", Enabled: true, Status: model.AccountOK,
			Credential:    &model.Credential{Present: true},
			Quota:         model.Quota{Remaining: 100, Total: 200, ExpiresAt: 1_800_000_000, Plan: "Pro"},
			CooldownUntil: 123,
		},
		{ID: "acc_2", Enabled: true, Credential: &model.Credential{Present: false}},
	}
	src.errs["other"] = errors.New("plugin is not running")
	src.models["mock"] = []model.ModelInfo{
		{ID: "glm", Name: "GLM", ContextLength: 1000, Vision: true},
	}

	snap, err := app.snapshotFrom(context.Background(), src)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.InterPool != scheduler.InterPoolFixed {
		t.Fatalf("inter pool = %q", snap.InterPool)
	}
	if len(snap.Pools) != 4 {
		t.Fatalf("pools = %d, want 4", len(snap.Pools))
	}
	mock := snap.Pools["mock"]
	if mock == nil || mock.Name != "Mock" || mock.APIVersion != 2 {
		t.Fatalf("mock pool = %+v", mock)
	}
	if mock.Strategy != scheduler.StrategyP2C || mock.PoolPriority != 7 ||
		mock.AccountConcurrency != 1 || mock.PoolStickyTTL != 120 || mock.ConvStickyTTL != 60 {
		t.Fatalf("mock settings = %+v", mock)
	}
	// Unset per-pool fields inherit the global defaults.
	if mock.DefaultReasoning != "medium" {
		t.Fatalf("reasoning = %q", mock.DefaultReasoning)
	}
	if !mock.UpstreamEnabled {
		t.Fatal("mock upstream disabled")
	}
	if len(mock.Accounts) != 2 {
		t.Fatalf("accounts = %+v", mock.Accounts)
	}
	first := mock.Accounts[0]
	if first.Priority != 5 || first.Weight != 3 || !first.Enabled || !first.CredentialPresent {
		t.Fatalf("overlay lost: %+v", first)
	}
	if first.Remaining != 100 || first.Total != 200 || first.Plan != "Pro" || first.CooldownUntil != 123 {
		t.Fatalf("quota lost: %+v", first)
	}
	if !mock.Members["acc_1"] || mock.Members["acc_2"] {
		t.Fatalf("members = %+v", mock.Members)
	}
	if mock.Models["glm"].Multiplier != 2.5 || !mock.Models["glm"].Vision ||
		mock.Models["glm"].ContextLength != 1000 {
		t.Fatalf("glm catalog merge = %+v", mock.Models["glm"])
	}
	if !mock.Models["flash"].Free || mock.Models["flash"].Multiplier != 0.5 {
		t.Fatalf("flash catalog = %+v", mock.Models["flash"])
	}

	other := snap.Pools["other"]
	if other.Strategy != scheduler.StrategyRemainingFirst || other.AccountConcurrency != 2 ||
		other.PoolStickyTTL != 600 || other.ConvStickyTTL != 300 || other.HedgeMS != 3000 ||
		other.PoolPriority != 100 {
		t.Fatalf("global defaults = %+v", other)
	}
	if len(other.Accounts) != 1 || other.Accounts[0].ID != "acc_cached" {
		t.Fatalf("cache fallback = %+v", other.Accounts)
	}
	if other.Models["x"].ContextLength != 4096 {
		t.Fatalf("manifest models = %+v", other.Models)
	}
	if snap.Pools["off"].UpstreamEnabled {
		t.Fatal("disabled plugin pool has upstream enabled")
	}
	if len(snap.Pools["stopped"].Accounts) != 0 {
		t.Fatalf("stopped plugin accounts = %+v", snap.Pools["stopped"].Accounts)
	}
	if src.callCount("accounts:stopped") != 0 {
		t.Fatal("stopped plugin was called for accounts")
	}
	binding := snap.Bindings["deepseek"]
	if binding.Logical != "deepseek" || len(binding.Items) != 2 ||
		binding.Items[1].PluginID != "other" || binding.Items[1].Model != "x" {
		t.Fatalf("binding = %+v", binding)
	}

	// A second snapshot inside the TTLs reuses the account/model caches.
	beforeAccounts := src.callCount("accounts:mock")
	beforeModels := src.callCount("models:mock")
	if _, err := app.snapshotFrom(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	if src.callCount("accounts:mock") != beforeAccounts {
		t.Fatalf("accounts cache miss: %d -> %d", beforeAccounts, src.callCount("accounts:mock"))
	}
	if src.callCount("models:mock") != beforeModels {
		t.Fatalf("models cache miss: %d -> %d", beforeModels, src.callCount("models:mock"))
	}
}

func TestSchedulerPresetGlobalAndVersion(t *testing.T) {
	app, st := newSchedulerApp(t)

	payload, err := app.applySchedulerPreset("balanced")
	if err != nil {
		t.Fatalf("preset balanced: %v", err)
	}
	overview, ok := payload.(map[string]any)
	if !ok {
		t.Fatalf("payload = %T", payload)
	}
	globalPayload := overview["global"].(map[string]any)
	if globalPayload["inter_pool"] != scheduler.InterPoolSmart {
		t.Fatalf("inter_pool = %v", globalPayload["inter_pool"])
	}
	global, found, err := st.GetPoolSettings(model.GlobalPoolID)
	if err != nil || !found {
		t.Fatalf("global row = %+v, %v, %v", global, found, err)
	}
	if global.Strategy != scheduler.StrategyCreditFirst || global.AccountConcurrency != 2 ||
		global.PoolStickyTTL != 600 || global.ConvStickyTTL != 600 || global.HedgeMS != 0 {
		t.Fatalf("balanced row = %+v", global)
	}
	if version := app.schedulerVersion(); version != 1 {
		t.Fatalf("version = %d, want 1", version)
	}
	if overview["version"] != 1 {
		t.Fatalf("overview version = %v", overview["version"])
	}

	if _, err := app.applySchedulerPreset("latency"); err != nil {
		t.Fatal(err)
	}
	global, _, _ = st.GetPoolSettings(model.GlobalPoolID)
	if global.Strategy != scheduler.StrategyWeighted3 || global.HedgeMS != 3000 {
		t.Fatalf("latency row = %+v", global)
	}

	if _, err := app.applySchedulerPreset("nope"); !errors.Is(err, webui.ErrInvalidRequest) {
		t.Fatalf("unknown preset = %v", err)
	}

	// Global update writes inter-pool and defaults and bumps the version.
	mode := scheduler.InterPoolFixed
	strategy := scheduler.StrategyFixed
	ttl := 900
	update := webui.SchedulerGlobalUpdate{
		InterPool: &mode,
		Defaults: &webui.SchedulerDefaultsPatch{
			Strategy:      &strategy,
			PoolStickyTTL: &ttl,
		},
	}
	if _, err := app.saveSchedulerGlobal(update); err != nil {
		t.Fatalf("save global: %v", err)
	}
	if app.schedulerInterPool() != scheduler.InterPoolFixed {
		t.Fatalf("inter pool = %q", app.schedulerInterPool())
	}
	global, _, _ = st.GetPoolSettings(model.GlobalPoolID)
	if global.Strategy != scheduler.StrategyFixed || global.PoolStickyTTL != 900 {
		t.Fatalf("global after update = %+v", global)
	}
	if app.schedulerVersion() != 3 {
		t.Fatalf("version = %d, want 3", app.schedulerVersion())
	}

	// Invalid values passed directly to the app are rejected before writing.
	bad := 99999
	if _, err := app.saveSchedulerGlobal(webui.SchedulerGlobalUpdate{
		Defaults: &webui.SchedulerDefaultsPatch{PoolStickyTTL: &bad},
	}); !errors.Is(err, webui.ErrInvalidRequest) {
		t.Fatalf("bad ttl = %v", err)
	}

	audit, err := st.ListAudit(model.AuditScheduler, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) < 2 {
		t.Fatalf("audit = %+v", audit)
	}
}

func TestSchedulerStickyAdmin(t *testing.T) {
	app, st := newSchedulerApp(t)
	now := time.Now().Unix()
	for _, row := range []model.StickyBinding{
		{Scope: model.StickyScopeConversation, Key: "c1", PluginID: "mock", AccountID: "a", ExpiresAt: now + 60},
		{Scope: model.StickyScopeConversation, Key: "c2", PluginID: "mock", AccountID: "b", ExpiresAt: now + 60},
		{Scope: model.StickyScopePool, Key: "p1", PluginID: "mock", AccountID: "a", ExpiresAt: now + 60},
	} {
		if err := st.UpsertStickyBinding(row); err != nil {
			t.Fatal(err)
		}
	}

	payload, err := app.schedulerSticky()
	if err != nil || payload.(map[string]any)["count"] != 3 {
		t.Fatalf("sticky = %+v, %v", payload, err)
	}

	removed, err := app.deleteSchedulerSticky(model.StickyScopeConversation, "c1")
	if err != nil || removed != 1 {
		t.Fatalf("delete one = %d, %v", removed, err)
	}
	rows, _ := st.ListStickyBindings(model.StickyScopeConversation)
	if len(rows) != 1 || rows[0].Key != "c2" {
		t.Fatalf("remaining conv = %+v", rows)
	}

	removed, err = app.deleteSchedulerSticky(model.StickyScopeConversation, "")
	if err != nil || removed != 1 {
		t.Fatalf("delete scope = %d, %v", removed, err)
	}
	removed, err = app.deleteSchedulerSticky("", "")
	if err != nil || removed != 1 {
		t.Fatalf("delete all = %d, %v", removed, err)
	}
	rows, _ = st.ListStickyBindings("")
	if len(rows) != 0 {
		t.Fatalf("sticky rows = %+v", rows)
	}
	audit, _ := st.ListAudit(model.AuditScheduler, 10)
	if len(audit) != 3 {
		t.Fatalf("audit = %+v", audit)
	}
}

func TestSchedulerOverviewWithoutPlugins(t *testing.T) {
	app, _ := newSchedulerApp(t)
	payload, err := app.schedulerOverview()
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	out := payload.(map[string]any)
	if out["version"] != 0 {
		t.Fatalf("version = %v", out["version"])
	}
	if pools, ok := out["pools"].([]map[string]any); !ok || len(pools) != 0 {
		t.Fatalf("pools = %#v", out["pools"])
	}
	global := out["global"].(map[string]any)
	if global["inter_pool"] != scheduler.InterPoolSmart {
		t.Fatalf("inter_pool = %v", global["inter_pool"])
	}
	defaults := global["defaults"].(map[string]any)
	if defaults["strategy"] != scheduler.StrategyCreditFirst {
		t.Fatalf("defaults = %+v", defaults)
	}
}

func TestApplySchedulerPoolUpdateModelSyncTime(t *testing.T) {
	settings := model.PoolSettings{ModelSyncTime: "05:45"}
	next := " 07:30 "
	applySchedulerPoolUpdate(&settings, webui.SchedulerPoolUpdate{ModelSyncTime: &next})
	if settings.ModelSyncTime != "07:30" {
		t.Fatalf("model_sync_time = %q, want 07:30", settings.ModelSyncTime)
	}
	empty := ""
	applySchedulerPoolUpdate(&settings, webui.SchedulerPoolUpdate{ModelSyncTime: &empty})
	if settings.ModelSyncTime != "" {
		t.Fatalf("model_sync_time = %q, want empty (auto sync off)", settings.ModelSyncTime)
	}
}
