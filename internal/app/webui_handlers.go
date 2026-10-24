package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"aiworkgate/internal/config"
	"aiworkgate/internal/egress"
	"aiworkgate/internal/gateway"
	"aiworkgate/internal/logging"
	"aiworkgate/internal/model"
	"aiworkgate/internal/notify"
	"aiworkgate/internal/plugins"
	"aiworkgate/internal/rules"
	"aiworkgate/internal/secrets"
	"aiworkgate/internal/store"
	"aiworkgate/internal/usage"
	"aiworkgate/internal/webui"
	"aiworkgate/internal/winutil"
)

// webOptions wires the admin API to the host state.
func (a *App) webOptions() webui.Options {
	return webui.Options{
		// SessionToken is the launch token minted in New. The admin cookie is
		// this value, so the tray-opened ///?t=<token> URL is what earns it.
		SessionToken: a.adminToken,
		// RecordAction turns one state-changing admin API call into an audit
		// row. The UI has no per-button instrumentation: every button that
		// changes something issues an /api request, so this is what makes
		// "what did the user click" recoverable from the timeline.
		RecordAction: func(entry model.AuditEntry) error {
			if a.store == nil {
				return nil
			}
			_, err := a.store.AppendAudit(entry)
			return err
		},

		Status:         a.status,
		GetSettings:    a.getSettings,
		SaveSettings:   a.saveSettings,
		AKeyConfigured: a.aKeyConfigured,
		SetAKey:        a.setAKey,
		SetLANPassword: a.setLANPassword,

		Plugins:       a.listPlugins,
		PluginMeta:    a.pluginMeta,
		InstallPlugin: a.installPlugin,
		PluginAction:  a.pluginAction,

		ModelOverrides:      a.store.GetModelOverrides,
		PutModelOverride:    a.store.PutModelOverride,
		DeleteModelOverride: a.store.DeleteModelOverride,

		Accounts:   a.accounts,
		Checkins:   a.checkins,
		CheckinAll: a.checkinAll,

		SetAccountEgress: a.auditedSetAccountEgress,
		ResetIdentity:    a.resetIdentity,

		AccountLoginStart:  a.accountLoginStart,
		AccountLoginPoll:   a.accountLoginPoll,
		AccountLoginSubmit: a.accountLoginSubmit,
		AccountLoginCancel: a.accountLoginCancel,
		AccountPatch:       a.accountPatch,
		AccountDelete:      a.accountDelete,
		AccountRefresh:     a.accountRefresh,

		AccountCheckin:         a.accountCheckin,
		AccountRename:          a.accountRename,
		AccountBulk:            a.accountsBulk,
		AccountSave:            a.accountSave,
		AccountIdentityDetails: a.accountIdentityDetails,
		AccountLoginState:      a.accountLoginStates,

		Groups:      a.listGroups,
		CreateGroup: a.createGroup,

		RulePacks:      a.rulePacks,
		SaveRulePack:   a.saveRulePack,
		DeleteRulePack: a.deleteRulePack,
		ResetRulePack:  a.resetRulePack,
		ReloadRules:    a.reloadRules,

		Proxies:     a.listProxies,
		SaveProxy:   a.saveProxy,
		DeleteProxy: a.deleteProxy,
		CheckProxy:  a.checkProxy,

		RequestLogs: a.requestLogs,
		RuntimeLogs: a.runtimeLogs,

		UsageSummary:  a.usageSummary,
		UsageCredits:  a.usageCredits,
		UsageExpiry:   a.usageExpiry,
		UsageSessions: a.usageSessionsPage,
		UsageExport:   a.usageExport,
		UsageLatency:  a.usageLatency,
		UsageKeyUsage: a.usageKeyUsage,

		UsageModels:       a.usageModels,
		UsageModelLatency: a.usageModelLatency,

		LogTimeline: a.logTimeline,
		LogBody:     a.logBody,
		LogSeed:     a.logSeed,
		LogPoll:     a.logPoll,
		ProxyLogs:   a.proxyLogs,
		LogRaw:      a.logRaw,

		// Debug mode shows which plugin/account an admin action touched;
		// without it the action log keeps its "{id}" template.
		DebugActionIDs: func() bool { return a.configSnapshot().Logging.DebugBody },

		Keys:      a.listKeys,
		CreateKey: a.createKey,
		DeleteKey: a.deleteKey,

		CreateKeySpec: a.createKeySpec,
		UpdateKey:     a.updateKey,

		CustomModels:      a.listCustomModels,
		CreateCustomModel: a.createCustomModel,
		UpdateCustomModel: a.updateCustomModel,
		DeleteCustomModel: a.deleteCustomModel,
		TestCustomModel:   a.testCustomModel,

		CustomConnections:            a.listCustomConnections,
		CreateCustomConnection:       a.createCustomConnection,
		UpdateCustomConnection:       a.updateCustomConnection,
		DeleteCustomConnection:       a.deleteCustomConnection,
		TestCustomConnection:         a.testCustomConnection,
		FetchCustomConnectionModels:  a.fetchCustomConnectionModels,
		ImportCustomConnectionModels: a.importCustomConnectionModels,

		ProbeConnectivity: a.probeConnectivity,
		ProbeHistory:      a.listConnectivityProbes,

		SchedulerOverview:     a.schedulerOverview,
		SaveSchedulerGlobal:   a.saveSchedulerGlobal,
		SaveSchedulerPool:     a.saveSchedulerPool,
		ApplySchedulerPreset:  a.applySchedulerPreset,
		SchedulerModels:       a.schedulerModels,
		SaveSchedulerModel:    a.saveSchedulerModel,
		DeleteSchedulerModel:  a.deleteSchedulerModel,
		SchedulerExplain:      a.schedulerExplain,
		SchedulerSticky:       a.schedulerSticky,
		DeleteSchedulerSticky: a.deleteSchedulerSticky,

		PoolDetail:          a.poolDetail,
		SavePoolSettings:    a.savePoolSettings,
		SavePoolMembers:     a.savePoolMembers,
		SyncPoolCatalog:     a.syncPoolCatalog,
		PoolCatalog:         a.poolCatalog,
		SetPoolModelEnabled: a.setPoolModelEnabled,

		GetPluginSettings:  a.getPluginSettings,
		SavePluginSettings: a.savePluginSettings,

		GetNotify:  a.notifyStatus,
		SaveNotify: a.saveNotifySettings,
		TestNotify: a.testNotify,
	}
}

// status builds GET /api/status.
func (a *App) status() model.HostStatus {
	var plugins []model.PluginStatus
	if a.pluginMgr != nil {
		plugins = a.pluginMgr.List()
	}
	running := 0
	for _, p := range plugins {
		if p.Running {
			running++
		}
	}
	configured := false
	if a.secrets != nil {
		_, configured = a.secrets.GetAAPIKey()
	}
	state, detail, aPort, aRunning := a.aStatusProbe()
	egressAssigned, egressFailed := a.egressFailureCounts(plugins)
	return model.HostStatus{
		Version:        Version,
		Port:           a.port,
		DataDir:        a.dataDir,
		AConfigured:    configured,
		ARunning:       aRunning,
		APort:          aPort,
		AState:         state,
		ADetail:        detail,
		PluginsRunning: running,
		PluginsTotal:   len(plugins),
		EgressAssigned: egressAssigned,
		EgressFailed:   egressFailed,
	}
}

// egressFailureCounts reads stored assignments only — /api/status polls every
// few seconds and must not fan out to plugins. Verdicts come from the cached
// health lookup.
func (a *App) egressFailureCounts(plugins []model.PluginStatus) (assigned, failed int) {
	if a.store == nil || a.egressHealth == nil {
		return 0, 0
	}
	for _, p := range plugins {
		id := strings.TrimSpace(p.Manifest.ID)
		if id == "" {
			continue
		}
		rows, err := a.store.ListAccountEgress(id)
		if err != nil {
			continue
		}
		for _, row := range rows {
			if strings.ToLower(strings.TrimSpace(row.Mode)) != model.EgressProxy {
				continue
			}
			assigned++
			if a.egressHealth.Lookup(id, row.AccountID).Dead() {
				failed++
			}
		}
	}
	return assigned, failed
}

// notifyStatus builds GET /api/notify. The webhook URL is reported only as
// url_set: it is a credential and must never cross the admin API.
func (a *App) notifyStatus() (webui.NotifyStatus, error) {
	cfg := a.configSnapshot().Notify
	status := webui.NotifyStatus{
		Enabled:            cfg.Enabled,
		Channel:            cfg.Channel,
		Events:             append([]string{}, cfg.Events...),
		MinIntervalSeconds: cfg.MinIntervalSeconds,
	}
	if a.secrets != nil {
		if stored, ok := a.secrets.GetExtra(notify.URLSecretKey); ok {
			status.URLSet = strings.TrimSpace(stored) != ""
		}
	}
	if a.notify != nil {
		lastSent, lastError := a.notify.State()
		if !lastSent.IsZero() {
			status.LastSentAt = lastSent.Unix()
		}
		status.LastError = lastError
	}
	return status, nil
}

// saveNotifySettings applies a partial update. The config half goes through
// saveSettings, the existing save/reload path, so the in-memory copy and the
// dispatcher policy change together; the URL half goes to the DPAPI store
// under notify.url and never touches config.json.
func (a *App) saveNotifySettings(update webui.NotifyUpdate) (webui.NotifyStatus, error) {
	next := a.configSnapshot()
	if update.Enabled != nil {
		next.Notify.Enabled = *update.Enabled
	}
	if update.Channel != nil {
		next.Notify.Channel = *update.Channel
	}
	if update.Events != nil {
		next.Notify.Events = append([]string(nil), (*update.Events)...)
	}
	if update.MinIntervalSeconds != nil {
		next.Notify.MinIntervalSeconds = *update.MinIntervalSeconds
	}
	next.Normalize()

	var newURL *string
	if update.URL != nil {
		raw := strings.TrimSpace(*update.URL)
		if raw != "" {
			parsed, err := url.Parse(raw)
			if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
				return webui.NotifyStatus{}, fmt.Errorf("%w: 通知地址必须是完整的 http(s) URL", webui.ErrInvalidRequest)
			}
		}
		newURL = &raw
	}

	raw, err := json.Marshal(next)
	if err != nil {
		return webui.NotifyStatus{}, err
	}
	if err := a.saveSettings(json.RawMessage(raw)); err != nil {
		return webui.NotifyStatus{}, err
	}
	if newURL != nil {
		if a.secrets == nil {
			return webui.NotifyStatus{}, errors.New("app: secret store is not available")
		}
		// SetExtra with "" removes the key, which clears the stored URL.
		if err := a.secrets.SetExtra(notify.URLSecretKey, *newURL); err != nil {
			return webui.NotifyStatus{}, err
		}
	}
	return a.notifyStatus()
}

// testNotify sends one event through the configured channel, ignoring the
// master switch (that is the point of a test button).
func (a *App) testNotify(ctx context.Context) error {
	if a.notify == nil {
		return errors.New("通知服务未初始化")
	}
	return a.notify.Test(ctx)
}

// getSettings marshals the live configuration.
func (a *App) getSettings() (json.RawMessage, error) {
	cfg := a.configSnapshot()
	data, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(data), nil
}

