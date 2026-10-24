package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"aiworkgate/internal/model"
	"aiworkgate/internal/scheduler"
	"aiworkgate/internal/webui"
)

// Scheduler center and resource scheduling admin API (§5.4, §5.5). The webui
// layer has already validated shapes; these methods validate against the
// stored/installed state, persist, bump scheduler_version and write audit rows.

const (
	minSchedulerHedge = 1000
	maxSchedulerHedge = 8000
	maxSchedulerTTL   = 86400
	maxSchedulerConc  = 32
)

// installedPlugin resolves one installed plugin or returns ErrPluginNotFound.
func (a *App) installedPlugin(pluginID string) (model.PluginStatus, error) {
	pluginID = strings.TrimSpace(pluginID)
	if a.pluginMgr == nil {
		return model.PluginStatus{}, fmt.Errorf("%w: plugin manager is not available", webui.ErrPluginNotFound)
	}
	st, ok := a.pluginMgr.Get(pluginID)
	if !ok {
		return model.PluginStatus{}, fmt.Errorf("%w: %s", webui.ErrPluginNotFound, pluginID)
	}
	return st, nil
}

// validateSchedulerSettings guards direct app callers (the admin API already
// validates the same ranges in webui).
func validateSchedulerSettings(settings model.PoolSettings) error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", webui.ErrInvalidRequest, fmt.Sprintf(format, args...))
	}
	if settings.Strategy != "" && !scheduler.ValidStrategy(settings.Strategy) {
		return invalid("strategy %q is not supported", settings.Strategy)
	}
	if settings.PoolPriority < 0 || settings.PoolPriority > 1000 {
		return invalid("pool_priority must be between 0 and 1000")
	}
	if settings.AccountConcurrency < 0 || settings.AccountConcurrency > maxSchedulerConc {
		return invalid("account_concurrency must be between 0 and %d", maxSchedulerConc)
	}
	if settings.PoolStickyTTL < 0 || settings.PoolStickyTTL > maxSchedulerTTL {
		return invalid("pool_sticky_ttl must be between 0 and %d seconds", maxSchedulerTTL)
	}
	if settings.ConvStickyTTL < 0 || settings.ConvStickyTTL > maxSchedulerTTL {
		return invalid("conv_sticky_ttl must be between 0 and %d seconds", maxSchedulerTTL)
	}
	if settings.HedgeMS != 0 && (settings.HedgeMS < minSchedulerHedge || settings.HedgeMS > maxSchedulerHedge) {
		return invalid("hedge_ms must be 0 or between %d and %d", minSchedulerHedge, maxSchedulerHedge)
	}
	switch strings.ToLower(strings.TrimSpace(settings.DefaultReasoning)) {
	case "", "off", "low", "medium", "high":
	default:
		return invalid("default_reasoning must be empty, off, low, medium or high")
	}
	if raw := strings.TrimSpace(settings.ModelSyncTime); raw != "" {
		if _, err := time.Parse("15:04", raw); err != nil {
			return invalid("model_sync_time must be HH:MM")
		}
	}
	return nil
}

// ---------------------------------------------------------------- overview

