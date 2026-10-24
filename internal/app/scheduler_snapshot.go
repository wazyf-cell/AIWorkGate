package app

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"aiworkgate/internal/model"
	"aiworkgate/internal/scheduler"
	"aiworkgate/internal/store"
)

// Snapshot provider (§6, §8.3): the gateway reads one immutable scheduling
// view per request. Plugin account lists are cached for 10s and model lists for
// 30s; settings, membership, bindings and overrides come straight from SQLite so
// a saved strategy applies to the next request.
//
// Budget model (2026-10-22, "scheduling snapshot cannot read accounts" burst): a
// cache miss fans the plugin reads out in parallel, and each read carries its
// OWN deadline (schedulerAccountTimeout / schedulerModelTimeout) detached from
// the caller's. The caller's deadline only bounds how long the round *waits*;
// when it expires the round serves the last known accounts instead of reading
// the plugin. This replaced a sequential walk under one shared deadline, which
// made every plugin behind a slow one report "context deadline exceeded" for a
// timeout it never hit — see launchParallelPluginReads for the evidence.

const (
	schedulerAccountTTL     = 10 * time.Second
	schedulerModelTTL       = 30 * time.Second
	schedulerAccountTimeout = 5 * time.Second
	schedulerModelTimeout   = 3 * time.Second

	schedulerInterPoolKey = "scheduler_inter_pool"
	schedulerVersionKey   = "scheduler_version"

	// Baselines for a pool that has never been configured. AccountConcurrency is
	// deliberately absent: 0 (unlimited) is the safe default, because imposing a
	// limit would throttle pools that work today. The three below are pure
	// upside and match what the UI documents.
	//
	// defaultHedgeMS stays inside dispatch.go's accepted band (>=1000) and the
	// documented 1s–8s range enforced by webui/scheduler.go.
	defaultPoolStickyTTL = 300
	defaultConvStickyTTL = 1800
	defaultHedgeMS       = 8000
)

// cachedAccountSet is one plugin's cached account answer.
type cachedAccountSet struct {
	accounts []model.Account
	at       time.Time
}

// cachedModelSet is one plugin's cached /v1/models answer.
type cachedModelSet struct {
	models []model.ModelInfo
	at     time.Time
}

// schedulerSnapshot builds the current scheduling view from the live plugin
// manager and the store.
func (a *App) schedulerSnapshot(ctx context.Context) (*scheduler.Snapshot, error) {
	var src schedulerSource
	if a.pluginMgr != nil {
		src = a.pluginMgr
	}
	return a.snapshotFrom(ctx, src)
}

