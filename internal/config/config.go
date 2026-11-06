// Package config manages AIWorkGate's non-secret host configuration, stored as
// %APPDATA%\AIWorkGate\config.json. Credentials never belong in this file; see
// the secrets package for DPAPI-protected material.
package config

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"

	"aiworkgate/internal/model"
)

const (
	// CurrentVersion is the schema version written by Default and appended to
	// configurations that predate versioning.
	CurrentVersion = 1

	// FileName is the on-disk name of the configuration file.
	FileName = "config.json"

	DefaultPort         = 7865
	DefaultABaseURL     = "http://127.0.0.1:7864"
	DefaultModelCache   = 10
	DefaultConcurrency  = 2
	DefaultCheckinTime  = "09:00"
	DefaultRetryCount   = 2
	DefaultAPIReqDays   = 30
	DefaultBodyPrevDays = 3
	DefaultCheckinDays  = 180
	DefaultHostLogDays  = 30
	// DefaultProxyLogDays is the retention window of the proxy traffic log. One
	// row is written per tunnelled CONNECT, so this table grows faster than the
	// request log and keeps a shorter window by default.
	DefaultProxyLogDays = 7

	// DefaultNotifyChannel and DefaultNotifyMinInterval mirror the reference
	// product's defaults: disabled until the user configures a channel, and a
	// ten-minute floor between two pushes for the same event (A notifies once
	// per event, not once per account, to stay inside free push quotas).
	DefaultNotifyChannel     = "generic"
	DefaultNotifyMinInterval = 600
	MinNotifyInterval        = 60
	MaxNotifyInterval        = 86400

	StartupAlways   = "always"
	StartupOnDemand = "on_demand"

	MinPort       = 1024
	MaxPort       = 65535
	MinConcurr    = 1
	MaxConcurr    = 32
	MaxRetryCount = 5
	// MinModelCacheModels keeps the A model cache from being refreshed on every
	// request; MaxModelCacheMinutes caps it at one day.
	MinModelCacheMinutes = 1
	MaxModelCacheMinutes = 1440
	MaxRetryBackoff      = 3600

	// Market update check cadence (P2-5): the default mirrors the 6h rhythm of
	// the magpie updater. An interval of 0 disables the timer entirely.
	DefaultMarketUpdateIntervalMinutes = 360
	// MarketUpdatePolicyPrompt (default) records available updates but never
	// installs them; MarketUpdatePolicyAuto is an explicit opt-in that applies
	// updates through the rollback primitive.
	MarketUpdatePolicyPrompt = "prompt"
	MarketUpdatePolicyAuto   = "auto"
)

// DefaultNoProxy lists destinations that must never be sent to a proxy.
var DefaultNoProxy = []string{"127.0.0.1", "localhost"}

// DefaultNotifyEvents is the subscription every fresh install starts with.
// The two P9-2 quota alert kinds are subscribed so the three independent alert
// rules actually reach the user out of the box.
var DefaultNotifyEvents = []string{
	"checkin_fail", "checkin_done", "account_dead", "quota_low",
	"quota_expiring", "quota_resetting",
}

// notifyChannels is the closed set accepted in config.json (unknown values are
// normalized to generic so a hand-edited file still starts).
var notifyChannels = map[string]bool{"generic": true, "bark": true, "serverchan": true, "wecom": true}

// notifyEvents is the closed set of event kinds the host can emit.
var notifyEvents = map[string]bool{
	"checkin_fail": true, "checkin_done": true, "account_dead": true, "quota_low": true,
	// P9-2: the two new quota alert kinds.
	"quota_expiring": true, "quota_resetting": true,
}

var defaultRetryBackoff = []int{5, 20}

// Config is the full non-secret host configuration.
type Config struct {
	Version   int             `json:"version"`
	Port      int             `json:"port"`
	ListenLAN bool            `json:"listen_lan"`
	Autostart bool            `json:"autostart"`
	A         AConfig         `json:"a"`
	Proxy     ProxyConfig     `json:"proxy"`
	Plugins   PluginsConfig   `json:"plugins"`
	Egress    EgressConfig    `json:"egress"`
	Checkin   CheckinConfig   `json:"checkin"`
	Retention RetentionConfig `json:"retention"`
	Logging   LoggingConfig   `json:"logging"`
	Gateway   GatewayConfig   `json:"gateway"`
	Redact    RedactConfig    `json:"redact"`
	Notify    NotifyConfig    `json:"notify"`
	Decision  DecisionConfig  `json:"decision"`
	// RoutingGroups are P11 pattern groups: each id is routable and resolves to
	// the models matching Match at snapshot time (* glob or re:<regexp>).
	RoutingGroups []RoutingGroupConfig `json:"routing_groups"`
}