func (a *App) schedulerOverview() (any, error) {
	global, globalOK := a.globalPoolSettings()
	defaults := poolDefaults(global, globalOK)

	settingsRows := map[string]model.PoolSettings{}
	if a.store != nil {
		rows, err := a.store.ListPoolSettings()
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			settingsRows[row.PluginID] = row
		}
	}

	pools := make([]map[string]any, 0)
	health := make([]map[string]any, 0)
	if a.pluginMgr != nil {
		list := a.pluginMgr.List()
		sort.SliceStable(list, func(i, j int) bool { return list[i].Manifest.ID < list[j].Manifest.ID })
		for _, st := range list {
			id := strings.TrimSpace(st.Manifest.ID)
			if id == "" {
				continue
			}
			row, rowOK := settingsRows[id]
			settings := effectivePoolSettings(defaults, row, rowOK)
			pools = append(pools, map[string]any{
				"plugin_id":            id,
				"name":                 strings.TrimSpace(st.Manifest.Name),
				"api_level":            st.Manifest.APILevel,
				"upstream_enabled":     settings.UpstreamEnabled,
				"strategy":             settings.Strategy,
				"pool_priority":        settings.PoolPriority,
				"account_concurrency":  settings.AccountConcurrency,
				"pool_sticky_ttl":      settings.PoolStickyTTL,
				"conv_sticky_ttl":      settings.ConvStickyTTL,
				"hedge_ms":             settings.HedgeMS,
				"default_reasoning":    settings.DefaultReasoning,
				"background_downgrade": settings.BackgroundDowngrade,
				"longctx_downgrade":    settings.LongctxDowngrade,
				"model_sync_time":      settings.ModelSyncTime,
			})
			health = append(health, map[string]any{
				"plugin_id":    id,
				"circuit_open": a.dispatch != nil && a.dispatch.CircuitOpen(id),
			})
		}
	}

	models := []map[string]any{}
	stickyCount := 0
	if a.store != nil {
		bindings, err := a.store.ListModelBindings()
		if err != nil {
			return nil, err
		}
		for _, binding := range bindings {
			items := make([]map[string]any, 0, len(binding.Items))
			for _, item := range binding.Items {
				items = append(items, map[string]any{
					"plugin_id": item.SourcePlugin,
					"model":     item.SourceModel,
				})
			}
			models = append(models, map[string]any{
				"logical_model": binding.LogicalModel,
				"display_name":  binding.DisplayName,
				"items":         items,
			})
		}
		if _, err := a.store.DeleteExpiredStickyBindings(time.Now().Unix()); err == nil {
			if rows, err := a.store.ListStickyBindings(""); err == nil {
				stickyCount = len(rows)
			}
		}
	}

	return map[string]any{
		"version": a.schedulerVersion(),
		"global": map[string]any{
			"inter_pool": a.schedulerInterPool(),
			"defaults":   schedulerDefaultsPayload(defaults),
		},
		"pools":        pools,
		"models":       models,
		"sticky_count": stickyCount,
		"health":       health,
	}, nil
}

func schedulerDefaultsPayload(settings model.PoolSettings) map[string]any {
	return map[string]any{
		"strategy":             settings.Strategy,
		"account_concurrency":  settings.AccountConcurrency,
		"pool_sticky_ttl":      settings.PoolStickyTTL,
		"conv_sticky_ttl":      settings.ConvStickyTTL,
		"hedge_ms":             settings.HedgeMS,
		"default_reasoning":    settings.DefaultReasoning,
		"background_downgrade": settings.BackgroundDowngrade,
		"longctx_downgrade":    settings.LongctxDowngrade,
	}
}

func poolSettingsPayload(settings model.PoolSettings) map[string]any {
	out := schedulerDefaultsPayload(settings)
	out["upstream_enabled"] = settings.UpstreamEnabled
	out["strategy"] = settings.Strategy
	out["pool_priority"] = settings.PoolPriority
	out["tool_exec"] = settings.ToolExec
	out["model_sync_time"] = settings.ModelSyncTime
	return out
}

// ---------------------------------------------------------------- global

func (a *App) saveSchedulerGlobal(update webui.SchedulerGlobalUpdate) (any, error) {
	if a.store == nil {
		return nil, errors.New("app: store is not available")
	}
	mode := ""
	if update.InterPool != nil {
		mode = strings.ToLower(strings.TrimSpace(*update.InterPool))
		if !scheduler.ValidInterPool(mode) {
			return nil, fmt.Errorf("%w: inter_pool must be smart or fixed", webui.ErrInvalidRequest)
		}
	}
	var settings *model.PoolSettings
	if update.Defaults != nil {
		global, globalOK := a.globalPoolSettings()
		next := poolDefaults(global, globalOK)
		next.PluginID = model.GlobalPoolID
		applySchedulerDefaults(&next, update.Defaults)
		if err := validateSchedulerSettings(next); err != nil {
			return nil, err
		}
		settings = &next
	}
	if mode != "" {
		if err := a.store.SetKV(schedulerInterPoolKey, mode); err != nil {
			return nil, err
		}
	}
	if settings != nil {
		if err := a.store.UpsertPoolSettings(*settings); err != nil {
			return nil, err
		}
	}
	a.bumpSchedulerVersion()
	a.writeAudit(model.AuditScheduler, "global", "scheduler", "ok")
	return a.schedulerOverview()
}