// saveSettings persists a new configuration. The in-memory copy is updated
// immediately; the listening port and LAN binding take effect on restart.
func (a *App) saveSettings(raw json.RawMessage) error {
	next := a.configSnapshot()
	if err := json.Unmarshal(raw, &next); err != nil {
		return err
	}
	next.Normalize()
	// Binding to LAN without a password would expose the admin API to the
	// network with nothing in front of it; refuse instead of silently falling
	// back to loopback (startup does fall back, and says so in the log).
	if next.ListenLAN && (a.secrets == nil || !a.secrets.HasLANPassword()) {
		return fmt.Errorf("%w: 开启局域网访问前必须先设置局域网网页密码", webui.ErrInvalidRequest)
	}
	old := a.configSnapshot()
	if err := config.Save(a.dataDir, next); err != nil {
		return err
	}
	a.cfgMu.Lock()
	a.cfg = next
	a.cfgMu.Unlock()

	// The notification policy is read by the dispatcher on every Emit, so a
	// save takes effect immediately; the webhook URL lives in secrets and is
	// fetched lazily by the dispatcher itself.
	if a.notify != nil {
		a.notify.Update(notifySettingsFromConfig(next.Notify))
	}

	if a.apiLog != nil {
		a.apiLog.SetDebug(next.Logging.DebugBody)
	}
	if g, ok := a.gatewayHandler.(interface{ SetDebug(bool) }); ok {
		g.SetDebug(next.Logging.DebugBody)
	}
	if g, ok := a.gatewayHandler.(interface{ SetCorsOrigins([]string) }); ok {
		g.SetCorsOrigins(next.Gateway.CorsAllowOrigins)
	}
	// Direct egress accounting is a listener decision, not a plugin one: apply
	// it before the files are rewritten, so switching it on puts the listener URL
	// into egress.json right away. Running children keep the environment they
	// started with, which is harmless while the switch goes on (no listener means
	// no injection, and the plugin keeps dialling by itself) and fatal when it
	// goes off: SetDirectLogging just closed the ports those children were told to
	// route through. See reconcileDirectEgressChildren.
	if a.egressMgr != nil {
		if err := a.egressMgr.SetDirectLogging(next.Egress.DirectLogging); err != nil {
			a.logger.Warn("app", "cannot apply direct egress logging", map[string]any{"error": err.Error()})
		}
	}
	a.writeAllEgress()
	a.reloadEgressPlugins()
	a.reconcileDirectEgressChildren()
	// Autostart is a registry write; applying it here means the switch means
	// what it says instead of waiting for the next launch.
	if next.Autostart != old.Autostart {
		if next.Autostart {
			if exe, exeErr := os.Executable(); exeErr != nil {
				a.logger.Warn("app", "cannot resolve executable path for autostart", map[string]any{"error": exeErr.Error()})
			} else if err := winutil.SetAutostart(AppName, exe); err != nil {
				a.logger.Warn("app", "cannot register autostart", map[string]any{"error": err.Error()})
			}
		} else if err := winutil.RemoveAutostart(AppName); err != nil {
			a.logger.Warn("app", "cannot remove autostart", map[string]any{"error": err.Error()})
		}
	}
	a.writeAudit(model.AuditConfig, "settings", "config.json", "ok")
	fields := map[string]any{"debug_body": next.Logging.DebugBody, "listen_lan": next.ListenLAN,
		"cors_origins": len(next.Gateway.CorsAllowOrigins)}
	if next.Port != old.Port {
		fields["port"] = next.Port
		fields["port_active"] = a.port
		a.logger.Info("app", "port change saved; restart to apply", fields)
	} else if next.ListenLAN != old.ListenLAN {
		a.logger.Info("app", "listen_lan change saved; restart to rebind", fields)
	} else {
		a.logger.Info("app", "settings saved", fields)
	}
	return nil
}

func (a *App) aKeyConfigured() bool {
	if a.secrets == nil {
		return false
	}
	_, ok := a.secrets.GetAAPIKey()
	return ok
}

func (a *App) setAKey(key string) error {
	if a.secrets == nil {
		return errors.New("app: secrets store is not available")
	}
	if err := a.secrets.SetAAPIKey(key); err != nil {
		return err
	}
	a.logger.Info("app", "A key saved", nil)
	return nil
}

func (a *App) setLANPassword(password string) error {
	if a.secrets == nil {
		return errors.New("app: secrets store is not available")
	}
	if err := a.secrets.SetLANPassword(password); err != nil {
		return err
	}
	if strings.TrimSpace(password) == "" {
		if a.configSnapshot().ListenLAN {
			a.logger.Warn("app", "LAN password cleared while LAN mode is configured; restart binds to loopback", nil)
		}
		a.logger.Info("app", "LAN password cleared", nil)
	} else {
		a.logger.Info("app", "LAN password updated", nil)
	}
	return nil
}

func (a *App) listPlugins() []model.PluginStatus {
	if a.pluginMgr == nil {
		return []model.PluginStatus{}
	}
	return a.pluginMgr.List()
}

// installPlugin installs an uploaded zip and writes the rules files of any
// plugin that appeared as a result. enabled is the user's explicit "start it
// now" choice from the install form; the plugin stays stopped otherwise.
func (a *App) installPlugin(zipPath string, enabled bool) error {
	if a.pluginMgr == nil {
		return errors.New("app: plugin manager is not available")
	}
	before := make(map[string]bool)
	for _, st := range a.pluginMgr.List() {
		before[st.Manifest.ID] = true
	}
	if err := a.pluginMgr.InstallZipOpts(zipPath, plugins.InstallOpts{Enabled: enabled}); err != nil {
		return err
	}
	if a.rulesMgr != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, st := range a.pluginMgr.List() {
			id := strings.TrimSpace(st.Manifest.ID)
			if id == "" || before[id] {
				continue
			}
			if _, err := a.rulesMgr.WriteForPlugin(ctx, id, st.Manifest.Rules.Packs); err != nil {
				a.logger.Warn("app", "cannot write rules for installed plugin", map[string]any{
					"plugin": id, "error": err.Error(),
				})
			}
			a.writeEgressChecked(id, "install")
			if st.Running {
				_ = a.pluginMgr.Reload(ctx, id)
			}
		}
	}
	a.writeAllEgress()
	a.logger.Info("app", "plugin installed", map[string]any{"zip": zipPath})
	a.writeAudit(model.AuditPlugin, "install", filepath.Base(zipPath), "ok")
	return nil
}

// pluginAction dispatches the admin UI's lifecycle buttons and audits the
// outcome (the action name is safe metadata, never a credential).
func (a *App) pluginAction(id, action string) error {
	err := a.pluginActionInner(id, action)
	result := "ok"
	if err != nil {
		result = "fail"
	}
	a.writeAudit(model.AuditPlugin, action, id, result)
	return err
}

// pluginActionInner performs the lifecycle work.
func (a *App) pluginActionInner(id, action string) error {
	if a.pluginMgr == nil {
		return errors.New("app: plugin manager is not available")
	}
	switch action {
	case "enable":
		a.writePluginRules(id)
		a.writeEgressChecked(id, "enable")
		if err := a.pluginMgr.Enable(id); err != nil {
			return err
		}
		a.writeAllEgress()
		return nil
	case "disable":
		return a.pluginMgr.Disable(id)
	case "start":
		a.writePluginRules(id)
		a.writeEgressChecked(id, "start")
		if err := a.pluginMgr.Start(id); err != nil {
			return err
		}
		a.writeAllEgress()
		return nil
	case "stop":
		return a.pluginMgr.Stop(id)
	case "restart":
		if err := a.pluginMgr.Stop(id); err != nil && !errors.Is(err, plugins.ErrNotInstalled) {
			return err
		}
		a.writePluginRules(id)
		a.writeEgressChecked(id, "restart")
		if err := a.pluginMgr.Start(id); err != nil {
			return err
		}
		a.writeAllEgress()
		return nil
	case "reload":
		st, ok := a.pluginMgr.Get(id)
		if !ok {
			return fmt.Errorf("%w: %s", plugins.ErrNotInstalled, id)
		}
		if !st.Running {
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return a.pluginMgr.Reload(ctx, id)
	case "uninstall":
		if err := a.pluginMgr.Uninstall(id, false); err != nil {
			return err
		}
		a.logger.Info("app", "plugin uninstalled (data kept)", map[string]any{"plugin": id})
		return nil
	default:
		return fmt.Errorf("app: unknown plugin action %q", action)
	}
}

// writePluginRules refreshes one plugin's merged rules file before it starts.
func (a *App) writePluginRules(id string) {
	if a.rulesMgr == nil || a.pluginMgr == nil {
		return
	}
	st, ok := a.pluginMgr.Get(id)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := a.rulesMgr.WriteForPlugin(ctx, id, st.Manifest.Rules.Packs); err != nil {
		a.logger.Warn("app", "cannot write rules for plugin", map[string]any{
			"plugin": id, "error": err.Error(),
		})
	}
}

// ---------------------------------------------------------------- plugin settings (GAP 1)

// isValidCheckinTime reports whether value is a strict HH:MM clock.
func isValidCheckinTime(value string) bool {
	if len(value) != 5 || value[2] != ':' {
		return false
	}
	h, err1 := strconv.Atoi(value[0:2])
	m, err2 := strconv.Atoi(value[3:5])
	if err1 != nil || err2 != nil {
		return false
	}
	if value[0] < '0' || value[0] > '2' {
		return false
	}
	return h >= 0 && h <= 23 && m >= 0 && m <= 59
}

// effectiveStartupMode resolves the mode shown as "当前生效" for the settings
// dialog: an explicit per-plugin mode wins, otherwise the manifest startup mode,
// otherwise the global plugins.startup_mode default.
func effectiveStartupMode(st model.PluginStatus, override string, cfg config.Config) string {
	if override == model.StartupAlways || override == model.StartupOnDemand {
		return override
	}
	switch strings.TrimSpace(st.Manifest.Startup.Mode) {
	case model.StartupAlways, model.StartupOnDemand:
		return strings.TrimSpace(st.Manifest.Startup.Mode)
	}
	if cfg.Plugins.StartupMode == model.StartupOnDemand {
		return model.StartupOnDemand
	}
	return model.StartupAlways
}

// effectiveCheckinTime mirrors plugins.deriveSettings for the read-only hint.
func effectiveCheckinTime(st model.PluginStatus, override string, cfg config.Config) string {
	if isValidCheckinTime(override) {
		return override
	}
	if t := strings.TrimSpace(st.Manifest.Checkin.DefaultTime); isValidCheckinTime(t) {
		return t
	}
	return cfg.Checkin.DefaultTime
}

// pluginMetaTimeout bounds GET /meta: the call only reads state from the
// already-running process, so a plugin needing longer is stuck rather than
// busy.
const pluginMetaTimeout = 5 * time.Second

// pluginMeta implements GET /api/plugins/{id}/meta. The running state is
// checked first so a stopped plugin is reported instead of cold-started by a
// dashboard poll.
func (a *App) pluginMeta(id string) (any, error) {
	id = strings.TrimSpace(id)
	if a.pluginMgr == nil {
		return nil, errors.New("app: plugin manager is not available")
	}
	st, ok := a.pluginMgr.Get(id)
	if !ok {
		return nil, fmt.Errorf("%w: %s", webui.ErrPluginNotFound, id)
	}
	if !st.Running {
		return nil, fmt.Errorf("%w: %s", webui.ErrPluginNotRunning, id)
	}
	ctx, cancel := context.WithTimeout(context.Background(), pluginMetaTimeout)
	defer cancel()
	meta, err := a.pluginMgr.Meta(ctx, id)
	if err != nil {
		return nil, pluginCallError(id, err)
	}
	return meta, nil
}

// getPluginSettings builds GET /api/plugins/{id}/settings.
func (a *App) getPluginSettings(id string) (any, error) {
	id = strings.TrimSpace(id)
	if a.pluginMgr == nil {
		return nil, errors.New("app: plugin manager is not available")
	}
	st, ok := a.pluginMgr.Get(id)
	if !ok {
		return nil, fmt.Errorf("%w: %s", webui.ErrPluginNotFound, id)
	}
	cfg := a.configSnapshot()

	var (
		view    store.PluginSettingsView
		hasRow  bool
		enabled = st.Enabled
	)
	if a.store != nil {
		v, found, err := a.store.GetPluginSettingsView(id)
		if err != nil {
			return nil, err
		}
		view, hasRow = v, found
	}

	// Editable override fields (empty / 0 / "default" mean 跟随全局).
	modeOverride := ""
	if hasRow && (view.Mode == model.StartupAlways || view.Mode == model.StartupOnDemand) {
		modeOverride = view.Mode
	}
	concurrencyOverride := 0
	if hasRow {
		concurrencyOverride = view.Concurrency
	}
	checkinOverride := ""
	if hasRow {
		checkinOverride = view.CheckinTime
	}
	strictState := "default"
	if hasRow && view.StrictSet {
		if view.Strict {
			strictState = "on"
		} else {
			strictState = "off"
		}
	}

	// Effective values actually used by the runtime.
	strictEffective := st.Manifest.Egress.Strict || cfg.Plugins.StrictEgress
	if hasRow && view.StrictSet {
		strictEffective = view.Strict
	}

	return map[string]any{
		"plugin_id":    id,
		"enabled":      enabled,
		"mode":         modeOverride,
		"concurrency":  concurrencyOverride,
		"checkin_time": checkinOverride,
		"strict":       strictState,
		"effective": map[string]any{
			"mode":         effectiveStartupMode(st, modeOverride, cfg),
			"concurrency":  a.pluginMgr.Concurrency(id),
			"checkin_time": effectiveCheckinTime(st, checkinOverride, cfg),
			"strict":       strictEffective,
		},
		"global": map[string]any{
			"concurrency":          cfg.Plugins.Concurrency,
			"startup_mode":         cfg.Plugins.StartupMode,
			"strict_egress":        cfg.Plugins.StrictEgress,
			"checkin_default_time": cfg.Checkin.DefaultTime,
		},
	}, nil
}

// savePluginSettings writes PUT /api/plugins/{id}/settings. The persisted row
// stores NULL for concurrency 0 and strict "default" so those fall back to the
// global value; only explicitly-set overrides win. An enabled change drives the
// start/stop lifecycle through the existing action path, then the authoritative
// row is written so 跟随全局 markers survive. egress is rewritten and running
// plugins are reloaded so strict takes effect without a host restart.
func (a *App) savePluginSettings(id string, update webui.PluginSettingsUpdate) (any, error) {
	id = strings.TrimSpace(id)
	if a.pluginMgr == nil {
		return nil, errors.New("app: plugin manager is not available")
	}
	if a.store == nil {
		return nil, errors.New("app: store is not available")
	}
	st, ok := a.pluginMgr.Get(id)
	if !ok {
		return nil, fmt.Errorf("%w: %s", webui.ErrPluginNotFound, id)
	}

	enabled := st.Enabled
	if update.Enabled != nil {
		enabled = *update.Enabled
	}
	mode := strings.TrimSpace(update.Mode)
	if mode != "" && mode != model.StartupAlways && mode != model.StartupOnDemand {
		return nil, fmt.Errorf("%w: invalid mode %q", webui.ErrInvalidRequest, update.Mode)
	}
	if update.Concurrency < 0 || update.Concurrency > config.MaxConcurr {
		return nil, fmt.Errorf("%w: concurrency %d out of range 0..%d",
			webui.ErrInvalidRequest, update.Concurrency, config.MaxConcurr)
	}
	checkin := strings.TrimSpace(update.CheckinTime)
	if checkin != "" && !isValidCheckinTime(checkin) {
		return nil, fmt.Errorf("%w: checkin_time must be HH:MM", webui.ErrInvalidRequest)
	}
	var strictOverride *bool
	switch update.Strict {
	case "on":
		value := true
		strictOverride = &value
	case "off":
		value := false
		strictOverride = &value
	case "", "default":
		strictOverride = nil
	default:
		return nil, fmt.Errorf("%w: strict must be on, off or default", webui.ErrInvalidRequest)
	}

	// Keep the check-in calendar anchored to the original install date.
	var installedAt int64
	if v, found, err := a.store.GetPluginSettingsView(id); err == nil && found {
		installedAt = v.InstalledAt
	}

	if enabled != st.Enabled {
		if actionErr := a.pluginAction(id, map[bool]string{true: "enable", false: "disable"}[enabled]); actionErr != nil {
			a.logger.Warn("app", "plugin settings enable/disable failed", map[string]any{
				"plugin": id, "enabled": enabled, "error": actionErr.Error(),
			})
		}
	}
	if err := a.store.UpdatePluginSettings(id, enabled, mode, update.Concurrency, checkin, strictOverride, installedAt); err != nil {
		return nil, err
	}
	// Apply the strict/egress change live; concurrency is read from the store on
	// every request, and startup mode/enable take effect on the next host start.
	a.writeEgressChecked(id, "settings")
	a.reloadEgressPlugins()
	a.writeAudit(model.AuditPlugin, "settings", id, "ok")
	a.logger.Info("app", "plugin settings saved", map[string]any{
		"plugin": id, "enabled": enabled, "mode": mode,
		"concurrency": update.Concurrency, "checkin_time": checkin, "strict": update.Strict,
	})
	return map[string]any{"ok": true, "plugin_id": id}, nil
}

// accounts returns one plugin's accounts or, with an empty plugin id, every
// plugin's accounts tagged with plugin_id and merged with the host overlay. A
// plugin that cannot answer is served from account_cache with stale=true.
func (a *App) accounts(pluginID string) any {
	if a.pluginMgr == nil {
		return map[string]any{"accounts": []accountEntry{}}
	}
	return a.accountsFrom(a.pluginMgr, pluginID)
}

// accountsFrom builds the merged listing from any account source (the plugin
// manager in production, a fake in tests).
func (a *App) accountsFrom(src accountLister, pluginID string) any {
	if src == nil {
		return map[string]any{"accounts": []accountEntry{}}
	}
	pluginID = strings.TrimSpace(pluginID)
	meta := a.accountMetaMap()

	if pluginID != "" {
		entries, stale, cachedAt, err := a.accountEntriesFor(src, pluginID, meta)
		payload := map[string]any{"accounts": entries}
		if stale {
			payload["stale"] = true
			if cachedAt > 0 {
				payload["cached_at"] = cachedAt
			}
		}
		if err != nil {
			payload["error"] = err.Error()
		}
		return payload
	}

	out := []accountEntry{}
	for _, st := range src.List() {
		id := strings.TrimSpace(st.Manifest.ID)
		if id == "" || !st.Enabled {
			continue
		}
		entries, stale, _, err := a.accountEntriesFor(src, id, meta)
		if err != nil {
			a.logger.Warn("app", "cannot read accounts", map[string]any{
				"plugin": id, "error": err.Error(), "stale": stale,
			})
		}
		out = append(out, entries...)
	}
	return map[string]any{"accounts": out}
}

// accountEntriesFor reads one plugin's accounts, refreshes the cache on
// success and falls back to the cached snapshot on any failure.
func (a *App) accountEntriesFor(src accountLister, pluginID string, meta map[string]model.AccountMeta) (entries []accountEntry, stale bool, cachedAt int64, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), accountLoginTimeout)
	defer cancel()
	accounts, callErr := src.Accounts(ctx, pluginID)
	if callErr == nil {
		a.cacheAccounts(pluginID, accounts)
		return mergeAccountEntries(pluginID, accounts, a.accountMergeOptions(pluginID, meta, false)), false, 0, nil
	}
	cached, cachedAt := a.cachedAccounts(pluginID)
	if len(cached) > 0 {
		return mergeAccountEntries(pluginID, cached, a.accountMergeOptions(pluginID, meta, true)), true, cachedAt, callErr
	}
	return []accountEntry{}, false, 0, callErr
}

