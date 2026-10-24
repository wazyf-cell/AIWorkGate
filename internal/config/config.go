// Package config manages AIWorkGate's non-secret host configuration, stored as
// %APPDATA%\AIWorkGate\config.json. Credentials never belong in this file; see
// the secrets package for DPAPI-protected material.
package config

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
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
)

// DefaultNoProxy lists destinations that must never be sent to a proxy.
var DefaultNoProxy = []string{"127.0.0.1", "localhost"}

// DefaultNotifyEvents is the subscription every fresh install starts with.
var DefaultNotifyEvents = []string{"checkin_fail", "checkin_done", "account_dead", "quota_low"}

// notifyChannels is the closed set accepted in config.json (unknown values are
// normalized to generic so a hand-edited file still starts).
var notifyChannels = map[string]bool{"generic": true, "bark": true, "serverchan": true, "wecom": true}

// notifyEvents is the closed set of event kinds the host can emit.
var notifyEvents = map[string]bool{
	"checkin_fail": true, "checkin_done": true, "account_dead": true, "quota_low": true,
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
	Notify    NotifyConfig    `json:"notify"`
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

	// TrustedKeys maps a signing key id to its base64 Ed25519 public key.
	// Empty means unsigned packages install as before. Once any key is
	// configured every install must carry manifest.sig signed by one of these
	// keys: an installed plugin is a process running as the gateway user.
	TrustedKeys map[string]string `json:"trusted_keys,omitempty"`
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
			Concurrency:  DefaultConcurrency,
			StartupMode:  StartupAlways,
			StrictEgress: false,
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
		},
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
	c.Gateway.DefaultPlugin = strings.ToLower(strings.TrimSpace(c.Gateway.DefaultPlugin))
	c.normalizeNotify()
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