// snapshotFrom is the testable form of schedulerSnapshot: any plugin source
// (the real manager or a fake) can be supplied.
func (a *App) snapshotFrom(ctx context.Context, src schedulerSource) (*scheduler.Snapshot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	snap := &scheduler.Snapshot{
		Pools:     map[string]*scheduler.Pool{},
		Bindings:  map[string]scheduler.Binding{},
		InterPool: a.schedulerInterPool(),
	}

	global, globalOK := a.globalPoolSettings()
	defaults := poolDefaults(global, globalOK)
	settings := map[string]model.PoolSettings{}
	if a.store != nil {
		rows, err := a.store.ListPoolSettings()
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			settings[row.PluginID] = row
		}
		bindings, err := a.store.ListModelBindings()
		if err != nil {
			return nil, err
		}
		for _, binding := range bindings {
			items := make([]scheduler.Source, 0, len(binding.Items))
			for _, item := range binding.Items {
				items = append(items, scheduler.Source{
					PluginID: strings.TrimSpace(item.SourcePlugin),
					Model:    strings.TrimSpace(item.SourceModel),
				})
			}
			if len(items) == 0 {
				continue
			}
			snap.Bindings[binding.LogicalModel] = scheduler.Binding{
				Logical: binding.LogicalModel,
				Display: binding.DisplayName,
				Items:   items,
			}
		}
	}

	meta := a.accountMetaMap()
	if src == nil {
		return snap, nil
	}
	statuses := src.List()
	reads := a.launchParallelPluginReads(ctx, src, statuses)

	for i, st := range statuses {
		id := strings.TrimSpace(st.Manifest.ID)
		if id == "" {
			continue
		}
		row, rowOK := settings[id]
		poolSettings := effectivePoolSettings(defaults, row, rowOK)
		accounts := a.roundAccounts(ctx, id, reads.accounts[i])
		poolAccounts := make([]scheduler.Account, 0, len(accounts))
		for _, acc := range accounts {
			overlay, hasOverlay := meta[accountKey(id, acc.ID)]
			poolAccounts = append(poolAccounts, a.withEgressEvidence(id, schedulerAccount(acc, overlay, hasOverlay)))
		}
		snap.Pools[id] = &scheduler.Pool{
			PluginID:             id,
			Name:                 strings.TrimSpace(st.Manifest.Name),
			APIVersion:           st.Manifest.APILevel,
			UpstreamEnabled:      poolSettings.UpstreamEnabled && st.Enabled,
			Strategy:             poolSettings.Strategy,
			PoolPriority:         poolSettings.PoolPriority,
			AccountConcurrency:   poolSettings.AccountConcurrency,
			PoolStickyTTL:        poolSettings.PoolStickyTTL,
			ConvStickyTTL:        poolSettings.ConvStickyTTL,
			HedgeMS:              poolSettings.HedgeMS,
			DefaultReasoning:     poolSettings.DefaultReasoning,
			BackgroundDowngrade:  poolSettings.BackgroundDowngrade,
			LongContextDowngrade: poolSettings.LongctxDowngrade,
			Accounts:             poolAccounts,
			Models:               a.roundModels(ctx, id, st, reads.models[i]),
			Members:              a.poolMemberSet(id),
		}
	}
	return snap, nil
}

// ---------------------------------------------------------------- fan-out

// pluginRead is one in-flight plugin read, shared by every snapshot round that
// needs the same answer. The fetching goroutine writes the payload fields before
// closing done, so a reader that observed <-done needs no lock; a reader that
// gave up (its own budget ran out) must never touch them.
type pluginRead struct {
	done     chan struct{}
	accounts []model.Account
	models   []model.ModelInfo
	err      error
}

// pluginReads indexes the reads launched by one snapshot round by the position
// of the plugin in the status list. A round never waits for a plugin it did not
// launch: the caller's deadline only bounds the wait, and the read itself
// finishes on its own detached budget.
type pluginReads struct {
	accounts map[int]*pluginRead
	models   map[int]*pluginRead
}

// launchParallelPluginReads starts one accounts read and one models read per
// running plugin whose cache is stale, then returns immediately so the round
// waits on all of them at once.
//
// Why: the round used to walk the plugins sequentially under the caller's
// context. healthz caps that context at 3s (healthzSnapshotTimeout) while a
// cold round has to walk every installed plugin, several of which do upstream
// network I/O inside GET /accounts (plugins/gemini/quota.go:26,
// plugins/codearts/accounts.go:32, plugins/officeace/accounts.go:88). Once the
// shared deadline passed, every remaining plugin failed instantly: the host log
// at 2026-10-22T17:20:32 shows all twelve plugins' /accounts *and* /v1/models
// failing in the same second, which a genuine 5s-per-plugin timeout could never
// do (that round would take a minute). zcode was simply the last name in the
// sorted list — its own /accounts handler (plugins/zcode/server.go:108) does no
// I/O at all.
func (a *App) launchParallelPluginReads(ctx context.Context, src schedulerSource, statuses []model.PluginStatus) pluginReads {
	reads := pluginReads{
		accounts: make(map[int]*pluginRead, len(statuses)),
		models:   make(map[int]*pluginRead, len(statuses)),
	}
	now := time.Now()
	for i, st := range statuses {
		id := strings.TrimSpace(st.Manifest.ID)
		if id == "" || !st.Running {
			continue
		}
		if _, fresh := a.cachedAccountsFresh(id, now); !fresh {
			reads.accounts[i] = a.startAccountsRead(ctx, src, id)
		}
		if len(st.Manifest.Models) == 0 {
			if _, fresh := a.cachedModelsFresh(id, now); !fresh {
				reads.models[i] = a.startModelsRead(ctx, src, id)
			}
		}
	}
	return reads
}