// accountMergeOptions snapshots the overlay, egress assignments and the cached
// egress verdicts for one listing.
func (a *App) accountMergeOptions(pluginID string, meta map[string]model.AccountMeta, stale bool) accountMergeOptions {
	return accountMergeOptions{
		Meta:   meta,
		Egress: a.accountEgress(pluginID),
		Health: a.accountEgressHealth,
		Stale:  stale,
		Now:    time.Now().Unix(),
	}
}

// accountEgressHealth returns the evidence for an account with an explicit
// proxy assignment. "follow default" and "direct" accounts have no per-account
// exit to judge.
func (a *App) accountEgressHealth(pluginID, accountID string) (egress.Evidence, bool) {
	if a.egressHealth == nil {
		return egress.Evidence{}, false
	}
	ev := a.egressHealth.Lookup(pluginID, accountID)
	if ev.Mode != model.EgressProxy {
		return ev, false
	}
	return ev, true
}

// cacheAccounts stores the latest plugin snapshot; a cache failure only
// degrades offline browsing and is logged.
func (a *App) cacheAccounts(pluginID string, accounts []model.Account) {
	if a.store == nil {
		return
	}
	if err := a.store.ReplaceAccountCache(pluginID, accounts); err != nil {
		a.logger.Warn("app", "cannot cache accounts", map[string]any{
			"plugin": pluginID, "error": err.Error(),
		})
	}
}

// cachedAccounts returns the last snapshot written by the plugin.
func (a *App) cachedAccounts(pluginID string) ([]model.Account, int64) {
	if a.store == nil {
		return nil, 0
	}
	accounts, updatedAt, err := a.store.ListAccountCache(pluginID)
	if err != nil {
		a.logger.Warn("app", "cannot read account cache", map[string]any{
			"plugin": pluginID, "error": err.Error(),
		})
		return nil, 0
	}
	return accounts, updatedAt
}

// accountMetaMap loads every overlay row once per listing.
func (a *App) accountMetaMap() map[string]model.AccountMeta {
	if a.store == nil {
		return nil
	}
	rows, err := a.store.ListAccountMeta()
	if err != nil {
		a.logger.Warn("app", "cannot read account meta", map[string]any{"error": err.Error()})
		return nil
	}
	return accountMetaMap(rows)
}

// accountEgress loads one plugin's stored egress assignments keyed by account
// id. A nil or failing store yields an empty map so the account list stays
// available.
func (a *App) accountEgress(pluginID string) map[string]model.AccountEgress {
	if a.store == nil || strings.TrimSpace(pluginID) == "" {
		return nil
	}
	rows, err := a.store.ListAccountEgress(pluginID)
	if err != nil {
		a.logger.Warn("app", "cannot read account egress", map[string]any{
			"plugin": pluginID, "error": err.Error(),
		})
		return nil
	}
	return egressByAccount(rows)
}

// egressByAccount indexes assignments by account id; later rows win.
func egressByAccount(rows []model.AccountEgress) map[string]model.AccountEgress {
	if len(rows) == 0 {
		return nil
	}
	out := make(map[string]model.AccountEgress, len(rows))
	for _, row := range rows {
		if row.AccountID == "" {
			continue
		}
		out[row.AccountID] = row
	}
	return out
}

// tagAccounts tags every account with its plugin id and its egress assignment
// (no overlay); kept as the simple building block of the merged view.
func tagAccounts(pluginID string, accounts []model.Account, egress map[string]model.AccountEgress) []accountEntry {
	return mergeAccountEntries(pluginID, accounts, accountMergeOptions{Egress: egress})
}

// ---------------------------------------------------------------- account login (M10)

// Account-level plugin calls. Login can block briefly while a plugin launches a
// browser or probes the local client, so it shares the generous timeout;
// account mutations are expected to answer quickly.
const (
	accountLoginTimeout    = 60 * time.Second
	accountMutationTimeout = 15 * time.Second
)

// accountPlugin validates that pluginID names an installed plugin.
func (a *App) accountPlugin(pluginID string) error {
	if a.pluginMgr == nil {
		return errors.New("app: plugin manager is not available")
	}
	pluginID = strings.TrimSpace(pluginID)
	if _, ok := a.pluginMgr.Get(pluginID); !ok {
		return fmt.Errorf("%w: %s", webui.ErrPluginNotFound, pluginID)
	}
	return nil
}

// pluginCallError maps the plugin runtime sentinels onto the admin API status
// codes without exposing anything but the plugin id.
func pluginCallError(pluginID string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, plugins.ErrNotInstalled):
		return fmt.Errorf("%w: %s", webui.ErrPluginNotFound, pluginID)
	case errors.Is(err, plugins.ErrNotRunning), errors.Is(err, plugins.ErrDisabled):
		return fmt.Errorf("%w: %s", webui.ErrPluginNotRunning, pluginID)
	default:
		return err
	}
}

// accountLoginStart opens a login session; method may be empty so the plugin
// chooses its default flow.
func (a *App) accountLoginStart(pluginID, method string) (model.LoginStart, error) {
	pluginID = strings.TrimSpace(pluginID)
	method = strings.TrimSpace(method)
	if err := a.accountPlugin(pluginID); err != nil {
		return model.LoginStart{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), accountLoginTimeout)
	defer cancel()
	start, err := a.pluginMgr.LoginStart(ctx, pluginID, method)
	if err != nil {
		a.logger.Warn("app", "account login start failed", map[string]any{
			"plugin": pluginID, "method": method, "error": err.Error(),
		})
		return model.LoginStart{}, pluginCallError(pluginID, err)
	}
	a.logger.Info("app", "account login started", map[string]any{
		"plugin": pluginID, "method": method, "session": start.SessionID, "status": start.Status,
	})
	return start, nil
}

// accountLoginPoll reads one login session. Polling is silent unless it fails
// or completes, so the runtime log is not flooded every two seconds.
func (a *App) accountLoginPoll(pluginID, sessionID string) (model.LoginStatus, error) {
	pluginID = strings.TrimSpace(pluginID)
	if err := a.accountPlugin(pluginID); err != nil {
		return model.LoginStatus{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), accountLoginTimeout)
	defer cancel()
	status, err := a.pluginMgr.LoginPoll(ctx, pluginID, sessionID)
	if err != nil {
		a.logger.Warn("app", "account login poll failed", map[string]any{
			"plugin": pluginID, "session": sessionID, "error": err.Error(),
		})
		return model.LoginStatus{}, pluginCallError(pluginID, err)
	}
	switch status.Status {
	case model.LoginSuccess, model.LoginFailed:
		a.logger.Info("app", "account login finished", map[string]any{
			"plugin": pluginID, "session": sessionID, "status": status.Status,
		})
	}
	a.clearHealthAfterLogin(pluginID, status)
	return status, nil
}

// accountLoginSubmit forwards user-supplied form values to the plugin. The
// values themselves are never logged.
func (a *App) accountLoginSubmit(pluginID, sessionID string, values map[string]string) (model.LoginStatus, error) {
	pluginID = strings.TrimSpace(pluginID)
	if err := a.accountPlugin(pluginID); err != nil {
		return model.LoginStatus{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), accountLoginTimeout)
	defer cancel()
	status, err := a.pluginMgr.LoginSubmit(ctx, pluginID, sessionID, values)
	if err != nil {
		a.logger.Warn("app", "account login submit failed", map[string]any{
			"plugin": pluginID, "session": sessionID, "fields": len(values), "error": err.Error(),
		})
		return model.LoginStatus{}, pluginCallError(pluginID, err)
	}
	a.logger.Info("app", "account login submitted", map[string]any{
		"plugin": pluginID, "session": sessionID, "fields": len(values), "status": status.Status,
	})
	a.clearHealthAfterLogin(pluginID, status)
	return status, nil
}