// RoutingGroupConfig is one pattern-based routing group.
type RoutingGroupConfig struct {
	ID      string   `json:"id"`
	Display string   `json:"display,omitempty"`
	Match   []string `json:"match"`
	// Disabled keeps the entry in config for later use without publishing it.
	Disabled bool `json:"disabled"`
}

// DecisionConfig lists the generic structured-decision upstreams (D-18/P6).
// The API key is not here: it lives in the secret store under
// "decision.<id>.api_key".
type DecisionConfig struct {
	Upstreams []DecisionUpstreamConfig `json:"upstreams"`
}

// DecisionUpstreamConfig is one fixed structured-decision endpoint. Protocol is
// "systemone" (default) or "decision"; the model is normally JEV.
type DecisionUpstreamConfig struct {
	ID       string `json:"id"`
	BaseURL  string `json:"base_url"`
	Protocol string `json:"protocol"`
	ModelID  string `json:"model_id"`
	// Disabled is the opt-out spelling so a hand-written entry works without
	// remembering to set enabled=true.
	Disabled bool `json:"disabled"`
}

// NotifyConfig holds the optional out-of-app notification channel. The webhook
// URL is deliberately absent: it lives in the DPAPI secret store under
// "notify.url" because a webhook URL is a bearer credential.
type NotifyConfig struct {
	Enabled            bool     `json:"enabled"`
	Channel            string   `json:"channel"`
	Events             []string `json:"events"`
	MinIntervalSeconds int      `json:"min_interval_seconds"`
}

// GatewayConfig holds the public /v1 surface switches that are not about
// upstream routing.
type GatewayConfig struct {
	// CorsAllowOrigins lists browser origins allowed to read /v1 responses.
	// Empty means the gateway sends no CORS headers and rejects preflight, so
	// only non-browser clients (which cannot be tricked by a web page) work.
	CorsAllowOrigins []string `json:"cors_allow_origins"`

	// AllowIPs is the global source-address allowlist (D-40), checked before
	// authentication. Entries are IPs or CIDRs; empty fails safe to loopback
	// only. See TrustedProxies for reverse-proxy deployments.
	AllowIPs []string `json:"allow_ips"`

	// TrustedProxies lists reverse proxies whose X-Forwarded-For is believed
	// (P5-4). Empty means never trust the header, so direct clients cannot
	// spoof their source.
	TrustedProxies []string `json:"trusted_proxies"`

	// ServiceMode is open (default), paused (every /v1 request gets 503) or
	// whitelist (only keys marked as owner are admitted), D-11.
	ServiceMode string `json:"service_mode"`

	// RateLimitPerMinute caps each key's requests per rolling minute; 0 = off.
	RateLimitPerMinute int `json:"rate_limit_per_minute"`
	// RateLimitPerIPPerMinute caps each source IP before authentication;
	// 0 = off. Heavy protection belongs at the reverse proxy (P5-5).
	RateLimitPerIPPerMinute int `json:"rate_limit_per_ip_per_minute"`

	// PublicHosts lists the hostnames a reverse proxy may send while the host
	// itself is loopback-only. Without it, the DNS-rebinding guard rejects a
	// public Host and every proxied request gets 403 (P4/P5).
	PublicHosts []string `json:"public_hosts"`

	// DefaultPlugin serves bare model names (no "id/" prefix). Empty means a
	// bare name is answered with 404 model_not_found.
	DefaultPlugin string `json:"default_plugin"`
	// LegacyAFallback keeps the pre-plugin A upstream as the bare-name fallback
	// when DefaultPlugin is empty. It exists only until A itself becomes a
	// plugin; set it to false to make bare names 404 without any code change.
	LegacyAFallback bool `json:"legacy_a_fallback"`
}

// AConfig describes how to reach the AI Work 助手 (A) gateway.
type AConfig struct {
	BaseURL           string `json:"base_url"`
	ExePath           string `json:"exe_path"`
	AutoStart         bool   `json:"auto_start"`
	ModelCacheMinutes int    `json:"model_cache_minutes"`
}

// ProxyConfig holds the optional upstream proxy used by plugins.
type ProxyConfig struct {
	Enabled bool     `json:"enabled"`
	HTTP    string   `json:"http"`
	HTTPS   string   `json:"https"`
	NoProxy []string `json:"no_proxy"`
}