// startAccountsRead returns the in-flight /accounts read of one plugin, reusing
// the one already running so a burst of concurrent requests cannot stampede a
// plugin with one call each.
func (a *App) startAccountsRead(ctx context.Context, src schedulerSource, id string) *pluginRead {
	key := "accounts:" + id
	read, owned := a.beginRead(key)
	if !owned {
		return read
	}
	go func() {
		callCtx, cancel := context.WithTimeout(detachCallerDeadline(ctx), schedulerAccountTimeout)
		defer cancel()
		accounts, err := src.Accounts(callCtx, id)
		if err == nil {
			a.storeAccountsCache(id, accounts, time.Now())
		}
		read.accounts, read.err = accounts, err
		a.finishRead(key, read)
	}()
	return read
}

// startModelsRead is startAccountsRead for GET /v1/models.
func (a *App) startModelsRead(ctx context.Context, src schedulerSource, id string) *pluginRead {
	key := "models:" + id
	read, owned := a.beginRead(key)
	if !owned {
		return read
	}
	go func() {
		callCtx, cancel := context.WithTimeout(detachCallerDeadline(ctx), schedulerModelTimeout)
		defer cancel()
		models, err := src.Models(callCtx, id)
		if err == nil {
			a.storeModelsCache(id, models, time.Now())
		}
		read.models, read.err = models, err
		a.finishRead(key, read)
	}()
	return read
}

func (a *App) beginRead(key string) (*pluginRead, bool) {
	a.snapMu.Lock()
	defer a.snapMu.Unlock()
	if read, ok := a.snapReads[key]; ok {
		return read, false
	}
	if a.snapReads == nil {
		a.snapReads = make(map[string]*pluginRead)
	}
	read := &pluginRead{done: make(chan struct{})}
	a.snapReads[key] = read
	return read, true
}

// finishRead publishes one read. The map entry is dropped before done closes, so
// the next round starts a fresh read instead of waiting on a finished one.
func (a *App) finishRead(key string, read *pluginRead) {
	a.snapMu.Lock()
	if current, ok := a.snapReads[key]; ok && current == read {
		delete(a.snapReads, key)
	}
	a.snapMu.Unlock()
	close(read.done)
}

// detachCallerDeadline keeps the caller's values and cancellation but drops its
// deadline, so a plugin's own budget is neither shortened nor zeroed by however
// much of the round's budget the earlier work already spent.
func detachCallerDeadline(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return context.WithoutCancel(ctx)
}

// awaitRead reports whether the read finished within the caller's remaining
// budget. A nil read (nothing to fetch) is treated as not finished.
func awaitRead(ctx context.Context, read *pluginRead) bool {
	if read == nil {
		return false
	}
	select {
	case <-read.done:
		return true
	default:
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-read.done:
		return true
	case <-ctx.Done():
		return false
	}
}

// roundAccounts returns one plugin's accounts for this round: the live answer if
// it arrived in time, else the in-memory snapshot, else the persisted cache.
func (a *App) roundAccounts(ctx context.Context, id string, read *pluginRead) []model.Account {
	if accounts, ok := a.cachedAccountsFresh(id, time.Now()); ok {
		return accounts
	}
	switch {
	case read == nil:
		// Not running, or the cache was fresh when the round launched: fall back.
	case awaitRead(ctx, read):
		if read.err == nil {
			return append([]model.Account(nil), read.accounts...)
		}
		a.warnAccountsRead(id, read.err)
	default:
		// The round gave up, not the plugin: say so instead of blaming a plugin
		// that answers in milliseconds (the read keeps going and fills the cache
		// for the next round).
		a.logAccountsFallback(id, schedulerAccountTimeout)
	}
	if accounts, ok := a.cachedAccountsAny(id); ok {
		return accounts
	}
	return a.storedAccounts(id)
}