// clearHealthAfterLogin drops a health cooldown after a successful login. A
// session_dead or banned account is parked forever "until the next login"
// (plan 2026-10-21 §4); this is the login that makes it eligible again.
func (a *App) clearHealthAfterLogin(pluginID string, status model.LoginStatus) {
	if a == nil || a.store == nil || status.Status != model.LoginSuccess {
		return
	}
	accountID := strings.TrimSpace(status.AccountID)
	if accountID == "" {
		return
	}
	pluginID = strings.TrimSpace(pluginID)
	if a.sched != nil {
		// The scheduler also holds an in-memory "cooldown already reported"
		// guard; clearing only the database would leave it parked.
		a.sched.ClearAccountHealth(pluginID, accountID)
	} else if err := a.store.ClearAccountHealth(pluginID, accountID); err != nil {
		a.logger.Warn("app", "cannot clear account health after login", map[string]any{
			"plugin": pluginID, "account": accountID, "error": err.Error(),
		})
		return
	}
	a.logger.Info("app", "account health cleared after login", map[string]any{
		"plugin": pluginID, "account": accountID,
	})
}

// accountLoginCancel abandons a login session; the plugin cleanup is optional.
func (a *App) accountLoginCancel(pluginID, sessionID string) error {
	pluginID = strings.TrimSpace(pluginID)
	if err := a.accountPlugin(pluginID); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), accountMutationTimeout)
	defer cancel()
	if err := a.pluginMgr.LoginCancel(ctx, pluginID, sessionID); err != nil {
		a.logger.Warn("app", "account login cancel failed", map[string]any{
			"plugin": pluginID, "session": sessionID, "error": err.Error(),
		})
		return pluginCallError(pluginID, err)
	}
	a.logger.Info("app", "account login cancelled", map[string]any{
		"plugin": pluginID, "session": sessionID,
	})
	return nil
}

// accountPatch applies a partial update. Only the patch keys are logged.
func (a *App) accountPatch(pluginID, accountID string, patch map[string]any) error {
	pluginID = strings.TrimSpace(pluginID)
	accountID = strings.TrimSpace(accountID)
	if err := a.accountPlugin(pluginID); err != nil {
		return err
	}
	keys := make([]string, 0, len(patch))
	for key := range patch {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	ctx, cancel := context.WithTimeout(context.Background(), accountMutationTimeout)
	defer cancel()
	if err := a.pluginMgr.PatchAccount(ctx, pluginID, accountID, patch); err != nil {
		a.logger.Warn("app", "account patch failed", map[string]any{
			"plugin": pluginID, "account": accountID, "keys": keys, "error": err.Error(),
		})
		a.writeAudit(model.AuditAccount, "patch", pluginID+"/"+accountID, "fail")
		return pluginCallError(pluginID, err)
	}
	a.logger.Info("app", "account patched", map[string]any{
		"plugin": pluginID, "account": accountID, "keys": keys,
	})
	a.writeAudit(model.AuditAccount, "patch", pluginID+"/"+accountID, "ok")
	return nil
}

// accountDelete removes one account and its credentials from the plugin.
func (a *App) accountDelete(pluginID, accountID string) error {
	pluginID = strings.TrimSpace(pluginID)
	accountID = strings.TrimSpace(accountID)
	if err := a.accountPlugin(pluginID); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), accountMutationTimeout)
	defer cancel()
	if err := a.pluginMgr.DeleteAccount(ctx, pluginID, accountID); err != nil {
		a.logger.Warn("app", "account delete failed", map[string]any{
			"plugin": pluginID, "account": accountID, "error": err.Error(),
		})
		a.writeAudit(model.AuditAccount, "delete", pluginID+"/"+accountID, "fail")
		return pluginCallError(pluginID, err)
	}
	// The plugin-owned account is gone: drop the overlay, cache, pool
	// membership, egress assignment and sticky pins pointing at it.
	if a.store != nil {
		if err := a.store.DeleteAccountData(pluginID, accountID); err != nil {
			a.logger.Warn("app", "cannot delete account overlay rows", map[string]any{
				"plugin": pluginID, "account": accountID, "error": err.Error(),
			})
		}
	}
	a.syncEgressAfterAccountRemoval(pluginID)
	a.logger.Info("app", "account deleted", map[string]any{
		"plugin": pluginID, "account": accountID,
	})
	a.writeAudit(model.AuditAccount, "delete", pluginID+"/"+accountID, "ok")
	return nil
}

// syncEgressAfterAccountRemoval rewrites runtime/<id>/egress.json from the
// remaining assignments and makes the running plugin re-read it; otherwise a
// deleted account keeps its proxy rule until the next settings save or restart.
func (a *App) syncEgressAfterAccountRemoval(pluginID string) {
	if a.pluginMgr == nil || a.egressMgr == nil {
		return
	}
	a.writeEgressChecked(pluginID, "account-removal")
	a.reloadEgressPlugins()
}

// accountRefresh asks the plugin to renew one account's credentials.
func (a *App) accountRefresh(pluginID, accountID string) (model.RefreshResult, error) {
	pluginID = strings.TrimSpace(pluginID)
	accountID = strings.TrimSpace(accountID)
	if err := a.accountPlugin(pluginID); err != nil {
		return model.RefreshResult{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), accountLoginTimeout)
	defer cancel()
	result, err := a.pluginMgr.Refresh(ctx, pluginID, accountID)
	if err != nil {
		a.logger.Warn("app", "account refresh failed", map[string]any{
			"plugin": pluginID, "account": accountID, "error": err.Error(),
		})
		a.writeAudit(model.AuditAccount, "refresh", pluginID+"/"+accountID, "fail")
		return model.RefreshResult{}, pluginCallError(pluginID, err)
	}
	a.logger.Info("app", "account refreshed", map[string]any{
		"plugin": pluginID, "account": accountID, "result": result.Result,
	})
	a.writeAudit(model.AuditAccount, "refresh", pluginID+"/"+accountID, result.Result)
	return result, nil
}

// ---------------------------------------------------------------- S1 account center

// maxAliasRunes is the host overlay alias limit (design §5.1).
const maxAliasRunes = 64

// writeAudit records one management operation in audit_log. It only ever
// receives metadata, never credentials.
func (a *App) writeAudit(category, action, target, result string) {
	a.writeAuditDetail(category, action, target, "", result)
}

// writeAuditDetail is writeAudit with the free-form detail column, used when the
// operation has more to tell the admin than one target (which plugins a settings
// save had to restart).
func (a *App) writeAuditDetail(category, action, target, detail, result string) {
	if a.store == nil {
		return
	}
	if _, err := a.store.AppendAudit(model.AuditEntry{
		Category: category, Action: action, Target: target, Detail: detail, Result: result,
	}); err != nil {
		if a.logger != nil {
			a.logger.Warn("app", "cannot append audit entry", map[string]any{
				"category": category, "action": action, "error": err.Error(),
			})
		}
		return
	}
	if a.logger != nil {
		a.logger.Info("app", "audit "+category, map[string]any{
			"action": action, "target": target, "result": result,
		})
	}
}

// auditedSetAccountEgress wraps the egress assignment with an audit row so the
// existing PUT /api/accounts/egress endpoint is covered too.
func (a *App) auditedSetAccountEgress(pluginID, accountID, mode, proxyID string) (bool, string, error) {
	reloaded, warning, err := a.setAccountEgress(pluginID, accountID, mode, proxyID)
	result := "ok"
	if err != nil {
		result = "fail"
	}
	a.writeAudit(model.AuditAccount, "egress", pluginID+"/"+accountID, result)
	return reloaded, warning, err
}

// normalizeCheckinResult folds any unexpected plugin answer into fail so only
// ok/already/fail reaches the store.
func normalizeCheckinResult(result model.CheckinResult) model.CheckinResult {
	switch result.Result {
	case model.CheckinOK, model.CheckinAlready, model.CheckinFail:
		return result
	}
	message := strings.TrimSpace(result.Message)
	if message == "" && result.Result != "" {
		message = "unexpected result: " + result.Result
	}
	result.Result = model.CheckinFail
	result.Message = message
	return result
}

// checkinMessage renders a short human-readable outcome.
func checkinMessage(result model.CheckinResult) string {
	if message := strings.TrimSpace(result.Message); message != "" {
		return message
	}
	if result.CreditsBefore != nil && result.CreditsAfter != nil {
		delta := *result.CreditsAfter - *result.CreditsBefore
		if delta != 0 {
			return fmt.Sprintf("%+.1f", delta)
		}
	}
	return result.Result
}

// recordCheckin upserts today's check-in row for one account.
func (a *App) recordCheckin(pluginID, accountID, label string, result model.CheckinResult) {
	if a.store == nil {
		return
	}
	result = normalizeCheckinResult(result)
	rec := model.CheckinRecord{
		PluginID:      pluginID,
		AccountID:     accountID,
		AccountLabel:  label,
		Date:          time.Now().Format("2006-01-02"),
		Result:        result.Result,
		CreditsBefore: result.CreditsBefore,
		CreditsAfter:  result.CreditsAfter,
		Message:       result.Message,
		At:            time.Now().Unix(),
	}
	if _, err := a.store.UpsertCheckinRecord(rec); err != nil {
		a.logger.Warn("app", "cannot record check-in", map[string]any{
			"plugin": pluginID, "account": accountID, "error": err.Error(),
		})
	}
}

// cachedLabel returns the last label the plugin reported for one account.
func (a *App) cachedLabel(pluginID, accountID string) string {
	accounts, _ := a.cachedAccounts(pluginID)
	for _, acc := range accounts {
		if acc.ID == accountID {
			return acc.Label
		}
	}
	return ""
}