func applySchedulerDefaults(settings *model.PoolSettings, patch *webui.SchedulerDefaultsPatch) {
	if patch == nil {
		return
	}
	if patch.Strategy != nil {
		settings.Strategy = strings.TrimSpace(*patch.Strategy)
	}
	if patch.AccountConcurrency != nil {
		settings.AccountConcurrency = *patch.AccountConcurrency
	}
	if patch.PoolStickyTTL != nil {
		settings.PoolStickyTTL = *patch.PoolStickyTTL
	}
	if patch.ConvStickyTTL != nil {
		settings.ConvStickyTTL = *patch.ConvStickyTTL
	}
	if patch.HedgeMS != nil {
		settings.HedgeMS = *patch.HedgeMS
	}
	if patch.DefaultReasoning != nil {
		settings.DefaultReasoning = strings.TrimSpace(*patch.DefaultReasoning)
	}
	if patch.BackgroundDowngrade != nil {
		settings.BackgroundDowngrade = *patch.BackgroundDowngrade
	}
	if patch.LongctxDowngrade != nil {
		settings.LongctxDowngrade = *patch.LongctxDowngrade
	}
}

// ---------------------------------------------------------------- pool policy

func (a *App) effectiveSettings(pluginID string) (model.PoolSettings, error) {
	global, globalOK := a.globalPoolSettings()
	defaults := poolDefaults(global, globalOK)
	var (
		row   model.PoolSettings
		rowOK bool
	)
	if a.store != nil {
		existing, ok, err := a.store.GetPoolSettings(pluginID)
		if err != nil {
			return model.PoolSettings{}, err
		}
		row, rowOK = existing, ok
	}
	settings := effectivePoolSettings(defaults, row, rowOK)
	settings.PluginID = pluginID
	return settings, nil
}

func (a *App) saveSchedulerPool(pluginID string, update webui.SchedulerPoolUpdate) (any, error) {
	if _, err := a.installedPlugin(pluginID); err != nil {
		return nil, err
	}
	if a.store == nil {
		return nil, errors.New("app: store is not available")
	}
	settings, err := a.effectiveSettings(pluginID)
	if err != nil {
		return nil, err
	}
	applySchedulerPoolUpdate(&settings, update)
	if err := validateSchedulerSettings(settings); err != nil {
		return nil, err
	}
	if err := a.store.UpsertPoolSettings(settings); err != nil {
		return nil, err
	}
	a.bumpSchedulerVersion()
	a.writeAudit(model.AuditScheduler, "pool", pluginID, "ok")
	return a.schedulerOverview()
}

func applySchedulerPoolUpdate(settings *model.PoolSettings, update webui.SchedulerPoolUpdate) {
	if update.UpstreamEnabled != nil {
		settings.UpstreamEnabled = *update.UpstreamEnabled
	}
	if update.Strategy != nil {
		settings.Strategy = strings.TrimSpace(*update.Strategy)
	}
	if update.PoolPriority != nil {
		settings.PoolPriority = *update.PoolPriority
	}
	if update.AccountConcurrency != nil {
		settings.AccountConcurrency = *update.AccountConcurrency
	}
	if update.PoolStickyTTL != nil {
		settings.PoolStickyTTL = *update.PoolStickyTTL
	}
	if update.ConvStickyTTL != nil {
		settings.ConvStickyTTL = *update.ConvStickyTTL
	}
	if update.HedgeMS != nil {
		settings.HedgeMS = *update.HedgeMS
	}
	if update.DefaultReasoning != nil {
		settings.DefaultReasoning = strings.TrimSpace(*update.DefaultReasoning)
	}
	if update.BackgroundDowngrade != nil {
		settings.BackgroundDowngrade = *update.BackgroundDowngrade
	}
	if update.LongctxDowngrade != nil {
		settings.LongctxDowngrade = *update.LongctxDowngrade
	}
	if update.ModelSyncTime != nil {
		settings.ModelSyncTime = strings.TrimSpace(*update.ModelSyncTime)
	}
}