// roundModels returns one plugin's model map for this round. Manifest models
// win; then the fresh cache, the live answer, and finally the catalog.
func (a *App) roundModels(ctx context.Context, id string, st model.PluginStatus, read *pluginRead) map[string]model.ModelInfo {
	catalog, disabled := a.modelCatalogIndex(id)

	if len(st.Manifest.Models) > 0 {
		out := make(map[string]model.ModelInfo, len(st.Manifest.Models))
		for _, info := range st.Manifest.Models {
			if strings.TrimSpace(info.ID) == "" || disabled[info.ID] {
				continue
			}
			if extra, ok := catalog[info.ID]; ok {
				info = enrichModelInfo(info, extra)
			}
			out[info.ID] = info
		}
		return out
	}
	if models, ok := a.cachedModelsFresh(id, time.Now()); ok {
		return dropDisabledModels(mergeModelInfos(models, catalog), disabled)
	}
	if read != nil {
		if awaitRead(ctx, read) {
			if read.err == nil {
				return dropDisabledModels(mergeModelInfos(read.models, catalog), disabled)
			}
			if a.logger != nil && !errors.Is(read.err, context.Canceled) {
				a.logger.Warn("app", "scheduling snapshot cannot read models", map[string]any{
					"plugin": id, "error": read.err.Error(),
				})
			}
		} else if a.logger != nil {
			a.logger.Info("app", "scheduling snapshot uses cached models", map[string]any{
				"plugin": id, "budget": schedulerModelTimeout.String(),
			})
		}
	}
	if len(catalog) > 0 {
		return dropDisabledModels(catalog, disabled)
	}
	return nil
}

// warnAccountsRead records a plugin that really failed (or really ran past its
// own budget). A client hanging up cancels this read; that is normal traffic, not
// plugin failure, and must not become warning-level log noise.
func (a *App) warnAccountsRead(id string, err error) {
	if a.logger == nil || errors.Is(err, context.Canceled) {
		return
	}
	a.logger.Warn("app", "scheduling snapshot cannot read accounts", map[string]any{
		"plugin": id, "error": err.Error(), "budget": schedulerAccountTimeout.String(),
	})
}

func (a *App) logAccountsFallback(id string, budget time.Duration) {
	if a.logger == nil {
		return
	}
	a.logger.Info("app", "scheduling snapshot uses cached accounts", map[string]any{
		"plugin": id, "budget": budget.String(),
	})
}

// storedAccounts falls back to the persisted account cache of one plugin.
func (a *App) storedAccounts(id string) []model.Account {
	if a.store == nil {
		return nil
	}
	accounts, _, err := a.store.ListAccountCache(id)
	if err != nil {
		if a.logger != nil {
			a.logger.Warn("app", "cannot read account cache for scheduling", map[string]any{
				"plugin": id, "error": err.Error(),
			})
		}
		return nil
	}
	return accounts
}

// ---------------------------------------------------------------- settings

// globalPoolSettings reads the "__global__" defaults row.
func (a *App) globalPoolSettings() (model.PoolSettings, bool) {
	if a.store == nil {
		return model.PoolSettings{}, false
	}
	row, ok, err := a.store.GetPoolSettings(model.GlobalPoolID)
	if err != nil {
		if a.logger != nil {
			a.logger.Warn("app", "cannot read global pool settings", map[string]any{"error": err.Error()})
		}
		return model.PoolSettings{}, false
	}
	return row, ok
}