// PluginsConfig holds plugin process defaults.
type PluginsConfig struct {
	Concurrency  int    `json:"concurrency"`
	StartupMode  string `json:"startup_mode"`
	StrictEgress bool   `json:"strict_egress"`

	// SandboxUser is the Linux account plugin child processes are spawned under
	// (P8-① uid isolation, internal/plugins.Options.PluginUser). Empty (the
	// default) = off: children keep running as the host user. Opt-in semantics:
	// when set, the host must run as root and the account must exist, otherwise
	// the supervisor falls back to the host uid and logs a warning. Windows
	// builds ignore it.
	SandboxUser string `json:"sandbox_user"`

	// MarketMirror selects the npm registry base for the plugin market (P2-4).
	// Empty (the default) = the official https://registry.npmjs.org; set e.g.
	// "https://registry.npmmirror.com" to use a domestic mirror. This is a
	// direct selection, not an automatic failover.
	MarketMirror string `json:"market_mirror"`

	// MarketUpdateIntervalMinutes is the cadence of the market update check
	// (P2-5). 0 disables the timer; the default (360) mirrors the 6h rhythm of
	// the magpie updater.
	MarketUpdateIntervalMinutes int `json:"market_update_interval_minutes"`
	// MarketUpdatePolicy is "prompt" (default: record available updates, never
	// install) or "auto" (explicit opt-in: apply updates through the rollback
	// primitive). Any other value normalizes to prompt.
	MarketUpdatePolicy string `json:"market_update_policy"`

	// TrustedKeys maps a signing key id to its base64 Ed25519 public key.
	// Empty means unsigned packages install as before. Once any key is
	// configured every install must carry manifest.sig signed by one of these
	// keys: an installed plugin is a process running as the gateway user.
	TrustedKeys map[string]string `json:"trusted_keys,omitempty"`

	// QuotaAlerts holds the global defaults for the three independent quota
	// alert rules (P9-2, D-21③). A zero field disables that rule.
	QuotaAlerts QuotaAlertsConfig `json:"quota_alerts"`

	// QuotaAlertPluginOverrides maps a plugin id to per-plugin overrides of
	// the alert thresholds (D-21③: a platform whose voucher only lives 3 days
	// needs a shorter window than the global 7). A nil field in an override
	// keeps the global default.
	QuotaAlertPluginOverrides map[string]QuotaAlertPluginOverride `json:"quota_alert_plugin_overrides,omitempty"`
}

// QuotaAlertsConfig is the global defaults for the three quota alert rules.
type QuotaAlertsConfig struct {
	// RunsOutWarningDays is how many days before runsOutAt (or legacy
	// expires_at) the "即将到期" rule fires.
	RunsOutWarningDays int `json:"runs_out_warning_days"`
	// ResetWarningDays is how many days before resetsAt the "即将重置" rule
	// fires.
	ResetWarningDays int `json:"reset_warning_days"`
	// ResetWarningUsageRatio is the used/limit ratio below which the
	// "即将重置" rule fires (most of the quota will reset unused).
	ResetWarningUsageRatio float64 `json:"reset_warning_usage_ratio"`
	// BalanceLowRatio is the used/limit ratio at/above which the "余额不足"
	// rule fires.
	BalanceLowRatio float64 `json:"balance_low_ratio"`
	// BalanceLowAbsolute is the absolute remaining threshold used when the
	// plugin reports no total (legacy single-group data often has total=0).
	BalanceLowAbsolute float64 `json:"balance_low_absolute"`
}

// QuotaAlertPluginOverride overrides individual alert thresholds for one
// plugin; a nil field keeps the global default.
type QuotaAlertPluginOverride struct {
	RunsOutWarningDays     *int     `json:"runs_out_warning_days,omitempty"`
	ResetWarningDays       *int     `json:"reset_warning_days,omitempty"`
	ResetWarningUsageRatio *float64 `json:"reset_warning_usage_ratio,omitempty"`
	BalanceLowRatio        *float64 `json:"balance_low_ratio,omitempty"`
	BalanceLowAbsolute     *float64 `json:"balance_low_absolute,omitempty"`
}