// ---------------------------------------------------------------- presets

type schedulerPreset struct {
	interPool   string
	strategy    string
	concurrency int
	poolSticky  int
	convSticky  int
	hedge       int
}

func lookupPreset(name string) (schedulerPreset, bool) {
	switch name {
	case "balanced":
		return schedulerPreset{scheduler.InterPoolSmart, scheduler.StrategyCreditFirst, 2, 600, 600, 0}, true
	case "freshness":
		return schedulerPreset{scheduler.InterPoolSmart, scheduler.StrategyCreditFirst, 2, 0, 0, 0}, true
	case "amortized":
		return schedulerPreset{scheduler.InterPoolSmart, scheduler.StrategyRemainingFirst, 2, 600, 600, 0}, true
	case "latency":
		return schedulerPreset{scheduler.InterPoolSmart, scheduler.StrategyWeighted3, 2, 600, 600, 3000}, true
	case "fixed":
		return schedulerPreset{scheduler.InterPoolFixed, scheduler.StrategyFixed, 2, 600, 600, 0}, true
	}
	return schedulerPreset{}, false
}

// applySchedulerPreset writes the global defaults and every installed pool in
// one transaction, then persists the inter-pool mode and bumps the version.
func (a *App) applySchedulerPreset(name string) (any, error) {
	preset, ok := lookupPreset(strings.ToLower(strings.TrimSpace(name)))
	if !ok {
		return nil, fmt.Errorf("%w: unknown preset %q", webui.ErrInvalidRequest, name)
	}
	if a.store == nil {
		return nil, errors.New("app: store is not available")
	}
	global, globalOK := a.globalPoolSettings()
	globalRow := poolDefaults(global, globalOK)
	globalRow.PluginID = model.GlobalPoolID
	preset.apply(&globalRow)

	rows := []model.PoolSettings{globalRow}
	if a.pluginMgr != nil {
		list := a.pluginMgr.List()
		sort.SliceStable(list, func(i, j int) bool { return list[i].Manifest.ID < list[j].Manifest.ID })
		for _, st := range list {
			id := strings.TrimSpace(st.Manifest.ID)
			if id == "" {
				continue
			}
			settings, err := a.effectiveSettings(id)
			if err != nil {
				return nil, err
			}
			preset.apply(&settings)
			settings.PluginID = id
			rows = append(rows, settings)
		}
	}
	if err := a.store.UpsertPoolSettingsBatch(rows); err != nil {
		return nil, err
	}
	if err := a.store.SetKV(schedulerInterPoolKey, preset.interPool); err != nil {
		return nil, err
	}
	a.bumpSchedulerVersion()
	a.writeAudit(model.AuditScheduler, "preset", name, "ok")
	return a.schedulerOverview()
}

func (p schedulerPreset) apply(settings *model.PoolSettings) {
	settings.Strategy = p.strategy
	settings.AccountConcurrency = p.concurrency
	settings.PoolStickyTTL = p.poolSticky
	settings.ConvStickyTTL = p.convSticky
	settings.HedgeMS = p.hedge
	settings.DefaultReasoning = ""
	settings.BackgroundDowngrade = false
	settings.LongctxDowngrade = false
}

// ---------------------------------------------------------------- model bindings