// poolDefaults applies the global row over the documented baseline.
//
// The two cases are deliberately different:
//   - no stored row at all: use the baselines. Without them the TTLs and the
//     hedge threshold stay 0, which silently means "no KV-cache reuse" and
//     "hedging off" even though the UI presents both as working features.
//   - a stored row: it is the operator's decision, taken verbatim. In particular
//     a stored 0 means "unlimited" / "off" and must NOT be replaced by the
//     baseline, or upgrading would switch hedging on for someone who turned it
//     off.
func poolDefaults(global model.PoolSettings, globalOK bool) model.PoolSettings {
	// AccountConcurrency is baseline 0 (unlimited) on purpose: a non-zero
	// default would start throttling pools that work fine today.
	out := model.PoolSettings{
		UpstreamEnabled:    true,
		Strategy:           scheduler.StrategyCreditFirst,
		PoolPriority:       100,
		DefaultReasoning:   "",
		AccountConcurrency: 0,
		PoolStickyTTL:      defaultPoolStickyTTL,
		ConvStickyTTL:      defaultConvStickyTTL,
		HedgeMS:            defaultHedgeMS,
	}
	if !globalOK {
		return out
	}
	// A stored row replaces the baseline outright; nothing below falls back.
	out.AccountConcurrency = global.AccountConcurrency
	out.PoolStickyTTL = global.PoolStickyTTL
	out.ConvStickyTTL = global.ConvStickyTTL
	out.HedgeMS = global.HedgeMS
	if strategy := strings.TrimSpace(global.Strategy); scheduler.ValidStrategy(strategy) {
		out.Strategy = strategy
	}
	out.DefaultReasoning = strings.TrimSpace(global.DefaultReasoning)
	out.BackgroundDowngrade = global.BackgroundDowngrade
	out.LongctxDowngrade = global.LongctxDowngrade
	out.ToolExec = global.ToolExec
	out.ModelSyncTime = global.ModelSyncTime
	if global.PoolPriority != 0 {
		out.PoolPriority = global.PoolPriority
	}
	return out
}

// effectivePoolSettings merges one plugin row over the global defaults.
func effectivePoolSettings(defaults, row model.PoolSettings, rowOK bool) model.PoolSettings {
	out := defaults
	if !rowOK {
		return out
	}
	out.PluginID = row.PluginID
	out.UpstreamEnabled = row.UpstreamEnabled
	if strategy := strings.TrimSpace(row.Strategy); strategy != "" {
		out.Strategy = strategy
	}
	out.PoolPriority = row.PoolPriority
	out.AccountConcurrency = row.AccountConcurrency
	out.PoolStickyTTL = row.PoolStickyTTL
	out.ConvStickyTTL = row.ConvStickyTTL
	out.HedgeMS = row.HedgeMS
	if reasoning := strings.TrimSpace(row.DefaultReasoning); reasoning != "" {
		out.DefaultReasoning = reasoning
	}
	out.BackgroundDowngrade = row.BackgroundDowngrade
	out.LongctxDowngrade = row.LongctxDowngrade
	out.ToolExec = row.ToolExec
	if syncTime := strings.TrimSpace(row.ModelSyncTime); syncTime != "" {
		out.ModelSyncTime = syncTime
	}
	out.UpdatedAt = row.UpdatedAt
	return out
}

// schedulerInterPool returns the persisted inter-pool mode.
func (a *App) schedulerInterPool() string {
	if a.store != nil {
		if raw, ok, err := a.store.GetKV(schedulerInterPoolKey); err == nil && ok {
			if mode := strings.TrimSpace(raw); scheduler.ValidInterPool(mode) {
				return mode
			}
		}
	}
	return scheduler.InterPoolSmart
}

// schedulerVersion returns the persisted strategy version (0 before the first
// mutation).
func (a *App) schedulerVersion() int {
	if a.store == nil {
		return 0
	}
	raw, ok, err := a.store.GetKV(schedulerVersionKey)
	if err != nil || !ok {
		return 0
	}
	version, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || version < 0 {
		return 0
	}
	return version
}

// bumpSchedulerVersion increments and persists the strategy version.
func (a *App) bumpSchedulerVersion() int {
	version := a.schedulerVersion() + 1
	if a.store != nil {
		if err := a.store.SetKV(schedulerVersionKey, strconv.Itoa(version)); err != nil && a.logger != nil {
			a.logger.Warn("app", "cannot persist scheduler version", map[string]any{"error": err.Error()})
		}
	}
	return version
}

// ---------------------------------------------------------------- accounts