// ThresholdsFor resolves the effective thresholds for one plugin: the global
// defaults with the plugin's overrides applied on top. A zero global field
// stays zero (rule disabled) unless an override fills it.
func (c QuotaAlertsConfig) ThresholdsFor(pluginID string, overrides map[string]QuotaAlertPluginOverride) model.QuotaAlertThresholds {
	base := model.DefaultQuotaAlertThresholds()
	// The config file may carry explicit zeros meaning "disabled", so start
	// from the file's values rather than the model defaults when they are set.
	if c.RunsOutWarningDays != 0 {
		base.RunsOutWarningDays = c.RunsOutWarningDays
	}
	if c.ResetWarningDays != 0 {
		base.ResetWarningDays = c.ResetWarningDays
	}
	if c.ResetWarningUsageRatio != 0 {
		base.ResetWarningUsageRatio = c.ResetWarningUsageRatio
	}
	if c.BalanceLowRatio != 0 {
		base.BalanceLowRatio = c.BalanceLowRatio
	}
	if c.BalanceLowAbsolute != 0 {
		base.BalanceLowAbsolute = c.BalanceLowAbsolute
	}
	ov, ok := overrides[pluginID]
	if !ok {
		return base
	}
	if ov.RunsOutWarningDays != nil {
		base.RunsOutWarningDays = *ov.RunsOutWarningDays
	}
	if ov.ResetWarningDays != nil {
		base.ResetWarningDays = *ov.ResetWarningDays
	}
	if ov.ResetWarningUsageRatio != nil {
		base.ResetWarningUsageRatio = *ov.ResetWarningUsageRatio
	}
	if ov.BalanceLowRatio != nil {
		base.BalanceLowRatio = *ov.BalanceLowRatio
	}
	if ov.BalanceLowAbsolute != nil {
		base.BalanceLowAbsolute = *ov.BalanceLowAbsolute
	}
	return base
}

// EgressConfig holds the host's own egress accounting switches. It is separate
// from ProxyConfig, which is the upstream proxy plugins are told to use: these
// are booleans about what the host records.
type EgressConfig struct {
	// DirectLogging makes the host bind one loopback CONNECT listener per
	// direct-mode account/plugin and point the plugin child environment at it,
	// so unproxied egress also lands in the proxy traffic log. It is off by
	// default: direct mode then keeps dialling by itself, which is the behaviour
	// every plugin shipped with.
	DirectLogging bool `json:"direct_logging"`
}

// CheckinConfig holds the daily check-in scheduler defaults.
type CheckinConfig struct {
	DefaultTime         string `json:"default_time"`
	RetryCount          int    `json:"retry_count"`
	RetryBackoffSeconds []int  `json:"retry_backoff_seconds"`
}

// RetentionConfig holds log retention windows in days.
type RetentionConfig struct {
	APIRequestDays  int `json:"api_request_days"`
	BodyPreviewDays int `json:"body_preview_days"`
	CheckinDays     int `json:"checkin_days"`
	HostLogDays     int `json:"host_log_days"`
	// ProxyLogDays bounds the proxy traffic log. It is separate from
	// APIRequestDays because one CONNECT tunnel produces one row.
	ProxyLogDays int `json:"proxy_log_days"`
}

// LoggingConfig holds logging switches.
type LoggingConfig struct {
	DebugBody bool `json:"debug_body"`
}

// RedactConfig holds the P10 request-redaction switches (internal/redact).
// Off by default this phase: configurable first, flipped to default-on later
// (P10 计划：本阶段先可配后默认开)。
type RedactConfig struct {
	// Enabled turns request-body masking and response restoring on.
	Enabled bool `json:"enabled"`
	// KeyFile is where the placeholder fingerprint key is kept across restarts
	// (redact.SetKeyPath). Empty (the default) resolves to
	// <dataDir>\redact.key at Normalize time.
	KeyFile string `json:"key_file"`
	// RouteAware keeps a text masked for a provider masked for that provider
	// across turns (sent spans, protecting the upstream prompt cache). Default
	// true.
	RouteAware bool `json:"route_aware"`
	// Kinds, when non-empty, restricts masking to these personal-data kinds
	// (SECRET/EMAIL/PHONE/…). Empty = the rules' default kind table.
	Kinds []string `json:"kinds"`
	// Categories, when non-empty, restricts masking to these category ids
	// (ssn/passport/iban/birthday/mac/serial/home/userhost/ip/bucket). Empty =
	// the default category table (internal/redact more.go Categories). When
	// both Kinds and Categories are set, the union of the two is masked.
	Categories []string `json:"categories"`
}