func (a *App) schedulerModels() (any, error) {
	models := []map[string]any{}
	if a.store != nil {
		bindings, err := a.store.ListModelBindings()
		if err != nil {
			return nil, err
		}
		for _, binding := range bindings {
			items := make([]map[string]any, 0, len(binding.Items))
			for _, item := range binding.Items {
				items = append(items, map[string]any{
					"plugin_id": item.SourcePlugin,
					"model":     item.SourceModel,
				})
			}
			models = append(models, map[string]any{
				"logical_model": binding.LogicalModel,
				"display_name":  binding.DisplayName,
				"items":         items,
			})
		}
	}
	return map[string]any{"models": models}, nil
}

func (a *App) saveSchedulerModel(logical string, update webui.SchedulerModelUpdate) (any, error) {
	logical = strings.TrimSpace(logical)
	if logical == "" {
		return nil, fmt.Errorf("%w: logical model is required", webui.ErrInvalidRequest)
	}
	if a.store == nil {
		return nil, errors.New("app: store is not available")
	}
	if len(update.Items) == 0 {
		if err := a.store.DeleteModelBinding(logical); err != nil {
			return nil, err
		}
		a.bumpSchedulerVersion()
		a.writeAudit(model.AuditScheduler, "model_delete", logical, "ok")
		return a.schedulerModels()
	}
	if err := a.validateModelSources(update.Items); err != nil {
		return nil, err
	}
	binding := model.ModelBinding{
		LogicalModel: logical,
		DisplayName:  strings.TrimSpace(update.DisplayName),
	}
	for i, item := range update.Items {
		binding.Items = append(binding.Items, model.ModelBindingItem{
			LogicalModel: logical,
			Position:     i,
			SourcePlugin: strings.TrimSpace(item.PluginID),
			SourceModel:  strings.TrimSpace(item.Model),
		})
	}
	if err := a.store.ReplaceModelBinding(binding); err != nil {
		return nil, err
	}
	a.bumpSchedulerVersion()
	a.writeAudit(model.AuditScheduler, "model_save", logical, "ok")
	return a.schedulerModels()
}

func (a *App) deleteSchedulerModel(logical string) error {
	logical = strings.TrimSpace(logical)
	if logical == "" {
		return fmt.Errorf("%w: logical model is required", webui.ErrInvalidRequest)
	}
	if a.store == nil {
		return errors.New("app: store is not available")
	}
	if err := a.store.DeleteModelBinding(logical); err != nil {
		return err
	}
	a.bumpSchedulerVersion()
	a.writeAudit(model.AuditScheduler, "model_delete", logical, "ok")
	return nil
}

// validateModelSources rejects sources that do not exist in a plugin manifest
// or catalog (§5.4).
func (a *App) validateModelSources(items []webui.SchedulerModelSource) error {
	ctx, cancel := context.WithTimeout(context.Background(), schedulerModelTimeout)
	defer cancel()
	for _, item := range items {
		pluginID := strings.TrimSpace(item.PluginID)
		modelID := strings.TrimSpace(item.Model)
		st, err := a.installedPlugin(pluginID)
		if err != nil {
			return fmt.Errorf("%w: source plugin %q is not installed", webui.ErrInvalidRequest, pluginID)
		}
		models := a.schedulerModelsFor(ctx, a.pluginMgr, st)
		if _, ok := models[modelID]; !ok {
			return fmt.Errorf("%w: model %s/%s is not in the plugin manifest or catalog",
				webui.ErrInvalidRequest, pluginID, modelID)
		}
	}
	return nil
}

// ---------------------------------------------------------------- explain

func (a *App) schedulerExplain(modelID, conversationID, clientKey string) (any, error) {
	if a.dispatch == nil {
		return nil, errors.New("app: scheduler is not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	snap, err := a.schedulerSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	explanation, err := a.dispatch.Explain(ctx, snap, scheduler.Request{
		Model:          modelID,
		ConversationID: conversationID,
		ClientKey:      clientKey,
	})
	if err == nil {
		return explanation, nil
	}
	if errors.Is(err, scheduler.ErrUnknownModel) {
		return nil, fmt.Errorf("%w: %v", webui.ErrUnknownModel, err)
	}
	if errors.Is(err, scheduler.ErrNoCandidate) {
		// A trace without a decision is still the most useful answer.
		return map[string]any{
			"decision":   nil,
			"inter_pool": explanation.InterPool,
			"candidates": explanation.Candidates,
			"reasons":    explanation.Reasons,
			"error":      err.Error(),
		}, nil
	}
	return nil, err
}