// schedulerAccountsFor reads ONE plugin's accounts synchronously with a 10s
// cache; the scheduling round itself uses roundAccounts (parallel fan-out). A
// stopped plugin is never called; its last in-memory or account_cache snapshot
// is used instead (design §9: offline accounts are filtered, not lost).
func (a *App) schedulerAccountsFor(ctx context.Context, src schedulerSource, st model.PluginStatus) []model.Account {
	id := strings.TrimSpace(st.Manifest.ID)
	if id == "" || src == nil {
		return nil
	}
	now := time.Now()
	if accounts, ok := a.cachedAccountsFresh(id, now); ok {
		return accounts
	}
	if st.Running {
		callCtx, cancel := context.WithTimeout(ctx, schedulerAccountTimeout)
		defer cancel()
		accounts, err := src.Accounts(callCtx, id)
		if err == nil {
			a.storeAccountsCache(id, accounts, now)
			return accounts
		}
		a.warnAccountsRead(id, err)
	}
	if accounts, ok := a.cachedAccountsAny(id); ok {
		return accounts
	}
	return a.storedAccounts(id)
}

func (a *App) cachedAccountsFresh(id string, now time.Time) ([]model.Account, bool) {
	a.snapMu.Lock()
	defer a.snapMu.Unlock()
	entry, ok := a.snapAccounts[id]
	if !ok || now.Sub(entry.at) >= schedulerAccountTTL {
		return nil, false
	}
	return append([]model.Account(nil), entry.accounts...), true
}

func (a *App) cachedAccountsAny(id string) ([]model.Account, bool) {
	a.snapMu.Lock()
	defer a.snapMu.Unlock()
	entry, ok := a.snapAccounts[id]
	if !ok {
		return nil, false
	}
	return append([]model.Account(nil), entry.accounts...), true
}

func (a *App) storeAccountsCache(id string, accounts []model.Account, at time.Time) {
	a.snapMu.Lock()
	defer a.snapMu.Unlock()
	if a.snapAccounts == nil {
		a.snapAccounts = make(map[string]cachedAccountSet)
	}
	a.snapAccounts[id] = cachedAccountSet{accounts: append([]model.Account(nil), accounts...), at: at}
}

// schedulerAccount converts a plugin account plus its overlay.
func schedulerAccount(acc model.Account, meta model.AccountMeta, hasMeta bool) scheduler.Account {
	enabled := acc.Enabled
	if hasMeta && meta.EnabledOverride != nil {
		enabled = enabled && *meta.EnabledOverride
	}
	credentialPresent := acc.Credential == nil || acc.Credential.Present
	priority := acc.Priority
	if priority == 0 {
		priority = model.DefaultAccountPriority
	}
	weight := acc.Weight
	if weight <= 0 {
		weight = model.DefaultAccountWeight
	}
	if hasMeta && meta.Priority != nil {
		priority = *meta.Priority
	}
	if hasMeta && meta.Weight != nil {
		weight = *meta.Weight
	}
	// The health cooldown of plan 2026-10-21 §4 is a scheduling fact too: a
	// parked account must not be picked while it is cooling down.
	cooldownUntil := acc.CooldownUntil
	if hasMeta && meta.CooldownUntil > cooldownUntil {
		cooldownUntil = meta.CooldownUntil
	}
	return scheduler.Account{
		ID:                acc.ID,
		Label:             firstNonEmpty(acc.Label, acc.ID),
		Enabled:           enabled,
		CredentialPresent: credentialPresent,
		Status:            acc.Status,
		Remaining:         acc.Quota.Remaining,
		Total:             acc.Quota.Total,
		Unlimited:         acc.Quota.Unlimited,
		ExpiresAt:         acc.Quota.ExpiresAt,
		Plan:              acc.Quota.Plan,
		Priority:          priority,
		Weight:            weight,
		CooldownUntil:     cooldownUntil,
		ModelCooldowns:    acc.ModelCooldowns,
	}
}

// withEgressEvidence fills the recent proxy verdict into a scheduler account.
// Pick runs under the scheduler lock, so the evidence travels in the snapshot
// instead of being looked up during planning.
func (a *App) withEgressEvidence(pluginID string, acc scheduler.Account) scheduler.Account {
	if a.egressHealth == nil {
		return acc
	}
	ev := a.egressHealth.Lookup(pluginID, acc.ID)
	if ev.Mode != model.EgressProxy || !ev.Dead() {
		return acc
	}
	acc.Egress = scheduler.EgressEvidence{Failed: true, Label: ev.ProxyLabel, Error: ev.Error, At: ev.At}
	return acc
}

