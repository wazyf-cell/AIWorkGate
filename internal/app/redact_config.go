package app

// P10 收尾（脱敏管理 UI）：config.redact 的读写端点与热更新生效链。
//
// 生效链：PUT /api/redact → saveRedactConfig 校验 → saveSettings（落盘
// config.json + 存活快照）→ applyRedactLive 比较 redactOptionsFromConfig 新旧
// → 变了才重建 gateway。gateway 的脱敏配置是构造期值（gateway.Options.Redact，
// internal/gateway 不可改动），没有运行期 setter，所以热更新以「重建网关并
// 原子换柄」实现：旧网关只服务在途请求直到结束，新请求即走新配置；速率限制
// 窗口与模型/意图缓存随重建清零（日志里说明）。页面级 PUT /api/settings 同样
// 携带 redact 段，saveSettings 里也调用 applyRedactLive，两条保存路径行为一致。

import (
	"encoding/json"
	"fmt"
	"net/http"

	"aiworkgate/internal/config"
	"aiworkgate/internal/gateway"
	"aiworkgate/internal/redact"
	"aiworkgate/internal/webui"
)

// nopHostLogger drops host log lines when no logger is wired (tests, fakes).
// The logging package owns no exported no-op, and internal/logging is
// off-limits for this change, so the app keeps its own two-method sink. It
// satisfies the same shape the gateway's SchedLogger expects.
type nopHostLogger struct{}

func (nopHostLogger) Info(module, msg string, fields map[string]any) {}
func (nopHostLogger) Warn(module, msg string, fields map[string]any) {}

// hostLogger is the slice of *logging.Logger the gateway needs (Info/Warn);
// typed as an interface so tests can pass nopHostLogger.
type hostLogger interface {
	Info(module, msg string, fields map[string]any)
	Warn(module, msg string, fields map[string]any)
}

// redactOptionsFromConfig maps the config.redact section to the gateway's
// redaction wiring. The zero value (enabled=false) leaves forwarding
// untouched, so a host with no redact section behaves exactly as before.
func redactOptionsFromConfig(c config.Config) gateway.RedactOptions {
	return gateway.RedactOptions{
		On:         c.Redact.Enabled,
		KeyFile:    c.Redact.KeyFile,
		RouteAware: c.Redact.RouteAware,
		Kinds:      c.Redact.Kinds,
		Categories: c.Redact.Categories,
	}
}

// redactOptionsChanged reports whether the two option sets differ; applyRedactLive
// rebuilds the gateway only when they do, so unrelated settings saves keep the
// current handler (its rate-limit windows and caches stay warm).
func redactOptionsChanged(a, b gateway.RedactOptions) bool {
	if a.On != b.On || a.KeyFile != b.KeyFile || a.RouteAware != b.RouteAware {
		return true
	}
	if !sameStringList(a.Kinds, b.Kinds) || !sameStringList(a.Categories, b.Categories) {
		return true
	}
	return false
}

// sameStringList compares two identifier lists as sets (order is irrelevant:
// the gateway folds them into a map).
func sameStringList(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]bool, len(a))
	for _, v := range a {
		seen[v] = true
	}
	for _, v := range b {
		if !seen[v] {
			return false
		}
	}
	return true
}

// redactStatus builds GET /api/redact. Kinds/categories are not credentials
// (the placeholder vocabulary and rule ids), so they cross the admin API
// verbatim; key_file is a power-user field that stays in config.json only.
func (a *App) redactStatus() (webui.RedactStatus, error) {
	cfg := a.configSnapshot().Redact
	return webui.RedactStatus{
		Enabled:    cfg.Enabled,
		RouteAware: cfg.RouteAware,
		Kinds:      append([]string{}, cfg.Kinds...),
		Categories: append([]string{}, cfg.Categories...),
	}, nil
}

// saveRedactConfig applies a partial update. Validation happens here (the
// transport layer only checks shape) so the category ids are checked against
// internal/redact's real rule table; the save then flows through saveSettings
// like every other config write, which also applies the change live.
func (a *App) saveRedactConfig(update webui.RedactUpdate) (webui.RedactStatus, error) {
	next := a.configSnapshot()
	if update.Enabled != nil {
		next.Redact.Enabled = *update.Enabled
	}
	if update.RouteAware != nil {
		next.Redact.RouteAware = *update.RouteAware
	}
	if update.Kinds != nil {
		next.Redact.Kinds = append([]string(nil), (*update.Kinds)...)
	}
	if update.Categories != nil {
		next.Redact.Categories = append([]string(nil), (*update.Categories)...)
	}
	if err := validateRedactCategories(next.Redact.Categories); err != nil {
		return webui.RedactStatus{}, fmt.Errorf("%w: %v", webui.ErrInvalidRequest, err)
	}
	next.Normalize()

	raw, err := json.Marshal(next)
	if err != nil {
		return webui.RedactStatus{}, err
	}
	if err := a.saveSettings(json.RawMessage(raw)); err != nil {
		return webui.RedactStatus{}, err
	}
	return a.redactStatus()
}

// validateRedactCategories rejects category ids that no rule in internal/redact
// knows. Kinds stay open on purpose: the rule table's kind vocabulary grows
// with it and the gateway silently ignores unknown kinds (kindsMap drops the
// lookup miss), so an unknown kind cannot misconfigure anything.
func validateRedactCategories(ids []string) error {
	known := make(map[string]bool, len(redact.Categories))
	for _, c := range redact.Categories {
		known[c.ID] = true
	}
	for _, id := range ids {
		if !known[id] {
			return fmt.Errorf("脱敏类别无效：%q（可选：%v）", id, categoryIDs())
		}
	}
	return nil
}