// Default returns the built-in configuration used on first run.
func Default() Config {
	return Config{
		Version:   CurrentVersion,
		Port:      DefaultPort,
		ListenLAN: false,
		Autostart: true,
		A: AConfig{
			BaseURL:           DefaultABaseURL,
			ExePath:           "",
			AutoStart:         false,
			ModelCacheMinutes: DefaultModelCache,
		},
		Proxy: ProxyConfig{
			Enabled: false,
			HTTP:    "",
			HTTPS:   "",
			NoProxy: append([]string(nil), DefaultNoProxy...),
		},
		Plugins: PluginsConfig{
			Concurrency:                 DefaultConcurrency,
			StartupMode:                 StartupAlways,
			StrictEgress:                false,
			MarketUpdateIntervalMinutes: DefaultMarketUpdateIntervalMinutes,
			MarketUpdatePolicy:          MarketUpdatePolicyPrompt,
			QuotaAlerts: QuotaAlertsConfig{
				RunsOutWarningDays:     7,
				ResetWarningDays:       7,
				ResetWarningUsageRatio: 0.30,
				BalanceLowRatio:        0.90,
				BalanceLowAbsolute:     100,
			},
		},
		// Direct egress is recorded only when the user opts in: turning it on
		// binds loopback listeners that plugins route through, so the shipped
		// default keeps the pre-existing "plugin dials by itself" behaviour.
		Egress: EgressConfig{DirectLogging: false},
		Checkin: CheckinConfig{
			DefaultTime:         DefaultCheckinTime,
			RetryCount:          DefaultRetryCount,
			RetryBackoffSeconds: append([]int(nil), defaultRetryBackoff...),
		},
		Retention: RetentionConfig{
			APIRequestDays:  DefaultAPIReqDays,
			BodyPreviewDays: DefaultBodyPrevDays,
			CheckinDays:     DefaultCheckinDays,
			HostLogDays:     DefaultHostLogDays,
			ProxyLogDays:    DefaultProxyLogDays,
		},
		Logging: LoggingConfig{DebugBody: false},
		Gateway: GatewayConfig{
			// Bare model names keep reaching A until the plugin layer replaces
			// it; a fresh install must not break existing client configs.
			DefaultPlugin:   "",
			LegacyAFallback: true,
			// D-40: /v1 answers the local machine only until the user names the
			// networks that may reach it.
			AllowIPs:    []string{"127.0.0.1", "::1"},
			ServiceMode: "open",
		},
		// Redaction is off by default this phase; Route awareness on.
		Redact: RedactConfig{RouteAware: true},
		Notify: NotifyConfig{
			Enabled:            false,
			Channel:            DefaultNotifyChannel,
			Events:             append([]string(nil), DefaultNotifyEvents...),
			MinIntervalSeconds: DefaultNotifyMinInterval,
		},
	}
}