// accountCheckin runs one immediate check-in, records it and audits the call.
// When the scheduler is available the account goes through the same
// cooldown/device-dedup/attribution rules as the scheduled pass; a success is
// therefore also the manual way out of a stale health tuple.
func (a *App) accountCheckin(pluginID, accountID string) (model.CheckinResult, error) {
	pluginID = strings.TrimSpace(pluginID)
	accountID = strings.TrimSpace(accountID)
	if err := a.accountPlugin(pluginID); err != nil {
		return model.CheckinResult{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if a.sched != nil {
		if acc, ok := a.pluginAccount(ctx, pluginID, accountID); ok {
			result := a.sched.CheckinAccountNow(ctx, pluginID, acc)
			a.logger.Info("app", "account check-in finished", map[string]any{
				"plugin": pluginID, "account": accountID, "result": result.Result,
			})
			a.writeAudit(model.AuditAccount, "checkin", pluginID+"/"+accountID, result.Result)
			return result, nil
		}
	}
	result, err := a.pluginMgr.Checkin(ctx, pluginID, accountID)
	if err != nil {
		a.logger.Warn("app", "account check-in failed", map[string]any{
			"plugin": pluginID, "account": accountID, "error": err.Error(),
		})
		// The attempt still belongs on the calendar: record it as fail.
		a.recordCheckin(pluginID, accountID, a.cachedLabel(pluginID, accountID),
			model.CheckinResult{Result: model.CheckinFail, Message: err.Error()})
		a.writeAudit(model.AuditAccount, "checkin", pluginID+"/"+accountID, "fail")
		return model.CheckinResult{}, pluginCallError(pluginID, err)
	}
	result = normalizeCheckinResult(result)
	label := a.cachedLabel(pluginID, accountID)
	a.recordCheckin(pluginID, accountID, label, result)
	a.logger.Info("app", "account check-in finished", map[string]any{
		"plugin": pluginID, "account": accountID, "result": result.Result,
	})
	a.writeAudit(model.AuditAccount, "checkin", pluginID+"/"+accountID, result.Result)
	return result, nil
}

// pluginAccount resolves one account from the plugin's live list. A failure
// simply lets the caller use the direct plugin call instead.
func (a *App) pluginAccount(ctx context.Context, pluginID, accountID string) (model.Account, bool) {
	if a.pluginMgr == nil {
		return model.Account{}, false
	}
	accounts, err := a.pluginMgr.Accounts(ctx, pluginID)
	if err != nil {
		return model.Account{}, false
	}
	for _, acc := range accounts {
		if acc.ID == accountID {
			return acc, true
		}
	}
	return model.Account{}, false
}

// accountRename writes the overlay alias (empty clears it).
func (a *App) accountRename(pluginID, accountID, alias string) error {
	if err := a.accountPlugin(strings.TrimSpace(pluginID)); err != nil {
		return err
	}
	return a.renameAccountAlias(pluginID, accountID, alias)
}

// renameAccountAlias validates and stores one alias override.
func (a *App) renameAccountAlias(pluginID, accountID, alias string) error {
	pluginID = strings.TrimSpace(pluginID)
	accountID = strings.TrimSpace(accountID)
	alias = strings.TrimSpace(alias)
	if a.store == nil {
		return errors.New("app: store is not available")
	}
	if utf8.RuneCountInString(alias) > maxAliasRunes {
		return fmt.Errorf("%w: alias is longer than %d characters", webui.ErrInvalidRequest, maxAliasRunes)
	}
	if err := a.store.SetAccountAlias(pluginID, accountID, alias); err != nil {
		return err
	}
	a.writeAudit(model.AuditAccount, "rename", pluginID+"/"+accountID, "ok")
	a.logger.Info("app", "account renamed", map[string]any{
		"plugin": pluginID, "account": accountID, "alias": alias,
	})
	return nil
}

// accountsBulk runs one immediate action over many accounts. Items are
// executed sequentially and every outcome is reported; a failing item never
// aborts the rest of the batch.
func (a *App) accountsBulk(action string, items []model.AccountRef, params model.BulkParams) []model.BulkResult {
	results := make([]model.BulkResult, 0, len(items))
	okCount := 0
	for _, item := range items {
		result := model.BulkResult{
			PluginID:  strings.TrimSpace(item.PluginID),
			AccountID: strings.TrimSpace(item.AccountID),
		}
		result.OK, result.Message = a.bulkAccountAction(action, result.PluginID, result.AccountID, params)
		if result.OK {
			okCount++
		}
		results = append(results, result)
	}
	a.writeAudit(model.AuditAccount, "bulk_"+action, fmt.Sprintf("%d accounts", len(items)),
		fmt.Sprintf("ok=%d fail=%d", okCount, len(items)-okCount))
	return results
}

// bulkAccountAction executes one bulk item.
func (a *App) bulkAccountAction(action, pluginID, accountID string, params model.BulkParams) (bool, string) {
	if pluginID == "" || accountID == "" {
		return false, "plugin_id and account_id are required"
	}
	if a.pluginMgr == nil {
		return false, "plugin manager is not available"
	}
	if _, ok := a.pluginMgr.Get(pluginID); !ok {
		return false, "plugin not found: " + pluginID
	}

	switch action {
	case model.BulkCheckin:
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if a.sched != nil {
			if acc, ok := a.pluginAccount(ctx, pluginID, accountID); ok {
				result := a.sched.CheckinAccountNow(ctx, pluginID, acc)
				return result.Result == model.CheckinOK || result.Result == model.CheckinAlready, checkinMessage(result)
			}
		}
		result, err := a.pluginMgr.Checkin(ctx, pluginID, accountID)
		if err != nil {
			a.recordCheckin(pluginID, accountID, "",
				model.CheckinResult{Result: model.CheckinFail, Message: err.Error()})
			return false, pluginCallError(pluginID, err).Error()
		}
		result = normalizeCheckinResult(result)
		a.recordCheckin(pluginID, accountID, "", result)
		return result.Result != model.CheckinFail, checkinMessage(result)

	case model.BulkRefresh:
		ctx, cancel := context.WithTimeout(context.Background(), accountLoginTimeout)
		defer cancel()
		result, err := a.pluginMgr.Refresh(ctx, pluginID, accountID)
		if err != nil {
			return false, pluginCallError(pluginID, err).Error()
		}
		ok := result.Result != model.CheckinFail
		return ok, checkinMessage(model.CheckinResult{Result: result.Result, Message: result.Message})

	case model.BulkEnable, model.BulkDisable:
		ctx, cancel := context.WithTimeout(context.Background(), accountMutationTimeout)
		defer cancel()
		patch := map[string]any{"enabled": action == model.BulkEnable}
		if err := a.pluginMgr.PatchAccount(ctx, pluginID, accountID, patch); err != nil {
			return false, pluginCallError(pluginID, err).Error()
		}
		return true, ""

	case model.BulkBindIP:
		if a.store == nil {
			return false, "store is not available"
		}
		_, warning, err := a.setAccountEgress(pluginID, accountID, params.Mode, params.ProxyID)
		if err != nil {
			return false, err.Error()
		}
		return true, warning

	case model.BulkGroup:
		if a.store == nil {
			return false, "store is not available"
		}
		if params.GroupID != "" {
			if _, ok, err := a.store.GetAccountGroup(params.GroupID); err != nil {
				return false, err.Error()
			} else if !ok {
				return false, "unknown group: " + params.GroupID
			}
		}
		if err := a.store.SetAccountGroup(pluginID, accountID, params.GroupID); err != nil {
			return false, err.Error()
		}
		return true, ""

	case model.BulkDelete:
		ctx, cancel := context.WithTimeout(context.Background(), accountMutationTimeout)
		defer cancel()
		if err := a.pluginMgr.DeleteAccount(ctx, pluginID, accountID); err != nil {
			return false, pluginCallError(pluginID, err).Error()
		}
		if a.store != nil {
			if err := a.store.DeleteAccountData(pluginID, accountID); err != nil {
				return false, err.Error()
			}
		}
		return true, ""

	default:
		return false, "unsupported action: " + action
	}
}

// maskIdentityID keeps the first and last two characters of an identifier, or
// fully masks anything too short to be safely partially revealed.
func maskIdentityID(value string) string {
	runes := []rune(value)
	if len(runes) == 0 {
		return ""
	}
	if len(runes) <= 4 {
		return "****"
	}
	return string(runes[:2]) + "****" + string(runes[len(runes)-2:])
}

// accountIdentityDetails reads one account's plugin-reported identity and
// masks it unless the caller explicitly revealed it.
func (a *App) accountIdentityDetails(pluginID, accountID string, reveal bool) (model.IdentityDetails, error) {
	pluginID = strings.TrimSpace(pluginID)
	accountID = strings.TrimSpace(accountID)
	if err := a.accountPlugin(pluginID); err != nil {
		return model.IdentityDetails{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), accountLoginTimeout)
	defer cancel()
	accounts, err := a.pluginMgr.Accounts(ctx, pluginID)
	if err != nil {
		return model.IdentityDetails{}, pluginCallError(pluginID, err)
	}
	details := model.IdentityDetails{PluginID: pluginID, AccountID: accountID, Masked: !reveal}
	found := false
	for _, acc := range accounts {
		if acc.ID != accountID {
			continue
		}
		found = true
		details.Label = acc.Label
		if acc.Identity != nil {
			identity := *acc.Identity
			if !reveal {
				identity.MachineID = maskIdentityID(identity.MachineID)
				identity.DeviceID = maskIdentityID(identity.DeviceID)
				identity.SessionID = maskIdentityID(identity.SessionID)
			}
			details.Identity = &identity
			details.UpdatedAt = identity.UpdatedAt
		}
		break
	}
	if !found {
		return model.IdentityDetails{}, fmt.Errorf("%w: %s/%s", webui.ErrAccountNotFound, pluginID, accountID)
	}
	if reveal {
		a.writeAudit(model.AuditAccount, "identity_reveal", pluginID+"/"+accountID, "ok")
	}
	return details, nil
}

// loginMethodsFromCapabilities derives the login method list from login_*
// capabilities in the contract's canonical order.
func loginMethodsFromCapabilities(mf model.Manifest) []string {
	type pair struct {
		capability string
		method     string
	}
	order := []pair{
		{model.CapLoginAPIKey, "api_key"},
		{model.CapLoginOAuth, "oauth"},
		{model.CapLoginIDEPlugin, "ide_plugin"},
		{model.CapLoginPasteCode, "paste_code"},
		{model.CapLoginLocalState, "local_state"},
		{model.CapLoginCookie, "cookie"},
	}
	methods := make([]string, 0, len(order))
	for _, item := range order {
		if pluginHasCapability(mf, item.capability) {
			methods = append(methods, item.method)
		}
	}
	return methods
}

// deriveLoginState builds one GET /api/accounts/login-state row. A manifest
// login block wins; otherwise the policy is derived from capabilities and
// permissions and labelled "derived".
func deriveLoginState(mf model.Manifest, checkedAt int64) model.LoginState {
	state := model.LoginState{
		PluginID:  strings.TrimSpace(mf.ID),
		Name:      strings.TrimSpace(mf.Name),
		APILevel:  mf.APILevel,
		Methods:   loginMethodsFromCapabilities(mf),
		CheckedAt: checkedAt,
		Source:    model.LoginSourceDerived,
	}
	if state.PluginID == "" {
		state.PluginID = state.Name
	}
	if state.Name == "" {
		state.Name = state.PluginID
	}
	state.ReadsLocalState = pluginHasCapability(mf, model.CapLoginLocalState)
	state.RequiresApp = state.ReadsLocalState ||
		len(mf.Permissions.ReadPaths) > 0 || len(mf.Permissions.LaunchApps) > 0
	state.AutoRefresh = pluginHasCapability(mf, model.CapRefresh)

	if mf.Login != nil {
		state.Source = model.LoginSourceManifest
		if methods := mf.Login.Methods; len(methods) > 0 {
			trimmed := make([]string, 0, len(methods))
			for _, method := range methods {
				if method = strings.TrimSpace(method); method != "" {
					trimmed = append(trimmed, method)
				}
			}
			if len(trimmed) > 0 {
				state.Methods = trimmed
			}
		}
		state.ReadsLocalState = mf.Login.ReadsLocalState
		state.RequiresApp = mf.Login.RequiresApp
		state.AutoRefresh = mf.Login.AutoRefresh
		state.Notes = strings.TrimSpace(mf.Login.Notes)
	}
	if state.Methods == nil {
		state.Methods = []string{}
	}
	return state
}

// accountLoginStates lists the login policy of every installed plugin.
func (a *App) accountLoginStates() []model.LoginState {
	if a.pluginMgr == nil {
		return []model.LoginState{}
	}
	return loginStatesFrom(a.pluginMgr.List(), time.Now().Unix())
}

// loginStatesFrom builds the login-state matrix for a plugin list.
func loginStatesFrom(list []model.PluginStatus, checkedAt int64) []model.LoginState {
	out := make([]model.LoginState, 0, len(list))
	for _, st := range list {
		out = append(out, deriveLoginState(st.Manifest, checkedAt))
	}
	return out
}

// ---------------------------------------------------------------- groups

// listGroups returns every group with its member count.
func (a *App) listGroups() ([]model.AccountGroup, error) {
	if a.store == nil {
		return []model.AccountGroup{}, nil
	}
	groups, err := a.store.ListAccountGroups()
	if err != nil {
		return nil, err
	}
	if groups == nil {
		groups = []model.AccountGroup{}
	}
	return groups, nil
}

// newGroupID returns "grp_" plus 8 hex characters.
func newGroupID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "grp_" + strconv.FormatUint(uint64(time.Now().UnixNano())&0xffffffff, 16)
	}
	return "grp_" + hex.EncodeToString(b[:])
}

// createGroup creates one group and reports its id.
func (a *App) createGroup(name, color string) (string, error) {
	if a.store == nil {
		return "", errors.New("app: store is not available")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("%w: group name is required", webui.ErrInvalidRequest)
	}
	id := newGroupID()
	if err := a.store.UpsertAccountGroup(model.AccountGroup{
		ID: id, Name: name, Color: strings.TrimSpace(color),
	}); err != nil {
		return "", err
	}
	a.writeAudit(model.AuditGroup, "create", id, "ok")
	return id, nil
}