// poolMemberSet loads explicit pool membership. Nil means "all accounts with a
// credential participate".
func (a *App) poolMemberSet(pluginID string) map[string]bool {
	if a.store == nil {
		return nil
	}
	rows, err := a.store.ListPoolMembers(pluginID)
	if err != nil {
		if a.logger != nil {
			a.logger.Warn("app", "cannot read pool members", map[string]any{
				"plugin": pluginID, "error": err.Error(),
			})
		}
		return nil
	}
	if len(rows) == 0 {
		return nil
	}
	members := make(map[string]bool, len(rows))
	for _, row := range rows {
		members[row.AccountID] = row.Enabled
	}
	return members
}

// ---------------------------------------------------------------- models

// schedulerModelsFor returns the plugin's model metadata map. Manifest models
// win; an empty manifest falls back to the catalog and then to a 30s cached
// /v1/models answer.
func (a *App) schedulerModelsFor(ctx context.Context, src schedulerSource, st model.PluginStatus) map[string]model.ModelInfo {
	id := strings.TrimSpace(st.Manifest.ID)
	if id == "" {
		return nil
	}
	catalog, disabled := a.modelCatalogIndex(id)

	if len(st.Manifest.Models) > 0 {
		out := make(map[string]model.ModelInfo, len(st.Manifest.Models))
		for _, info := range st.Manifest.Models {
			if strings.TrimSpace(info.ID) == "" || disabled[info.ID] {
				continue
			}
			if extra, ok := catalog[info.ID]; ok {
				info = enrichModelInfo(info, extra)
			}
			out[info.ID] = info
		}
		return out
	}

	now := time.Now()
	if models, ok := a.cachedModelsFresh(id, now); ok {
		return dropDisabledModels(mergeModelInfos(models, catalog), disabled)
	}
	if st.Running && src != nil {
		callCtx, cancel := context.WithTimeout(ctx, schedulerModelTimeout)
		models, err := src.Models(callCtx, id)
		cancel()
		if err == nil {
			a.storeModelsCache(id, models, now)
			return dropDisabledModels(mergeModelInfos(models, catalog), disabled)
		}
		if a.logger != nil {
			a.logger.Warn("app", "scheduling snapshot cannot read models", map[string]any{
				"plugin": id, "error": err.Error(),
			})
		}
	}
	if len(catalog) > 0 {
		return dropDisabledModels(catalog, disabled)
	}
	return nil
}

// dropDisabledModels removes models the user switched off on the resource
// page. A nil/empty disabled set leaves the map untouched.
func dropDisabledModels(models map[string]model.ModelInfo, disabled map[string]bool) map[string]model.ModelInfo {
	if len(disabled) == 0 {
		return models
	}
	for id := range disabled {
		delete(models, id)
	}
	if len(models) == 0 {
		return nil
	}
	return models
}

func (a *App) modelCatalogIndex(pluginID string) (map[string]model.ModelInfo, map[string]bool) {
	if a.store == nil {
		return nil, nil
	}
	entries, err := a.store.ListModelCatalog(pluginID)
	if err != nil {
		if a.logger != nil {
			a.logger.Warn("app", "cannot read model catalog", map[string]any{
				"plugin": pluginID, "error": err.Error(),
			})
		}
		return nil, nil
	}
	if len(entries) == 0 {
		return nil, nil
	}
	out := make(map[string]model.ModelInfo, len(entries))
	disabled := make(map[string]bool)
	for _, entry := range entries {
		out[entry.ModelID] = catalogModelInfo(entry)
		if !entry.Enabled {
			disabled[entry.ModelID] = true
		}
	}
	return out, disabled
}