// Normalize fills empty identifiers with defaults and clamps numeric fields
// into their supported ranges. It is always safe to call on a zero Config.
func (c *Config) Normalize() {
	if c.Version < CurrentVersion {
		c.Version = CurrentVersion
	}
	c.Port = clampInt(c.Port, MinPort, MaxPort)

	c.A.BaseURL = strings.TrimSpace(c.A.BaseURL)
	if c.A.BaseURL == "" {
		c.A.BaseURL = DefaultABaseURL
	}
	c.A.ModelCacheMinutes = clampInt(c.A.ModelCacheMinutes, MinModelCacheMinutes, MaxModelCacheMinutes)

	if c.Proxy.NoProxy == nil {
		c.Proxy.NoProxy = append([]string(nil), DefaultNoProxy...)
	}

	c.Plugins.Concurrency = clampInt(c.Plugins.Concurrency, MinConcurr, MaxConcurr)
	if c.Plugins.StartupMode == "" {
		c.Plugins.StartupMode = StartupAlways
	}
	if c.Plugins.MarketUpdateIntervalMinutes < 0 {
		c.Plugins.MarketUpdateIntervalMinutes = 0
	}
	c.Plugins.MarketUpdatePolicy = strings.ToLower(strings.TrimSpace(c.Plugins.MarketUpdatePolicy))
	if c.Plugins.MarketUpdatePolicy != MarketUpdatePolicyAuto {
		c.Plugins.MarketUpdatePolicy = MarketUpdatePolicyPrompt
	}
	if len(c.Plugins.TrustedKeys) > 0 {
		keys := make(map[string]string, len(c.Plugins.TrustedKeys))
		for id, key := range c.Plugins.TrustedKeys {
			id = strings.TrimSpace(id)
			key = strings.TrimSpace(key)
			if id != "" && key != "" {
				keys[id] = key
			}
		}
		c.Plugins.TrustedKeys = keys
	}

	c.Checkin.DefaultTime = strings.TrimSpace(c.Checkin.DefaultTime)
	if c.Checkin.DefaultTime == "" {
		c.Checkin.DefaultTime = DefaultCheckinTime
	}
	c.Checkin.RetryCount = clampInt(c.Checkin.RetryCount, 0, MaxRetryCount)
	if c.Checkin.RetryBackoffSeconds == nil {
		c.Checkin.RetryBackoffSeconds = append([]int(nil), defaultRetryBackoff...)
	}
	for i, v := range c.Checkin.RetryBackoffSeconds {
		if v < 0 {
			c.Checkin.RetryBackoffSeconds[i] = 0
		} else if v > MaxRetryBackoff {
			c.Checkin.RetryBackoffSeconds[i] = MaxRetryBackoff
		}
	}

	c.Retention.APIRequestDays = atLeast(c.Retention.APIRequestDays, 1)
	c.Retention.BodyPreviewDays = atLeast(c.Retention.BodyPreviewDays, 1)
	c.Retention.CheckinDays = atLeast(c.Retention.CheckinDays, 1)
	c.Retention.HostLogDays = atLeast(c.Retention.HostLogDays, 1)
	c.Retention.ProxyLogDays = atLeast(c.Retention.ProxyLogDays, 1)

	c.Gateway.CorsAllowOrigins = normalizeOriginList(c.Gateway.CorsAllowOrigins)
	c.Gateway.AllowIPs = normalizeAllowIPs(c.Gateway.AllowIPs)
	c.Gateway.TrustedProxies = normalizeIPList(c.Gateway.TrustedProxies, nil)
	c.Gateway.ServiceMode = normalizeServiceMode(c.Gateway.ServiceMode)
	c.Decision.Upstreams = normalizeDecisionUpstreams(c.Decision.Upstreams)
	c.RoutingGroups = normalizeRoutingGroups(c.RoutingGroups)
	c.Gateway.RateLimitPerMinute = clampRateLimit(c.Gateway.RateLimitPerMinute)
	c.Gateway.RateLimitPerIPPerMinute = clampRateLimit(c.Gateway.RateLimitPerIPPerMinute)
	c.Gateway.PublicHosts = normalizePublicHosts(c.Gateway.PublicHosts)
	c.Gateway.DefaultPlugin = strings.ToLower(strings.TrimSpace(c.Gateway.DefaultPlugin))

	// Redact: default key file lives beside the data dir; kinds/categories
	// overrides are trimmed and deduped, empty keeps the default tables.
	c.Redact.KeyFile = strings.TrimSpace(c.Redact.KeyFile)
	if c.Redact.KeyFile == "" {
		if dir, err := DataDir(); err == nil {
			c.Redact.KeyFile = filepath.Join(dir, "redact.key")
		}
	}
	c.Redact.Kinds = dedupeStrings(c.Redact.Kinds)
	c.Redact.Categories = dedupeStrings(c.Redact.Categories)

	c.normalizeNotify()
}

// dedupeStrings trims each entry, drops blanks and keeps first occurrences.
func dedupeStrings(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// normalizeNotify folds an unknown channel to generic, drops unknown event
// names and clamps the throttle window. Events left nil (an older config file)
// keep the default subscription; an explicit empty list means "notify nothing".
func (c *Config) normalizeNotify() {
	c.Notify.Channel = strings.ToLower(strings.TrimSpace(c.Notify.Channel))
	if !notifyChannels[c.Notify.Channel] {
		c.Notify.Channel = DefaultNotifyChannel
	}
	if c.Notify.Events == nil {
		c.Notify.Events = append([]string(nil), DefaultNotifyEvents...)
	}
	events := make([]string, 0, len(c.Notify.Events))
	seen := map[string]bool{}
	for _, raw := range c.Notify.Events {
		event := strings.ToLower(strings.TrimSpace(raw))
		if !notifyEvents[event] || seen[event] {
			continue
		}
		seen[event] = true
		events = append(events, event)
	}
	c.Notify.Events = events
	if c.Notify.MinIntervalSeconds <= 0 {
		c.Notify.MinIntervalSeconds = DefaultNotifyMinInterval
	} else {
		c.Notify.MinIntervalSeconds = clampInt(c.Notify.MinIntervalSeconds, MinNotifyInterval, MaxNotifyInterval)
	}
}

// normalizeOriginList trims, de-duplicates and drops empty entries. Invalid
// entries are kept so Validate can report them instead of silently discarding
// what the user typed.
func normalizeOriginList(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, raw := range in {
		origin := strings.TrimSpace(raw)
		if origin == "" {
			continue
		}
		key := strings.ToLower(strings.TrimRight(origin, "/"))
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, origin)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

var timeOfDayPattern = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

// Service modes (D-11).
const (
	ServiceModeOpen      = "open"
	ServiceModePaused    = "paused"
	ServiceModeWhitelist = "whitelist"
)

// normalizeServiceMode folds unknown/empty values to open so a hand-edited
// config still starts.
func normalizeServiceMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case ServiceModePaused:
		return ServiceModePaused
	case ServiceModeWhitelist:
		return ServiceModeWhitelist
	default:
		return ServiceModeOpen
	}
}