// accountSave applies the whole account-center draft atomically. It mints ids
// for new groups, rejects duplicate names and dangling group references before
// the store transaction starts, then rewrites the egress files once.
func (a *App) accountSave(draft model.AccountDraft) (model.SaveApplied, error) {
	if a.store == nil {
		return model.SaveApplied{}, errors.New("app: store is not available")
	}
	existing, err := a.store.ListAccountGroups()
	if err != nil {
		return model.SaveApplied{}, err
	}
	byID := make(map[string]model.AccountGroup, len(existing))
	byName := make(map[string]string, len(existing))
	for _, group := range existing {
		byID[group.ID] = group
		byName[group.Name] = group.ID
	}
	deleted := make(map[string]bool, len(draft.DeletedGroups))
	for _, id := range draft.DeletedGroups {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		deleted[id] = true
		delete(byID, id)
		if group, ok := groupByName(byName, id); ok {
			delete(byName, group)
		}
	}

	seen := make(map[string]bool, len(draft.Groups))
	draftNames := make(map[string]bool, len(draft.Groups))
	for i := range draft.Groups {
		group := &draft.Groups[i]
		group.ID = strings.TrimSpace(group.ID)
		group.Name = strings.TrimSpace(group.Name)
		if group.Name == "" {
			return model.SaveApplied{}, fmt.Errorf("%w: every group needs a name", webui.ErrInvalidRequest)
		}
		if group.ID == "" {
			for {
				id := newGroupID()
				if !seen[id] {
					if _, exists := byID[id]; !exists {
						group.ID = id
						break
					}
				}
			}
		} else if existing, ok := byID[group.ID]; ok && group.Sort == nil {
			// An omitted sort on an update keeps the stored position.
			sortValue := existing.Sort
			group.Sort = &sortValue
		}
		if deleted[group.ID] {
			return model.SaveApplied{}, fmt.Errorf("%w: group %s cannot be both saved and deleted", webui.ErrInvalidRequest, group.ID)
		}
		if seen[group.ID] {
			return model.SaveApplied{}, fmt.Errorf("%w: duplicate group id %s", webui.ErrInvalidRequest, group.ID)
		}
		seen[group.ID] = true
		if draftNames[group.Name] {
			return model.SaveApplied{}, fmt.Errorf("%w: duplicate group name %q", webui.ErrInvalidRequest, group.Name)
		}
		draftNames[group.Name] = true
		if owner, ok := byName[group.Name]; ok && owner != group.ID {
			return model.SaveApplied{}, fmt.Errorf("%w: group name %q is already in use", webui.ErrInvalidRequest, group.Name)
		}
	}
	for id := range seen {
		byID[id] = model.AccountGroup{ID: id}
	}

	for i := range draft.Accounts {
		item := &draft.Accounts[i]
		item.PluginID = strings.TrimSpace(item.PluginID)
		item.AccountID = strings.TrimSpace(item.AccountID)
		if item.PluginID == "" || item.AccountID == "" {
			return model.SaveApplied{}, fmt.Errorf("%w: every account needs plugin_id and account_id", webui.ErrInvalidRequest)
		}
		if item.Alias != nil {
			alias := strings.TrimSpace(*item.Alias)
			if utf8.RuneCountInString(alias) > maxAliasRunes {
				return model.SaveApplied{}, fmt.Errorf("%w: alias is longer than %d characters", webui.ErrInvalidRequest, maxAliasRunes)
			}
			item.Alias = &alias
		}
		if item.GroupID != nil {
			groupID := strings.TrimSpace(*item.GroupID)
			item.GroupID = &groupID
			if groupID != "" {
				if _, ok := byID[groupID]; !ok {
					return model.SaveApplied{}, fmt.Errorf("%w: unknown group %s", webui.ErrInvalidRequest, groupID)
				}
			}
		}
	}

	applied, err := a.store.SaveAccountDraft(draft)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return model.SaveApplied{}, fmt.Errorf("%w: %v", webui.ErrInvalidRequest, err)
		}
		return model.SaveApplied{}, err
	}

	// Egress assignments may have changed; refresh the runtime files once.
	a.writeAllEgress()
	a.reloadEgressPlugins()
	a.writeAudit(model.AuditAccount, "save",
		fmt.Sprintf("%d accounts/%d groups/%d memberships", applied.Accounts, applied.Groups, applied.Memberships), "ok")
	a.logger.Info("app", "account draft saved", map[string]any{
		"accounts": applied.Accounts, "groups": applied.Groups, "memberships": applied.Memberships,
	})
	return applied, nil
}

// groupByName finds the name owning a group id (used when a deletion frees a
// name for reuse in the same draft).
func groupByName(byName map[string]string, id string) (string, bool) {
	for name, owner := range byName {
		if owner == id {
			return name, true
		}
	}
	return "", false
}

// ---------------------------------------------------------------- rules (M6)

// rulePacks returns every stored pack plus the names of the builtin packs.
func (a *App) rulePacks() ([]model.RulePack, []string, error) {
	if a.rulesMgr == nil {
		return nil, nil, errors.New("app: rule manager is not available")
	}
	packs, err := a.rulesMgr.ListPacks()
	if err != nil {
		return nil, nil, err
	}
	if packs == nil {
		packs = []model.RulePack{}
	}
	return packs, []string{rules.BuiltinWBPack}, nil
}

// saveRulePack persists a pack, then rewrites and reloads every plugin that
// declares it. Reload failures are logged, never returned: the pack is saved.
func (a *App) saveRulePack(pack model.RulePack) error {
	if a.rulesMgr == nil {
		return errors.New("app: rule manager is not available")
	}
	if err := a.rulesMgr.SavePack(pack); err != nil {
		return err
	}
	a.reloadRulesFor(context.Background(), a.pluginsUsingPack(pack.Name))
	return nil
}

// deleteRulePack removes a pack and refreshes the plugins that declared it.
func (a *App) deleteRulePack(name string) error {
	if a.rulesMgr == nil {
		return errors.New("app: rule manager is not available")
	}
	plugins := a.pluginsUsingPack(name)
	if err := a.rulesMgr.DeletePack(name); err != nil {
		return err
	}
	a.reloadRulesFor(context.Background(), plugins)
	return nil
}

// resetRulePack restores a builtin pack to its shipped content. Packs that are
// not builtin are reported as missing (there is no default to restore).
func (a *App) resetRulePack(name string) error {
	if a.rulesMgr == nil {
		return errors.New("app: rule manager is not available")
	}
	pack, ok := rules.BuiltinPack(name)
	if !ok {
		return fmt.Errorf("%w: %s", rules.ErrPackNotFound, strings.TrimSpace(name))
	}
	if err := a.rulesMgr.SavePack(pack); err != nil {
		return err
	}
	a.logger.Info("app", "rule pack reset to builtin", map[string]any{"name": pack.Name})
	a.reloadRulesFor(context.Background(), a.pluginsUsingPack(pack.Name))
	return nil
}

// reloadRules rewrites every rules file and reloads the running plugins that
// declare packs. Failures are reported per plugin and never fail the request.
func (a *App) reloadRules() ([]string, map[string]string, error) {
	if a.rulesMgr == nil || a.pluginMgr == nil {
		return nil, nil, errors.New("app: rule services are not available")
	}
	a.writeAllRules()

	reloaded := []string{}
	failed := make(map[string]string)
	for _, st := range a.pluginMgr.List() {
		id := strings.TrimSpace(st.Manifest.ID)
		if id == "" || !st.Running || len(st.Manifest.Rules.Packs) == 0 {
			continue
		}
		if err := a.reloadPlugin(context.Background(), id); err != nil {
			failed[id] = err.Error()
			continue
		}
		reloaded = append(reloaded, id)
	}
	if len(failed) == 0 {
		failed = nil
	}
	return reloaded, failed, nil
}

// reloadRulesFor rewrites the rules file of pluginIDs and reloads the running
// ones. Errors are logged only; a plugin that cannot reload keeps the previous
// document and picks the new one up on its next start.
func (a *App) reloadRulesFor(ctx context.Context, pluginIDs []string) {
	if a.pluginMgr == nil {
		return
	}
	seen := make(map[string]bool, len(pluginIDs))
	for _, id := range pluginIDs {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		a.writePluginRules(id)
		st, ok := a.pluginMgr.Get(id)
		if !ok || !st.Running {
			continue
		}
		if err := a.reloadPlugin(ctx, id); err != nil {
			a.logger.Warn("app", "cannot reload plugin after rules change", map[string]any{
				"plugin": id, "error": err.Error(),
			})
		}
	}
}

// reloadPlugin asks a plugin to re-read its runtime files, bounded by 15s.
func (a *App) reloadPlugin(ctx context.Context, id string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return a.pluginMgr.Reload(ctx, id)
}

// pluginsUsingPack lists the plugins whose manifest declares name.
func (a *App) pluginsUsingPack(name string) []string {
	if a.pluginMgr == nil {
		return nil
	}
	name = strings.TrimSpace(name)
	var ids []string
	for _, st := range a.pluginMgr.List() {
		id := strings.TrimSpace(st.Manifest.ID)
		if id == "" || name == "" {
			continue
		}
		for _, pack := range st.Manifest.Rules.Packs {
			if strings.TrimSpace(pack) == name {
				ids = append(ids, id)
				break
			}
		}
	}
	return ids
}