// ---------------------------------------------------------------- sticky admin

func (a *App) schedulerSticky() (any, error) {
	if a.store == nil {
		return map[string]any{"bindings": []any{}, "count": 0}, nil
	}
	_, _ = a.store.DeleteExpiredStickyBindings(time.Now().Unix())
	rows, err := a.store.ListStickyBindings("")
	if err != nil {
		return nil, err
	}
	bindings := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		bindings = append(bindings, map[string]any{
			"scope":      row.Scope,
			"key":        row.Key,
			"plugin_id":  row.PluginID,
			"account_id": row.AccountID,
			"expires_at": row.ExpiresAt,
		})
	}
	return map[string]any{"bindings": bindings, "count": len(bindings)}, nil
}

func (a *App) deleteSchedulerSticky(scope, key string) (int64, error) {
	if a.store == nil {
		return 0, errors.New("app: store is not available")
	}
	scope = strings.TrimSpace(scope)
	key = strings.TrimSpace(key)
	var removed int64
	if key != "" {
		rows, err := a.store.ListStickyBindings(scope)
		if err != nil {
			return 0, err
		}
		for _, row := range rows {
			if row.Key != key {
				continue
			}
			if err := a.store.DeleteStickyBinding(row.Scope, row.Key); err != nil {
				return removed, err
			}
			if a.dispatch != nil {
				a.dispatch.ForgetSticky(row.Scope, row.Key)
			}
			removed++
		}
	} else {
		rows, err := a.store.ListStickyBindings(scope)
		if err != nil {
			return 0, err
		}
		for _, row := range rows {
			if err := a.store.DeleteStickyBinding(row.Scope, row.Key); err != nil {
				return removed, err
			}
			removed++
		}
		if a.dispatch != nil {
			a.dispatch.ForgetAllSticky()
		}
	}
	target := "all"
	if scope != "" {
		target = scope
	}
	if key != "" {
		target += "/" + key
	}
	a.writeAudit(model.AuditScheduler, "sticky_clear", target, fmt.Sprintf("%d removed", removed))
	return removed, nil
}

// ---------------------------------------------------------------- pool detail

func (a *App) poolDetail(pluginID string) (any, error) {
	st, err := a.installedPlugin(pluginID)
	if err != nil {
		return nil, err
	}
	settings, err := a.effectiveSettings(pluginID)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	accounts := a.schedulerAccountsFor(ctx, a.pluginMgr, st)
	meta := a.accountMetaMap()
	members := a.poolMemberSet(pluginID)

	memberRows := make([]map[string]any, 0, len(accounts))
	healthy := 0
	var totalRemaining float64
	now := time.Now().Unix()
	for _, acc := range accounts {
		overlay, hasOverlay := meta[accountKey(pluginID, acc.ID)]
		sacc := a.withEgressEvidence(pluginID, schedulerAccount(acc, overlay, hasOverlay))
		if sacc.Remaining > 0 {
			totalRemaining += sacc.Remaining
		}
		if sacc.Enabled && sacc.CredentialPresent && sacc.CooldownUntil <= now {
			healthy++
		}
		member := sacc.CredentialPresent && (members == nil || members[acc.ID])
		display := acc.ID
		if hasOverlay && strings.TrimSpace(overlay.Alias) != "" {
			display = overlay.Alias
		} else if strings.TrimSpace(acc.Label) != "" {
			display = acc.Label
		}
		status := strings.TrimSpace(acc.Status)
		if status == "" {
			status = model.AccountOK
		}
		memberRows = append(memberRows, map[string]any{
			"account_id":   acc.ID,
			"display_name": display,
			"enabled":      sacc.Enabled,
			"status":       status,
			"remaining":    sacc.Remaining,
			"member":       member,
		})
	}

	var todayUsed float64
	todayRequests := int64(0)
	catalog := []map[string]any{}
	if a.store != nil {
		today := time.Now().Format("2006-01-02")
		if rows, err := a.store.ListUsageDaily(today, today, pluginID); err == nil {
			for _, row := range rows {
				todayUsed += row.Credits
				todayRequests += row.Requests
			}
		}
		catalogRows, err := a.store.ListModelCatalog(pluginID)
		if err != nil {
			return nil, err
		}
		for _, entry := range catalogRows {
			catalog = append(catalog, catalogEntryPayload(entry))
		}
	}

	return map[string]any{
		"plugin_id":        pluginID,
		"name":             strings.TrimSpace(st.Manifest.Name),
		"api_level":        st.Manifest.APILevel,
		"upstream_enabled": settings.UpstreamEnabled,
		"credits": map[string]any{
			"total":          totalRemaining,
			"healthy":        healthy,
			"today_used":     todayUsed,
			"today_requests": todayRequests,
			"pool_size":      len(accounts),
		},
		"settings":   poolSettingsPayload(settings),
		"members":    memberRows,
		"catalog":    catalog,
		"model_sync": a.modelSyncSummary(),
	}, nil
}