func catalogModelInfo(entry model.ModelCatalogEntry) model.ModelInfo {
	name := strings.TrimSpace(entry.DisplayName)
	if name == "" {
		name = entry.ModelID
	}
	return model.ModelInfo{
		ID:            entry.ModelID,
		Name:          name,
		Vendor:        entry.Vendor,
		Multiplier:    entry.Multiplier,
		Thinking:      entry.Thinking,
		ContextLength: entry.ContextLength,
		Vision:        entry.Vision,
		Free:          entry.Free,
	}
}

func mergeModelInfos(models []model.ModelInfo, catalog map[string]model.ModelInfo) map[string]model.ModelInfo {
	out := make(map[string]model.ModelInfo, len(models)+len(catalog))
	for id, info := range catalog {
		out[id] = info
	}
	for _, info := range models {
		if strings.TrimSpace(info.ID) == "" {
			continue
		}
		if extra, ok := out[info.ID]; ok {
			info = enrichModelInfo(info, extra)
		}
		out[info.ID] = info
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// enrichModelInfo keeps live fields and fills catalog-only metadata.
func enrichModelInfo(info, extra model.ModelInfo) model.ModelInfo {
	if info.Name == "" {
		info.Name = extra.Name
	}
	if info.Vendor == "" {
		info.Vendor = extra.Vendor
	}
	if info.Multiplier <= 0 {
		info.Multiplier = extra.Multiplier
	}
	if len(info.Thinking) == 0 {
		info.Thinking = extra.Thinking
	}
	if info.ContextLength == 0 {
		info.ContextLength = extra.ContextLength
	}
	info.Vision = info.Vision || extra.Vision
	info.Free = info.Free || extra.Free
	return info
}

func (a *App) cachedModelsFresh(id string, now time.Time) ([]model.ModelInfo, bool) {
	a.snapMu.Lock()
	defer a.snapMu.Unlock()
	entry, ok := a.snapModels[id]
	if !ok || now.Sub(entry.at) >= schedulerModelTTL {
		return nil, false
	}
	return append([]model.ModelInfo(nil), entry.models...), true
}

func (a *App) storeModelsCache(id string, models []model.ModelInfo, at time.Time) {
	a.snapMu.Lock()
	defer a.snapMu.Unlock()
	if a.snapModels == nil {
		a.snapModels = make(map[string]cachedModelSet)
	}
	a.snapModels[id] = cachedModelSet{models: append([]model.ModelInfo(nil), models...), at: at}
}

// ---------------------------------------------------------------- cooldowns

// schedulerCooldownStore persists request-path cooldowns in scheduler_cooldowns.
// Ported from A (TraeWorkAssistant, MIT, 朱天伟) store/docs.rs:1266-1290 and
// models.rs:712-731: A stores the deadline plus the consecutive-error counter
// and restores both on startup (api_server/pool.rs:331-370), so a restart
// resumes the backoff. No credentials or account labels are stored here.
type schedulerCooldownStore struct{ store *store.Store }

func (s schedulerCooldownStore) LoadCooldowns(now int64) ([]scheduler.CooldownRecord, error) {
	if s.store == nil {
		return nil, nil
	}
	rows, err := s.store.LoadSchedulerCooldowns(now)
	if err != nil {
		return nil, err
	}
	out := make([]scheduler.CooldownRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, scheduler.CooldownRecord{
			PluginID: row.PluginID, AccountID: row.AccountID, Model: row.ModelID,
			Until: row.Until, Reason: row.Reason, Strikes: row.Strikes,
		})
	}
	return out, nil
}

func (s schedulerCooldownStore) SaveCooldown(record scheduler.CooldownRecord) error {
	if s.store == nil {
		return nil
	}
	return s.store.UpsertSchedulerCooldown(store.SchedulerCooldown{
		PluginID: record.PluginID, AccountID: record.AccountID, ModelID: record.Model,
		Until: record.Until, Reason: record.Reason, Strikes: record.Strikes,
	})
}

func (s schedulerCooldownStore) DeleteCooldown(pluginID, accountID, modelID string) error {
	if s.store == nil {
		return nil
	}
	return s.store.DeleteSchedulerCooldown(pluginID, accountID, modelID)
}