// resetIdentity proxies an identity reset to the plugin (M8). The host never
// touches the OS identity itself. OS-level switches are serialized host-wide so
// two plugins never rewrite machine identifiers at the same time; "request"
// mode only rotates per-request headers and does not take the lock.
func (a *App) resetIdentity(pluginID, accountID, mode string) error {
	if a.pluginMgr == nil {
		return errors.New("app: plugin manager is not available")
	}
	pluginID = strings.TrimSpace(pluginID)
	accountID = strings.TrimSpace(accountID)
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == model.IdentityOS {
		a.identityMu.Lock()
		defer a.identityMu.Unlock()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := a.pluginMgr.ResetIdentity(ctx, pluginID, accountID, mode); err != nil {
		a.logger.Warn("app", "identity reset failed", map[string]any{
			"plugin": pluginID, "account": accountID, "mode": mode, "error": err.Error(),
		})
		a.writeAudit(model.AuditAccount, "identity_reset", pluginID+"/"+accountID, "fail")
		return err
	}
	a.logger.Info("app", "identity reset", map[string]any{
		"plugin": pluginID, "account": accountID, "mode": mode,
	})
	a.writeAudit(model.AuditAccount, "identity_reset", pluginID+"/"+accountID, mode)
	return nil
}

// checkins reads the check-in calendar rows from SQLite.
func (a *App) checkins(pluginID, from, to string) any {
	if a.store == nil {
		return map[string]any{"records": []model.CheckinRecord{}}
	}
	rows, err := a.store.QueryCheckinRecords(strings.TrimSpace(pluginID), strings.TrimSpace(from), strings.TrimSpace(to))
	if err != nil {
		return map[string]any{"records": []model.CheckinRecord{}, "error": err.Error()}
	}
	if rows == nil {
		rows = []model.CheckinRecord{}
	}
	return map[string]any{"records": rows}
}

func (a *App) listProxies() []model.Proxy {
	if a.store == nil {
		return []model.Proxy{}
	}
	proxies, err := a.store.ListProxies()
	if err != nil {
		a.logger.Warn("app", "cannot list proxies", map[string]any{"error": err.Error()})
		return []model.Proxy{}
	}
	if proxies == nil {
		return []model.Proxy{}
	}
	for index := range proxies {
		if proxies[index].ID == model.SystemProxyID {
			proxies[index].Managed = model.SystemProxyID
			proxies[index].ManagedDetail = a.systemProxyNote()
		}
	}
	return proxies
}

// saveProxy persists a proxy (password only in DPAPI) and starts or stops its
// local forwarder.
func (a *App) saveProxy(proxy model.Proxy) error {
	if strings.EqualFold(strings.TrimSpace(proxy.ID), model.SystemProxyID) {
		return errSystemProxyManaged
	}
	if a.store == nil || a.secrets == nil || a.egressMgr == nil {
		return errors.New("app: proxy services are not available")
	}
	if strings.TrimSpace(proxy.ID) == "" {
		proxy.ID = newProxyID()
	}
	if proxy.Password != "" {
		if err := a.secrets.SetProxyPassword(proxy.ID, proxy.Password); err != nil {
			return err
		}
	} else if password, ok := a.secrets.GetProxyPassword(proxy.ID); ok {
		proxy.Password = password
	}
	if err := a.store.UpsertProxy(proxy, ""); err != nil {
		return err
	}
	if a.egressHealth != nil {
		a.egressHealth.InvalidateAll()
	}
	var err error
	if proxy.Enabled {
		err = a.egressMgr.StartProxy(proxy)
	} else {
		err = a.egressMgr.StopProxy(proxy.ID)
	}
	if err != nil {
		a.logger.Warn("app", "proxy saved but its forwarder failed", map[string]any{
			"proxy": proxy.ID, "error": err.Error(),
		})
		return err
	}
	a.logger.Info("app", "proxy saved", map[string]any{"proxy": proxy.ID, "enabled": proxy.Enabled})
	a.writeAllEgress()
	a.reloadEgressPlugins()
	return nil
}

func (a *App) deleteProxy(id string) error {
	if strings.EqualFold(strings.TrimSpace(id), model.SystemProxyID) {
		return errSystemProxyManaged
	}
	var errs []error
	if a.store != nil {
		if err := a.store.DeleteProxy(id); err != nil {
			errs = append(errs, err)
		}
	}
	if a.egressHealth != nil {
		a.egressHealth.InvalidateAll()
	}
	if a.secrets != nil {
		if err := a.secrets.RemoveProxyPassword(id); err != nil {
			errs = append(errs, err)
		}
	}
	if a.egressMgr != nil {
		if err := a.egressMgr.StopProxy(id); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		a.logger.Info("app", "proxy deleted", map[string]any{"proxy": id})
		a.writeAllEgress()
		a.reloadEgressPlugins()
	}
	return errors.Join(errs...)
}

// checkProxy probes a proxy's exit IP and records the health result.
func (a *App) checkProxy(id string) any {
	checkedAt := time.Now().Unix()
	if a.store == nil || a.secrets == nil {
		return map[string]any{"ok": false, "exit_ip": "", "latency_ms": int64(0),
			"error": "proxy services are not available", "checked_at": checkedAt}
	}
	proxy, _, err := a.store.GetProxy(id)
	if err != nil {
		return map[string]any{"ok": false, "exit_ip": "", "latency_ms": int64(0),
			"error": err.Error(), "checked_at": checkedAt}
	}
	if id == model.SystemProxyID && !proxy.Enabled {
		reason := a.systemProxyNote()
		if reason == "" {
			reason = "Windows 未开启系统代理。"
		}
		return map[string]any{"ok": false, "exit_ip": "", "latency_ms": int64(0),
			"error": reason, "checked_at": checkedAt}
	}
	if password, ok := a.secrets.GetProxyPassword(id); ok {
		proxy.Password = password
	}
	exitIP, latency, checkErr := egress.Check(context.Background(), proxy, egress.DefaultIPEchoURL, 10*time.Second)
	errText := ""
	if checkErr != nil {
		errText = checkErr.Error()
	}
	if err := a.store.UpdateProxyHealth(id, checkedAt, exitIP, latency.Milliseconds(), errText); err != nil {
		a.logger.Warn("app", "cannot store proxy health", map[string]any{"proxy": id, "error": err.Error()})
	}
	if a.egressHealth != nil {
		a.egressHealth.InvalidateAll()
	}
	if checkErr != nil {
		a.logger.Warn("app", "proxy check failed", map[string]any{"proxy": id, "error": errText})
	}
	return map[string]any{
		"ok":         checkErr == nil,
		"exit_ip":    exitIP,
		"latency_ms": latency.Milliseconds(),
		"error":      errText,
		"checked_at": checkedAt,
	}
}

// requestLogs applies the log table filters and pagination. When the transport
// sets the internal export marker it returns the newest rows (bodies excluded,
// capped at store.MaxExportRows) for CSV rendering instead.
func (a *App) requestLogs(query map[string]string) any {
	empty := []model.APIRequestLog{}
	if a.store == nil {
		return webui.RequestLogsResult{Logs: empty}
	}
	filter, err := requestLogFilter(query)
	if err != nil {
		return webui.RequestLogsResult{Logs: empty, Error: err.Error()}
	}

	if query[webui.RequestLogExportParam] == "1" {
		rows, truncated, err := a.store.ExportAPIRequestLogs(filter, store.MaxExportRows)
		if err != nil {
			return webui.RequestLogsResult{Logs: empty, Error: err.Error()}
		}
		if rows == nil {
			rows = empty
		}
		return webui.RequestLogsResult{Logs: rows, Total: len(rows), Truncated: truncated}
	}

	rows, total, err := a.store.QueryAPIRequestLogs(filter)
	if err != nil {
		return webui.RequestLogsResult{Logs: empty, Error: err.Error()}
	}
	if rows == nil {
		rows = empty
	}
	return webui.RequestLogsResult{Logs: rows, Total: total}
}

// requestLogFilter maps the snake_case query onto store.Filter. Model is a
// substring; source/plugin_id, account_id/account and key_id/key are aliases so
// older callers keep working.
func requestLogFilter(query map[string]string) (store.Filter, error) {
	statusClass := strings.ToLower(strings.TrimSpace(query["status_class"]))
	if !store.ValidStatusClass(statusClass) {
		return store.Filter{}, fmt.Errorf("unknown status_class %q", statusClass)
	}
	var stream *bool
	switch strings.ToLower(strings.TrimSpace(query["stream"])) {
	case "true":
		value := true
		stream = &value
	case "false":
		value := false
		stream = &value
	}
	status, _ := strconv.Atoi(strings.TrimSpace(query["status"]))
	return store.Filter{
		From:        timeQueryValue(query["from"], false),
		To:          timeQueryValue(query["to"], true),
		Source:      firstNonEmpty(query["source"], query["plugin_id"]),
		Model:       strings.TrimSpace(query["model"]),
		Status:      status,
		KeyID:       firstNonEmpty(query["key_id"], query["key"]),
		Account:     firstNonEmpty(query["account_id"], query["account"]),
		StatusClass: statusClass,
		Stream:      stream,
		Keyword:     firstNonEmpty(query["q"], query["keyword"]),
		Limit:       positiveInt(query["limit"]),
		Offset:      positiveInt(query["offset"]),
	}, nil
}

func timeQueryValue(raw string, endOfDay bool) int64 {
	value, _ := parseTimeQuery(raw, endOfDay)
	return value
}

// runtimeLogs tails the host log, a plugin log, or explains why A's log is not
// shown (A keeps its own logs).
func (a *App) runtimeLogs(source string, n int) []string {
	if n <= 0 {
		n = 200
	}
	source = strings.TrimSpace(source)
	switch {
	case source == "" || source == "host":
		if a.dataDir == "" {
			return []string{}
		}
		path := filepath.Join(a.dataDir, "logs", "host-"+time.Now().Format("2006-01-02")+".jsonl")
		lines, err := tailFileLines(path, n)
		if err != nil {
			a.logger.Warn("app", "cannot tail host log", map[string]any{"error": err.Error()})
			return []string{}
		}
		if lines == nil {
			return []string{}
		}
		return lines
	case strings.HasPrefix(source, "plugin:"):
		id := strings.TrimSpace(strings.TrimPrefix(source, "plugin:"))
		if id == "" || a.pluginLog == nil {
			return []string{}
		}
		lines, err := a.pluginLog.Tail(id, n)
		if err != nil {
			a.logger.Warn("app", "cannot tail plugin log", map[string]any{"plugin": id, "error": err.Error()})
			return []string{}
		}
		if lines == nil {
			return []string{}
		}
		return lines
	case source == "a":
		return []string{"A（AI Work 助手）的日志由 A 自己保存，不在宿主内展示；请在 A 的设置页打开 A 的日志目录。"}
	default:
		return []string{}
	}
}

func (a *App) listKeys() []model.ClientKey {
	if a.store == nil {
		return []model.ClientKey{}
	}
	keys, err := a.store.ListClientKeys()
	if err != nil {
		a.logger.Warn("app", "cannot list client keys", map[string]any{"error": err.Error()})
		return []model.ClientKey{}
	}
	if keys == nil {
		return []model.ClientKey{}
	}
	return keys
}

// createKey mints a new client key. The plain value is returned once; only its
// SHA-256 hash is persisted.
func (a *App) createKey(name string) (string, model.ClientKey, error) {
	if a.store == nil || a.secrets == nil {
		return "", model.ClientKey{}, errors.New("app: key services are not available")
	}
	plain, id, prefix, hash := secrets.NewClientKey()
	if err := a.secrets.AddClientKey(id, hash); err != nil {
		return "", model.ClientKey{}, err
	}
	now := time.Now().Unix()
	key := model.ClientKey{
		ID:        id,
		Name:      strings.TrimSpace(name),
		Prefix:    prefix,
		Enabled:   true,
		CreatedAt: now,
	}
	if err := a.store.UpsertClientKey(id, key.Name, prefix, hash, true, now); err != nil {
		_ = a.secrets.RemoveClientKey(id)
		return "", model.ClientKey{}, err
	}
	a.logger.Info("app", "client key created", map[string]any{"key_id": id, "name": key.Name})
	return plain, key, nil
}

func (a *App) deleteKey(id string) error {
	var errs []error
	// Revoke the secret first: if removing the hash fails the row is kept (so
	// the key never becomes a working secret with no scope/quota row). If the
	// row delete then fails, the hash is already gone and verification fails
	// closed.
	if a.secrets != nil {
		if err := a.secrets.RemoveClientKey(id); err != nil {
			errs = append(errs, err)
		}
	}
	if a.store != nil {
		if err := a.store.DeleteClientKey(id); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		a.logger.Info("app", "client key deleted", map[string]any{"key_id": id})
	}
	return errors.Join(errs...)
}

// createKeySpec mints a key together with its model scope, pool binding and
// daily quota (plan 2026-10-21 §1). The scope is written in the same insert, so
// there is no window where a restricted key exists unrestricted.
func (a *App) createKeySpec(spec webui.KeySpec) (string, model.ClientKey, error) {
	if a.store == nil || a.secrets == nil {
		return "", model.ClientKey{}, errors.New("app: key services are not available")
	}
	plain, id, prefix, hash := secrets.NewClientKey()
	if err := a.secrets.AddClientKey(id, hash); err != nil {
		return "", model.ClientKey{}, err
	}
	now := time.Now().Unix()
	key := model.ClientKey{
		ID:         id,
		Name:       spec.Name,
		Prefix:     prefix,
		Enabled:    spec.Enabled,
		CreatedAt:  now,
		ModelScope: spec.ModelScope,
		BindPool:   spec.BindPool,
		DailyQuota: spec.DailyQuota,
	}
	key.QuotaLeft = store.ClientKeyQuotaLeft(key.DailyQuota, 0)
	scope := store.ClientKeyScope{ModelScope: spec.ModelScope, BindPool: spec.BindPool, DailyQuota: spec.DailyQuota}
	if err := a.store.UpsertClientKeyScoped(id, key.Name, prefix, hash, key.Enabled, now, scope); err != nil {
		_ = a.secrets.RemoveClientKey(id)
		return "", model.ClientKey{}, err
	}
	a.logger.Info("app", "client key created", map[string]any{
		"key_id": id, "name": key.Name, "models": len(spec.ModelScope),
		"bind_pool": spec.BindPool, "daily_quota": spec.DailyQuota,
	})
	return plain, key, nil
}

// updateKey edits name, enabled flag and the scope/quota triple. The gateway
// reads the row on every request, so a rename or disable takes effect with the
// next call; the secret itself never changes.
func (a *App) updateKey(id string, spec webui.KeySpec) (model.ClientKey, error) {
	if a.store == nil {
		return model.ClientKey{}, errors.New("app: key services are not available")
	}
	scope := store.ClientKeyScope{ModelScope: spec.ModelScope, BindPool: spec.BindPool, DailyQuota: spec.DailyQuota}
	if err := a.store.UpdateClientKey(id, spec.Name, spec.Enabled, scope); err != nil {
		return model.ClientKey{}, err
	}
	key, ok, err := a.store.GetClientKey(id)
	if err != nil {
		return model.ClientKey{}, err
	}
	if !ok {
		return model.ClientKey{}, store.ErrNotFound
	}
	a.logger.Info("app", "client key updated", map[string]any{
		"key_id": id, "enabled": spec.Enabled, "models": len(spec.ModelScope),
		"bind_pool": spec.BindPool, "daily_quota": spec.DailyQuota,
	})
	return key, nil
}

// ---------------------------------------------------------------- custom models

// customModelTestTimeout bounds the explicit "测试" probe. Saving a model never
// calls the upstream.
const customModelTestTimeout = 5 * time.Second

// customModelTestBodyBytes caps how much of a probe response body is read. The
// body is discarded, never echoed: it may repeat the upstream credential.
const customModelTestBodyBytes = 1 << 10

// gatewayCustomModels is the gateway's view of the stored entries. The API key
// is not part of it; the gateway asks for it through customModelKey.
func (a *App) gatewayCustomModels() ([]gateway.CustomModel, error) {
	if a.store == nil {
		return nil, errors.New("app: custom model store is not available")
	}
	rows, err := a.store.ListCustomModels()
	if err != nil {
		return nil, err
	}
	out := make([]gateway.CustomModel, 0, len(rows))
	for _, row := range rows {
		out = append(out, gateway.CustomModel{
			ID:                row.ID,
			ConnectionID:      row.ConnectionID,
			Name:              row.Name,
			Protocol:          row.Protocol,
			BaseURL:           row.BaseURL,
			UpstreamModel:     row.UpstreamModel,
			Enabled:           row.Enabled,
			ConnectionEnabled: row.ConnectionEnabled,
			ContextWindow:     row.ContextWindow,
			MaxOutputTokens:   row.MaxOutputTokens,
			InputTypes:        row.InputTypes,
			ReasoningTiers:    row.ReasoningTiers,
			DefaultReasoning:  row.DefaultReasoning,
		})
	}
	return out, nil
}

// customModelKey resolves an upstream key. The gateway passes the connection id
// when the model has one; legacy rows still carry their key under the model id.
func (a *App) customModelKey(id string) (string, bool) {
	if a.secrets == nil || strings.TrimSpace(id) == "" {
		return "", false
	}
	if value, ok := a.secrets.GetExtra(store.CustomConnectionSecretKey(id)); ok && strings.TrimSpace(value) != "" {
		return value, true
	}
	return a.secrets.GetExtra(store.CustomModelSecretKey(id))
}

// listCustomModels returns the admin view: the stored rows plus key_set and
// the conflict flag (a plugin model or logical binding already owns the id).
func (a *App) listCustomModels() ([]store.CustomModel, error) {
	if a.store == nil {
		return nil, errors.New("app: custom model store is not available")
	}
	rows, err := a.store.ListCustomModels()
	if err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []store.CustomModel{}
	}
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, row.Name)
	}
	conflicts := map[string]bool{}
	if gw, ok := a.gatewayHandler.(*gateway.Gateway); ok && len(names) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		conflicts = gw.CustomModelConflicts(ctx, names)
		cancel()
	}
	for i := range rows {
		if a.secrets != nil {
			if rows[i].ConnectionID != "" {
				_, rows[i].KeySet = a.connectionKey(rows[i].ConnectionID)
			} else {
				_, rows[i].KeySet = a.secrets.GetExtra(store.CustomModelSecretKey(rows[i].ID))
			}
		}
		rows[i].Conflict = conflicts[store.CanonicalCustomModelName(rows[i].Name)]
	}
	return rows, nil
}