func catalogEntryPayload(entry model.ModelCatalogEntry) map[string]any {
	return map[string]any{
		"model_id":       entry.ModelID,
		"display_name":   entry.DisplayName,
		"vendor":         entry.Vendor,
		"multiplier":     entry.Multiplier,
		"context_length": entry.ContextLength,
		"vision":         entry.Vision,
		"free":           entry.Free,
		"enabled":        entry.Enabled,
		"thinking":       entry.Thinking,
		"synced_at":      entry.SyncedAt,
	}
}

func (a *App) savePoolSettings(pluginID string, update webui.PoolSettingsUpdate) (any, error) {
	if _, err := a.installedPlugin(pluginID); err != nil {
		return nil, err
	}
	if a.store == nil {
		return nil, errors.New("app: store is not available")
	}
	settings, err := a.effectiveSettings(pluginID)
	if err != nil {
		return nil, err
	}
	if update.UpstreamEnabled != nil {
		settings.UpstreamEnabled = *update.UpstreamEnabled
	}
	if update.AccountConcurrency != nil {
		settings.AccountConcurrency = *update.AccountConcurrency
	}
	if update.PoolStickyTTL != nil {
		settings.PoolStickyTTL = *update.PoolStickyTTL
	}
	if update.ConvStickyTTL != nil {
		settings.ConvStickyTTL = *update.ConvStickyTTL
	}
	if update.HedgeMS != nil {
		settings.HedgeMS = *update.HedgeMS
	}
	if update.DefaultReasoning != nil {
		settings.DefaultReasoning = strings.TrimSpace(*update.DefaultReasoning)
	}
	if update.BackgroundDowngrade != nil {
		settings.BackgroundDowngrade = *update.BackgroundDowngrade
	}
	if update.LongctxDowngrade != nil {
		settings.LongctxDowngrade = *update.LongctxDowngrade
	}
	if update.ToolExec != nil {
		settings.ToolExec = *update.ToolExec
	}
	if update.ModelSyncTime != nil {
		settings.ModelSyncTime = strings.TrimSpace(*update.ModelSyncTime)
	}
	if err := validateSchedulerSettings(settings); err != nil {
		return nil, err
	}
	if err := a.store.UpsertPoolSettings(settings); err != nil {
		return nil, err
	}
	a.bumpSchedulerVersion()
	a.writeAudit(model.AuditPool, "settings", pluginID, "ok")
	return a.poolDetail(pluginID)
}