// clampRateLimit keeps a hand-edited request-per-minute value sane.
func clampRateLimit(value int) int {
	if value < 0 {
		return 0
	}
	if value > 1_000_000 {
		return 1_000_000
	}
	return value
}

// normalizeRoutingGroups trims ids/patterns, dedupes ids and drops a group
// whose id is empty, whose match list is empty, or whose re: pattern is broken.
func normalizeRoutingGroups(in []RoutingGroupConfig) []RoutingGroupConfig {
	out := make([]RoutingGroupConfig, 0, len(in))
	seen := map[string]bool{}
	for _, raw := range in {
		group := raw
		group.ID = strings.TrimSpace(group.ID)
		group.Display = strings.TrimSpace(group.Display)
		if group.ID == "" || seen[group.ID] ||
			strings.ContainsAny(group.ID, "/ \t") {
			// A slash would make the id look like a "<plugin>/<model>" name.
			continue
		}
		match := make([]string, 0, len(group.Match))
		valid := true
		for _, pattern := range group.Match {
			pattern = strings.TrimSpace(pattern)
			if pattern == "" {
				continue
			}
			if strings.HasPrefix(pattern, "re:") {
				if _, err := regexp.Compile(strings.TrimSpace(strings.TrimPrefix(pattern, "re:"))); err != nil {
					valid = false
					break
				}
			}
			match = append(match, pattern)
		}
		if !valid || len(match) == 0 {
			continue
		}
		group.Match = match
		seen[group.ID] = true
		out = append(out, group)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// normalizeDecisionUpstreams trims entries, defaults the protocol and drops
// entries that cannot be called (bad URL, missing model). IDs are deduped.
func normalizeDecisionUpstreams(in []DecisionUpstreamConfig) []DecisionUpstreamConfig {
	out := make([]DecisionUpstreamConfig, 0, len(in))
	seen := map[string]bool{}
	for _, raw := range in {
		entry := raw
		entry.ID = strings.TrimSpace(entry.ID)
		entry.BaseURL = strings.TrimRight(strings.TrimSpace(entry.BaseURL), "/")
		entry.ModelID = strings.TrimSpace(entry.ModelID)
		switch strings.ToLower(strings.TrimSpace(entry.Protocol)) {
		case "decision":
			entry.Protocol = "decision"
		default:
			entry.Protocol = "systemone"
		}
		if entry.BaseURL == "" || entry.ModelID == "" {
			continue
		}
		parsed, err := url.Parse(entry.BaseURL)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			continue
		}
		if entry.ID == "" {
			entry.ID = entry.Protocol + "-" + entry.ModelID
		}
		if seen[entry.ID] {
			continue
		}
		seen[entry.ID] = true
		out = append(out, entry)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// normalizePublicHosts keeps bare host[:port] values lower-cased and deduped;
// a pasted scheme or path is dropped rather than silently accepted.
func normalizePublicHosts(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, raw := range in {
		value := strings.ToLower(strings.TrimSpace(raw))
		if value == "" {
			continue
		}
		if idx := strings.Index(value, "://"); idx >= 0 {
			value = value[idx+3:]
		}
		if idx := strings.IndexAny(value, "/?#"); idx >= 0 {
			value = value[:idx]
		}
		if value == "" || seen[value] || strings.ContainsAny(value, " \t") {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// normalizeAllowIPs keeps valid IP/CIDR entries, dedupes them and falls back to
// loopback when nothing valid remains (D-40).
func normalizeAllowIPs(in []string) []string {
	return normalizeIPList(in, []string{"127.0.0.1", "::1"})
}

// normalizeIPList trims/dedupes/validates IP and CIDR entries, dropping
// invalid ones and returning fallback when nothing valid remains.
func normalizeIPList(in []string, fallback []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	add := func(value string) {
		if value == "" || seen[value] {
			return
		}
		seen[value] = true
		out = append(out, value)
	}
	for _, raw := range in {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		if ip := net.ParseIP(value); ip != nil {
			add(ip.String())
			continue
		}
		if _, network, err := net.ParseCIDR(value); err == nil {
			add(network.String())
		}
	}
	if len(out) == 0 {
		return fallback
	}
	return out
}

// pluginIDPattern is the plugin id shape accepted in gateway.default_plugin. It
// mirrors the manifest contract: a lowercase letter, then lowercase letters,
// digits, "_" or "-".
var pluginIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// Validate reports whether the configuration is internally consistent. Call
// Normalize first; Validate does not repair values.
func (c Config) Validate() error {
	if c.Version < CurrentVersion {
		return fmt.Errorf("config: unsupported version %d", c.Version)
	}
	if c.Port < MinPort || c.Port > MaxPort {
		return fmt.Errorf("config: port %d out of range %d-%d", c.Port, MinPort, MaxPort)
	}
	if c.A.ModelCacheMinutes < MinModelCacheMinutes || c.A.ModelCacheMinutes > MaxModelCacheMinutes {
		return fmt.Errorf("config: a.model_cache_minutes %d out of range %d-%d", c.A.ModelCacheMinutes, MinModelCacheMinutes, MaxModelCacheMinutes)
	}
	if err := validateBaseURL(c.A.BaseURL); err != nil {
		return err
	}
	if c.Plugins.Concurrency < MinConcurr || c.Plugins.Concurrency > MaxConcurr {
		return fmt.Errorf("config: plugins.concurrency %d out of range %d-%d", c.Plugins.Concurrency, MinConcurr, MaxConcurr)
	}
	switch c.Plugins.StartupMode {
	case StartupAlways, StartupOnDemand:
	default:
		return fmt.Errorf("config: plugins.startup_mode %q must be %q or %q", c.Plugins.StartupMode, StartupAlways, StartupOnDemand)
	}
	for id, key := range c.Plugins.TrustedKeys {
		raw, err := base64.StdEncoding.DecodeString(key)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return fmt.Errorf("config: plugins.trusted_keys[%q] must be a base64 Ed25519 public key", id)
		}
	}
	if !timeOfDayPattern.MatchString(c.Checkin.DefaultTime) {
		return fmt.Errorf("config: checkin.default_time %q must be HH:MM", c.Checkin.DefaultTime)
	}
	if c.Checkin.RetryCount < 0 || c.Checkin.RetryCount > MaxRetryCount {
		return fmt.Errorf("config: checkin.retry_count %d out of range 0-%d", c.Checkin.RetryCount, MaxRetryCount)
	}
	retention := []struct {
		name string
		v    int
	}{
		{"retention.api_request_days", c.Retention.APIRequestDays},
		{"retention.body_preview_days", c.Retention.BodyPreviewDays},
		{"retention.checkin_days", c.Retention.CheckinDays},
		{"retention.host_log_days", c.Retention.HostLogDays},
		{"retention.proxy_log_days", c.Retention.ProxyLogDays},
	}
	for _, r := range retention {
		if r.v < 1 {
			return fmt.Errorf("config: %s must be >= 1, got %d", r.name, r.v)
		}
	}
	for _, origin := range c.Gateway.CorsAllowOrigins {
		if origin == "*" {
			continue
		}
		u, err := url.Parse(origin)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("config: gateway.cors_allow_origins %q must be \"*\" or an http(s) origin with a host", origin)
		}
	}
	// An empty default plugin is valid (no default); a non-empty one only has to
	// look like a plugin id. Whether that plugin is installed is deliberately
	// not checked: plugins can be installed after the setting is saved.
	if c.Gateway.DefaultPlugin != "" && !pluginIDPattern.MatchString(c.Gateway.DefaultPlugin) {
		return fmt.Errorf("config: invalid default_plugin %q: must match ^[a-z][a-z0-9_-]*$",
			c.Gateway.DefaultPlugin)
	}
	if !notifyChannels[c.Notify.Channel] {
		return fmt.Errorf("config: notify.channel %q must be generic, bark, serverchan or wecom", c.Notify.Channel)
	}
	if c.Notify.MinIntervalSeconds < MinNotifyInterval || c.Notify.MinIntervalSeconds > MaxNotifyInterval {
		return fmt.Errorf("config: notify.min_interval_seconds %d out of range %d-%d",
			c.Notify.MinIntervalSeconds, MinNotifyInterval, MaxNotifyInterval)
	}
	for _, event := range c.Notify.Events {
		if !notifyEvents[event] {
			return fmt.Errorf("config: notify.events contains unknown kind %q", event)
		}
	}
	return nil
}

func validateBaseURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return errors.New("config: a.base_url is empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("config: a.base_url %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("config: a.base_url %q must use http or https", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("config: a.base_url %q has no host", raw)
	}
	return nil
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func atLeast(v, lo int) int {
	if v < lo {
		return lo
	}
	return v
}