// customConnectionForModel resolves the owning connection. A model that names
// one inherits its endpoint and key; the legacy payload without a connection
// is merged into an OpenAI-compatible connection so every model has an owner.
func (a *App) customConnectionForModel(input webui.CustomModelInput) (store.CustomConnection, error) {
	if id := strings.TrimSpace(input.ConnectionID); id != "" {
		conn, found, err := a.store.GetCustomConnection(id)
		if err != nil {
			return store.CustomConnection{}, err
		}
		if !found {
			return store.CustomConnection{}, store.ErrNotFound
		}
		return conn, nil
	}
	conn, _, err := a.createCustomConnection(webui.CustomConnectionInput{
		Protocol: store.ProtocolOpenAI, BaseURL: input.BaseURL,
		APIKey: input.APIKey, Enabled: true,
	})
	return conn, err
}

// createCustomModel writes one model under its connection. The API key belongs
// to the connection and is never copied onto the model row.
func (a *App) createCustomModel(input webui.CustomModelInput) (store.CustomModel, error) {
	if a.store == nil || a.secrets == nil {
		return store.CustomModel{}, errors.New("app: custom model services are not available")
	}
	conn, err := a.customConnectionForModel(input)
	if err != nil {
		return store.CustomModel{}, err
	}
	row, err := a.store.UpsertCustomModel(store.CustomModel{
		ConnectionID: conn.ID, Name: input.Name, BaseURL: conn.BaseURL,
		UpstreamModel: input.UpstreamModel, Enabled: input.Enabled,
		ContextWindow: input.ContextWindow, MaxOutputTokens: input.MaxOutputTokens,
		InputTypes: input.InputTypes, ReasoningTiers: input.ReasoningTiers,
		DefaultReasoning: input.DefaultReasoning,
	})
	if err != nil {
		return store.CustomModel{}, err
	}
	row.ConnectionEnabled = conn.Enabled
	_, row.KeySet = a.connectionKey(conn.ID)
	a.logger.Info("app", "custom model created", map[string]any{
		"id": row.ID, "name": row.Name, "connection": conn.ID, "enabled": row.Enabled,
	})
	return row, nil
}

// updateCustomModel applies a partial edit. The connection and the upstream
// model id are immutable; settings and the display name can change. An API key
// patch rotates the owning connection's key.
func (a *App) updateCustomModel(id string, patch webui.CustomModelPatch) (store.CustomModel, error) {
	if a.store == nil || a.secrets == nil {
		return store.CustomModel{}, errors.New("app: custom model services are not available")
	}
	row, found, err := a.store.GetCustomModel(id)
	if err != nil {
		return store.CustomModel{}, err
	}
	if !found {
		return store.CustomModel{}, store.ErrNotFound
	}
	if patch.UpstreamModel != nil && strings.TrimSpace(*patch.UpstreamModel) != row.UpstreamModel {
		return store.CustomModel{}, fmt.Errorf("%w: 上游模型 ID 创建后不能修改，请删除后重新添加",
			webui.ErrInvalidRequest)
	}
	if patch.BaseURL != nil && normalizeConnectionBase(*patch.BaseURL) != row.BaseURL {
		return store.CustomModel{}, fmt.Errorf("%w: API 地址属于接入，请在接入上修改",
			webui.ErrInvalidRequest)
	}
	if patch.Name != nil {
		row.Name = *patch.Name
	}
	if patch.Enabled != nil {
		row.Enabled = *patch.Enabled
	}
	if patch.ContextWindow != nil {
		row.ContextWindow = *patch.ContextWindow
	}
	if patch.MaxOutputTokens != nil {
		row.MaxOutputTokens = *patch.MaxOutputTokens
	}
	if patch.InputTypes != nil {
		row.InputTypes = *patch.InputTypes
	}
	if patch.ReasoningTiers != nil {
		row.ReasoningTiers = *patch.ReasoningTiers
	}
	if patch.DefaultReasoning != nil {
		row.DefaultReasoning = *patch.DefaultReasoning
	}
	row, err = a.store.UpsertCustomModel(row)
	if err != nil {
		return store.CustomModel{}, err
	}
	if patch.APIKey != nil {
		if row.ConnectionID != "" {
			if _, err := a.updateCustomConnection(row.ConnectionID, webui.CustomConnectionPatch{APIKey: patch.APIKey}); err != nil {
				return store.CustomModel{}, err
			}
		} else if err := a.secrets.SetExtra(store.CustomModelSecretKey(row.ID), strings.TrimSpace(*patch.APIKey)); err != nil {
			return store.CustomModel{}, err
		}
	}
	if row.ConnectionID != "" {
		if conn, found, cerr := a.store.GetCustomConnection(row.ConnectionID); cerr == nil && found {
			row.ConnectionEnabled = conn.Enabled
		}
		_, row.KeySet = a.connectionKey(row.ConnectionID)
	} else {
		_, row.KeySet = a.secrets.GetExtra(store.CustomModelSecretKey(row.ID))
	}
	a.logger.Info("app", "custom model updated", map[string]any{
		"id": row.ID, "name": row.Name, "enabled": row.Enabled, "key_set": row.KeySet,
	})
	return row, nil
}

// deleteCustomModel removes the row and clears its DPAPI secret.
func (a *App) deleteCustomModel(id string) (bool, error) {
	if a.store == nil {
		return false, errors.New("app: custom model store is not available")
	}
	removed, err := a.store.DeleteCustomModel(id)
	if err != nil {
		return false, err
	}
	if removed && a.secrets != nil {
		if err := a.secrets.SetExtra(store.CustomModelSecretKey(strings.TrimSpace(id)), ""); err != nil {
			return true, err
		}
		a.logger.Info("app", "custom model deleted", map[string]any{"id": strings.TrimSpace(id)})
	}
	return removed, nil
}

// testCustomModel sends one minimal non-stream chat request ("ping",
// max_tokens 1) to the entry's upstream with a 5s timeout. It only runs on the
// explicit admin action; reasons are redacted of the key and never contain the
// request URL. The probe itself lives in runCustomModelTest (diagnostics.go)
// and is shared with the connectivity self-check.
func (a *App) testCustomModel(id string) (int64, error) {
	res := a.runCustomModelTest(id)
	return res.latencyMS, res.err
}

// ---------------------------------------------------------------- usage (S2)

// parseUsageQuery shares filter semantics across /api/usage/* endpoints.
func (a *App) parseUsageQuery(query map[string][]string) (usage.Query, error) {
	q, err := usage.ParseQuery(url.Values(query), time.Now())
	if err != nil {
		return q, fmt.Errorf("%w: %v", webui.ErrInvalidRequest, err)
	}
	return q, nil
}

func (a *App) usageSummary(query map[string][]string) (any, error) {
	q, err := a.parseUsageQuery(query)
	if err != nil {
		return nil, err
	}
	return a.usageSvc.Summary(q)
}

func (a *App) usageCredits(query map[string][]string) (any, error) {
	q, err := a.parseUsageQuery(query)
	if err != nil {
		return nil, err
	}
	return a.usageSvc.Credits(q, false)
}

func (a *App) usageExpiry(query map[string][]string) (any, error) {
	q, err := a.parseUsageQuery(query)
	if err != nil {
		return nil, err
	}
	return a.usageSvc.Credits(q, true)
}

func (a *App) usageSessionsPage(query map[string][]string) (any, error) {
	q, err := a.parseUsageQuery(query)
	if err != nil {
		return nil, err
	}
	return a.usageSvc.Sessions(q)
}

func (a *App) usageExport(w io.Writer, query map[string][]string) (string, error) {
	q, err := a.parseUsageQuery(query)
	if err != nil {
		return "", err
	}
	return a.usageSvc.Export(w, q)
}

// usageLatency serves GET /api/usage/latency (plan 2026-10-21 §5).
func (a *App) usageLatency(days int) ([]store.DayLatency, error) {
	return a.store.LatencyStats(days)
}

// usageKeyUsage serves GET /api/usage/keys. The rows are read-only; the key
// quota path owns the writes into client_key_usage.
func (a *App) usageKeyUsage(days int) ([]store.KeyUsageRow, error) {
	return a.store.KeyUsageSummary(days)
}

// usageModels serves GET /api/usage/models: consumption ranked by model.
func (a *App) usageModels(days int) ([]store.ModelUsageRow, error) {
	return a.store.ModelUsageSummary(days)
}

// usageModelLatency serves GET /api/usage/latency/models.
func (a *App) usageModelLatency(days int) ([]store.ModelLatencyRow, error) {
	return a.store.ModelLatencyStats(days)
}

// ---------------------------------------------------------------- unified logs (S2)

func (a *App) timelineFilter(query map[string][]string) (logging.TimelineFilter, error) {
	filter, err := logging.ParseTimelineFilter(url.Values(query), time.Now())
	if err != nil {
		return logging.TimelineFilter{}, fmt.Errorf("%w: %v", webui.ErrInvalidRequest, err)
	}
	return filter, nil
}

func (a *App) logTimeline(query map[string][]string) (any, error) {
	filter, err := a.timelineFilter(query)
	if err != nil {
		return nil, err
	}
	cursor, page, pageSize, err := logging.ParseTimelinePage(url.Values(query))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", webui.ErrInvalidRequest, err)
	}
	if page > 1 && cursor == "" {
		return nil, fmt.Errorf("%w: page pagination was removed from the merged timeline; pass next_cursor as cursor instead", webui.ErrInvalidRequest)
	}
	if a.timeline == nil {
		return nil, errors.New("app: log timeline is not available")
	}
	result, err := a.timeline.Query(filter, cursor, pageSize)
	if errors.Is(err, logging.ErrBadCursor) {
		return nil, fmt.Errorf("%w: %v", webui.ErrInvalidRequest, err)
	}
	return result, err
}

// logBody returns the stored, redacted body. Bodies are only reachable while
// debug_body is on (D97).
func (a *App) logBody(reqID, which string) (webui.LogBody, error) {
	if !a.configSnapshot().Logging.DebugBody {
		return webui.LogBody{}, webui.ErrDebugDisabled
	}
	if a.store == nil {
		return webui.LogBody{}, errors.New("app: store is not available")
	}
	reqBody, respBody, found, err := a.store.APILogBodies(reqID)
	if err != nil {
		return webui.LogBody{}, err
	}
	if !found {
		return webui.LogBody{}, webui.ErrLogNotFound
	}
	body := reqBody
	if which == "resp" {
		body = respBody
	}
	return webui.LogBody{
		Body:      body,
		Truncated: len(body) >= logging.BodyReqLimit,
	}, nil
}

// logSeed positions a stream at the current end of every source.
func (a *App) logSeed(query map[string][]string) (string, error) {
	filter, err := a.timelineFilter(query)
	if err != nil {
		return "", err
	}
	if a.timeline == nil {
		return "", errors.New("app: log timeline is not available")
	}
	cursor, err := a.timeline.Seed(filter)
	if err != nil {
		return "", err
	}
	return cursor.Encode(), nil
}

// logPoll returns everything appended since cursor as JSON timeline events.
func (a *App) logPoll(query map[string][]string, cursor string, limit int) (webui.LogPollResult, error) {
	filter, err := a.timelineFilter(query)
	if err != nil {
		return webui.LogPollResult{}, err
	}
	if a.timeline == nil {
		return webui.LogPollResult{}, errors.New("app: log timeline is not available")
	}
	cur, err := logging.DecodeTimelineCursor(cursor)
	if err != nil {
		return webui.LogPollResult{}, err
	}
	items, next, err := a.timeline.Poll(filter, cur, limit)
	if err != nil {
		return webui.LogPollResult{}, err
	}
	out := webui.LogPollResult{Events: make([]webui.LogEvent, 0, len(items)), Cursor: next.Encode()}
	for _, item := range items {
		data, err := json.Marshal(item)
		if err != nil {
			continue
		}
		out.Events = append(out.Events, webui.LogEvent{ID: out.Cursor, Data: data})
	}
	return out, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if v := strings.TrimSpace(value); v != "" {
			return v
		}
	}
	return ""
}

func positiveInt(raw string) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}