func (a *App) savePoolMembers(pluginID string, items []string) (any, error) {
	if _, err := a.installedPlugin(pluginID); err != nil {
		return nil, err
	}
	if a.store == nil {
		return nil, errors.New("app: store is not available")
	}
	refs := make([]model.AccountRef, 0, len(items))
	for _, item := range items {
		accountID := strings.TrimSpace(item)
		if accountID == "" {
			continue
		}
		refs = append(refs, model.AccountRef{PluginID: pluginID, AccountID: accountID})
	}
	if err := a.store.ReplacePoolMembers(pluginID, refs); err != nil {
		return nil, err
	}
	a.bumpSchedulerVersion()
	a.writeAudit(model.AuditPool, "members", pluginID, fmt.Sprintf("%d members", len(refs)))
	return a.poolDetail(pluginID)
}

// setPoolModelEnabled flips one model's "接入" switch and invalidates the
// scheduler's model cache so the next request sees it.
func (a *App) setPoolModelEnabled(pluginID, modelID string, enabled bool) (any, error) {
	if _, err := a.installedPlugin(pluginID); err != nil {
		return nil, err
	}
	if a.store == nil {
		return nil, errors.New("app: store is not available")
	}
	if err := a.store.SetModelCatalogEnabled(pluginID, modelID, enabled); err != nil {
		return nil, err
	}
	a.snapMu.Lock()
	delete(a.snapModels, pluginID)
	a.snapMu.Unlock()
	a.bumpSchedulerVersion()
	result := "off"
	if enabled {
		result = "on"
	}
	a.writeAudit(model.AuditPool, "model_"+result, pluginID+"/"+modelID, "ok")
	return a.poolCatalog(pluginID)
}

// syncPoolCatalog fetches /v1/models and replaces model_catalog. Offline or
// account-less plugins are skipped silently (§6.6, §9).
func (a *App) syncPoolCatalog(pluginID string) (any, error) {
	st, err := a.installedPlugin(pluginID)
	if err != nil {
		return nil, err
	}
	skipped := func(reason string) (any, error) {
		return map[string]any{
			"ok": true, "skipped": true, "reason": reason,
			"synced": 0, "models": 0,
		}, nil
	}
	if a.store == nil || a.pluginMgr == nil {
		return skipped("plugin services are not available")
	}
	if !st.Running {
		return skipped("plugin is not running")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The catalog is useful even before an account is added (it shows what the
	// plugin can serve and lets the user pick a subset).
	models, err := a.pluginMgr.ModelsFresh(ctx, pluginID)
	if err != nil {
		return skipped("plugin did not return a model list")
	}
	now := time.Now().Unix()
	entries := make([]model.ModelCatalogEntry, 0, len(models))
	for _, info := range models {
		if strings.TrimSpace(info.ID) == "" {
			continue
		}
		entries = append(entries, model.ModelCatalogEntry{
			PluginID:      pluginID,
			ModelID:       info.ID,
			DisplayName:   info.Name,
			Vendor:        info.Vendor,
			Multiplier:    info.Multiplier,
			Thinking:      info.Thinking,
			ContextLength: info.ContextLength,
			Vision:        info.Vision,
			Free:          info.Free,
			SyncedAt:      now,
		})
	}
	if err := a.store.ReplaceModelCatalog(pluginID, entries); err != nil {
		return nil, err
	}
	a.snapMu.Lock()
	delete(a.snapModels, pluginID)
	a.snapMu.Unlock()
	a.bumpSchedulerVersion()
	a.writeAudit(model.AuditPool, "sync", pluginID, fmt.Sprintf("%d models", len(entries)))
	return map[string]any{
		"ok": true, "skipped": false,
		"synced": len(entries), "models": len(entries), "synced_at": now,
	}, nil
}

func (a *App) poolCatalog(pluginID string) (any, error) {
	if _, err := a.installedPlugin(pluginID); err != nil {
		return nil, err
	}
	catalog := []map[string]any{}
	if a.store != nil {
		entries, err := a.store.ListModelCatalog(pluginID)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			catalog = append(catalog, catalogEntryPayload(entry))
		}
	}
	return map[string]any{"plugin_id": pluginID, "catalog": catalog}, nil
}