// categoryIDs lists the rule-table category ids for error messages.
func categoryIDs() []string {
	out := make([]string, 0, len(redact.Categories))
	for _, c := range redact.Categories {
		out = append(out, c.ID)
	}
	return out
}

// applyRedactLive swaps the gateway handler when the redact section changed.
// old is the config before the save; a no-op save (values equal) keeps the
// current handler with its warm caches and rate-limit windows.
func (a *App) applyRedactLive(old, next config.Config) {
	before := redactOptionsFromConfig(old)
	after := redactOptionsFromConfig(next)
	if !redactOptionsChanged(before, after) {
		return
	}
	a.rebuildGateway(next, after)
}

// rebuildGateway constructs a fresh gateway for next and swaps it in under
// gatewayMu. gatewayOptions leaves Plugins unset when no plugin manager is
// wired (tests route custom-model traffic only, which never touches Plugins);
// production always has the manager, so the built handler can route everything
// New() could.
func (a *App) rebuildGateway(cfg config.Config, opts gateway.RedactOptions) {
	var logger hostLogger
	if a.logger != nil {
		logger = a.logger
	} else {
		// Test apps without a wired logger stay silent.
		logger = nopHostLogger{}
	}
	handler := gateway.New(a.gatewayOptions(cfg, logger))
	a.setGatewayHandler(handler)
	logger.Info("app", "gateway rebuilt for redact config change", map[string]any{
		"enabled": opts.On, "route_aware": opts.RouteAware,
		"kinds": len(opts.Kinds), "categories": len(opts.Categories),
		"note": "rate-limit windows and model caches were reset with the old handler",
	})
}

// currentGateway reads the active /v1 handler. The read lock keeps the swap in
// applyRedactLive race-free.
func (a *App) currentGateway() http.Handler {
	a.gatewayMu.RLock()
	defer a.gatewayMu.RUnlock()
	return a.gatewayHandler
}

// setGatewayHandler replaces the active /v1 handler.
func (a *App) setGatewayHandler(h http.Handler) {
	a.gatewayMu.Lock()
	a.gatewayHandler = h
	a.gatewayMu.Unlock()
}

// gatewayOptions assembles the gateway.Options New() serves with. The same
// builder runs at startup and on every redact rebuild so the two paths cannot
// drift. A nil plugin manager leaves Plugins unset (tests route custom-model
// traffic only).
func (a *App) gatewayOptions(cfg config.Config, logger hostLogger) gateway.Options {
	opts := gateway.Options{
		Log:                     apiLogSink{app: a},
		DebugBody:               cfg.Logging.DebugBody,
		CorsAllowOrigins:        cfg.Gateway.CorsAllowOrigins,
		AllowIPs:                cfg.Gateway.AllowIPs,
		TrustedProxies:          cfg.Gateway.TrustedProxies,
		ServiceMode:             cfg.Gateway.ServiceMode,
		RateLimitPerMinute:      cfg.Gateway.RateLimitPerMinute,
		RateLimitPerIPPerMinute: cfg.Gateway.RateLimitPerIPPerMinute,
		// P5 排队等待/排队上限: queue_timeout_seconds=0 (一直等) maps onto the
		// gateway's negative sentinel; queue_max_depth 0 stays unbounded.
		QueueTimeout:  queueTimeoutOption(cfg.Gateway.QueueTimeoutSeconds),
		QueueMaxDepth: cfg.Gateway.QueueMaxDepth,
		// Bare model names route to the configured default plugin only.
		DefaultPlugin: cfg.Gateway.DefaultPlugin,
		Schedule:      a.dispatch,
		Snapshot:      a.schedulerSnapshot,
		SchedLog:      logger,
		Overrides:     a.store.GetModelOverrides,
		Prefs:         a.store.GetModelPrefs,
		// D-33 disabled-model classification (400 model_disabled vs 404).
		ModelCatalogState: a.store.ModelCatalogState,
		KeyRecord:         a.keyRecord,
		KeyUsageToday:     a.keyUsageToday,
		KeyUsageTotal:     a.totalKeyUsage,
		KeyUsage:          a.recordKeyUsage,
		DecisionUpstreams: a.decisionUpstreams,
		PriceFor:          a.priceFor,
		// D-30③ 收官：池级倍率接线（pool price_rate → 计价链）。
		PoolRateFor:       a.poolRateFor,
		// User-defined custom models route straight to their own upstream
		// (Settings → 自定义模型); the key is read from the DPAPI store.
		CustomModels:   a.gatewayCustomModels,
		CustomModelKey: a.customModelKey,
		Redact:         redactOptionsFromConfig(cfg),
	}
	switch {
	case a.gatewayPlugins != nil:
		// Test seam: the redact hot-update tests route plugin traffic through
		// a fake registry instead of a full plugin host.
		opts.Plugins = a.gatewayPlugins
	case a.pluginMgr != nil:
		opts.Plugins = a.pluginMgr
	}
	switch {
	case a.gatewayKeys != nil:
		// Matching test seam: the real keyAdapter needs the DPAPI store.
		opts.Keys = a.gatewayKeys
	default:
		opts.Keys = a.keyVerifier
	}
	return opts
}
