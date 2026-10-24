package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDefault(t *testing.T) {
	got := Default()
	want := Config{
		Version:   1,
		Port:      7865,
		ListenLAN: false,
		Autostart: true,
		A: AConfig{
			BaseURL:           "http://127.0.0.1:7864",
			ExePath:           "",
			AutoStart:         false,
			ModelCacheMinutes: 10,
		},
		Proxy: ProxyConfig{
			Enabled: false,
			HTTP:    "",
			HTTPS:   "",
			NoProxy: []string{"127.0.0.1", "localhost"},
		},
		Plugins: PluginsConfig{
			Concurrency:  2,
			StartupMode:  "always",
			StrictEgress: false,
		},
		Checkin: CheckinConfig{
			DefaultTime:         "09:00",
			RetryCount:          2,
			RetryBackoffSeconds: []int{5, 20},
		},
		Retention: RetentionConfig{
			APIRequestDays:  30,
			BodyPreviewDays: 3,
			CheckinDays:     180,
			HostLogDays:     30,
			ProxyLogDays:    7,
		},
		Logging: LoggingConfig{DebugBody: false},
		Gateway: GatewayConfig{DefaultPlugin: "", LegacyAFallback: true},
		Notify: NotifyConfig{
			Enabled:            false,
			Channel:            "generic",
			Events:             []string{"checkin_fail", "checkin_done", "account_dead", "quota_low"},
			MinIntervalSeconds: 600,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Default() mismatch:\n got %+v\nwant %+v", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("Default().Validate() = %v, want nil", err)
	}
}

func TestLoadMissingFileCreatesDefault(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !reflect.DeepEqual(cfg, Default()) {
		t.Fatalf("Load() on empty dir = %+v, want Default()", cfg)
	}
	path := ConfigPath(dir)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("config.json not written: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("config.json is empty")
	}
	// Second load must read the file that the first load created.
	again, err := Load(dir)
	if err != nil {
		t.Fatalf("second Load() error = %v", err)
	}
	if !reflect.DeepEqual(again, cfg) {
		t.Fatalf("second Load() = %+v, want %+v", again, cfg)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := Default()
	cfg.Port = 7870
	cfg.ListenLAN = true
	cfg.Autostart = false
	cfg.A.ExePath = `C:\Program Files\AI Work\A.exe`
	cfg.A.AutoStart = true
	cfg.A.ModelCacheMinutes = 30
	cfg.Proxy.Enabled = true
	cfg.Proxy.HTTP = "http://127.0.0.1:7890"
	cfg.Proxy.HTTPS = "http://127.0.0.1:7890"
	cfg.Proxy.NoProxy = []string{"127.0.0.1", "localhost", "*.internal"}
	cfg.Plugins.Concurrency = 8
	cfg.Plugins.StartupMode = StartupOnDemand
	cfg.Plugins.StrictEgress = true
	cfg.Checkin.DefaultTime = "08:30"
	cfg.Checkin.RetryCount = 4
	cfg.Checkin.RetryBackoffSeconds = []int{1, 2, 4}
	cfg.Retention.APIRequestDays = 14
	cfg.Retention.BodyPreviewDays = 2
	cfg.Retention.CheckinDays = 60
	cfg.Retention.HostLogDays = 7
	cfg.Logging.DebugBody = true
	cfg.Gateway.CorsAllowOrigins = []string{"http://localhost:7865", "https://app.test"}

	if err := Save(dir, cfg); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !reflect.DeepEqual(got, cfg) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, cfg)
	}
}

func TestSaveAtomic(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		cfg := Default()
		cfg.Port = 7865 + i
		if err := Save(dir, cfg); err != nil {
			t.Fatalf("Save() #%d error = %v", i, err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != FileName {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected only %s, found %v", FileName, names)
	}

	raw, err := os.ReadFile(ConfigPath(dir))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !json.Valid(raw) {
		t.Fatalf("config.json is not valid JSON: %q", raw)
	}
	var parsed Config
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if parsed.Port != 7867 {
		t.Fatalf("port = %d, want 7867 (last save)", parsed.Port)
	}
}

func TestSaveRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	cfg := Default()
	cfg.Plugins.StartupMode = "sometimes"
	if err := Save(dir, cfg); err == nil {
		t.Fatal("Save() accepted invalid startup_mode")
	}
	if _, err := os.Stat(ConfigPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("invalid config was written to disk (stat err = %v)", err)
	}
}

func TestLoadIgnoresUnknownFields(t *testing.T) {
	dir := t.TempDir()
	raw := `{"port": 9000, "future_option": {"nested": [1,2,3]}, "logging": {"debug_body": true, "extra": 1}}`
	if err := os.WriteFile(ConfigPath(dir), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Port != 9000 {
		t.Fatalf("port = %d, want 9000", cfg.Port)
	}
	if !cfg.Logging.DebugBody {
		t.Fatal("debug_body = false, want true")
	}
	// Absent fields keep their defaults.
	if cfg.A.BaseURL != DefaultABaseURL || !reflect.DeepEqual(cfg.Proxy.NoProxy, []string{"127.0.0.1", "localhost"}) {
		t.Fatalf("defaults lost for absent fields: %+v", cfg)
	}
}

// TestCorsOriginsWireKey pins the on-disk key name: the web UI posts
// gateway.cors_allow_origins, so the JSON tag is part of the contract.
func TestCorsOriginsWireKey(t *testing.T) {
	dir := t.TempDir()
	cfg := Default()
	cfg.Gateway.CorsAllowOrigins = []string{"http://app.test"}
	if err := Save(dir, cfg); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	raw, err := os.ReadFile(ConfigPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	var onDisk map[string]json.RawMessage
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("parse %s: %v", raw, err)
	}
	var gateway map[string]json.RawMessage
	if err := json.Unmarshal(onDisk["gateway"], &gateway); err != nil {
		t.Fatalf("gateway section: %v", err)
	}
	rawOrigins, ok := gateway["cors_allow_origins"]
	if !ok {
		t.Fatalf("gateway.cors_allow_origins missing in %s", raw)
	}
	var origins []string
	if err := json.Unmarshal(rawOrigins, &origins); err != nil {
		t.Fatalf("cors_allow_origins = %s: %v", rawOrigins, err)
	}
	if !reflect.DeepEqual(origins, []string{"http://app.test"}) {
		t.Fatalf("cors_allow_origins = %q, want the documented key", origins)
	}
	if got, err := Load(dir); err != nil || !reflect.DeepEqual(got.Gateway.CorsAllowOrigins, []string{"http://app.test"}) {
		t.Fatalf("round trip = %+v (%v)", got, err)
	}

	// A hand-written list is trimmed and de-duplicated on load.
	manual := `{"gateway":{"cors_allow_origins":[" https://app.test/ ","https://app.test","*"]}}`
	if err := os.WriteFile(ConfigPath(dir), []byte(manual), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if want := []string{"https://app.test/", "*"}; !reflect.DeepEqual(got.Gateway.CorsAllowOrigins, want) {
		t.Fatalf("origins = %q, want %q", got.Gateway.CorsAllowOrigins, want)
	}
}

func TestLoadClamps(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		then func(t *testing.T, c Config)
	}{
		{
			name: "port below minimum",
			raw:  `{"port": 80}`,
			then: func(t *testing.T, c Config) {
				if c.Port != 1024 {
					t.Fatalf("port = %d, want 1024", c.Port)
				}
			},
		},
		{
			name: "port above maximum",
			raw:  `{"port": 70000}`,
			then: func(t *testing.T, c Config) {
				if c.Port != 65535 {
					t.Fatalf("port = %d, want 65535", c.Port)
				}
			},
		},
		{
			name: "concurrency too small",
			raw:  `{"plugins": {"concurrency": 0}}`,
			then: func(t *testing.T, c Config) {
				if c.Plugins.Concurrency != 1 {
					t.Fatalf("concurrency = %d, want 1", c.Plugins.Concurrency)
				}
			},
		},
		{
			name: "concurrency too large",
			raw:  `{"plugins": {"concurrency": 100}}`,
			then: func(t *testing.T, c Config) {
				if c.Plugins.Concurrency != 32 {
					t.Fatalf("concurrency = %d, want 32", c.Plugins.Concurrency)
				}
			},
		},
		{
			name: "negative retention",
			raw:  `{"retention": {"api_request_days": -1, "body_preview_days": 0, "checkin_days": -10, "host_log_days": 0, "proxy_log_days": -3}}`,
			then: func(t *testing.T, c Config) {
				if c.Retention.APIRequestDays != 1 || c.Retention.BodyPreviewDays != 1 ||
					c.Retention.CheckinDays != 1 || c.Retention.HostLogDays != 1 ||
					c.Retention.ProxyLogDays != 1 {
					t.Fatalf("retention = %+v, want all 1", c.Retention)
				}
			},
		},
		{
			name: "retry count bounds",
			raw:  `{"checkin": {"retry_count": 99}}`,
			then: func(t *testing.T, c Config) {
				if c.Checkin.RetryCount != 5 {
					t.Fatalf("retry_count = %d, want 5", c.Checkin.RetryCount)
				}
			},
		},
		{
			name: "negative retry count",
			raw:  `{"checkin": {"retry_count": -3}}`,
			then: func(t *testing.T, c Config) {
				if c.Checkin.RetryCount != 0 {
					t.Fatalf("retry_count = %d, want 0", c.Checkin.RetryCount)
				}
			},
		},
		{
			name: "negative model cache",
			raw:  `{"a": {"model_cache_minutes": -5}}`,
			then: func(t *testing.T, c Config) {
				if c.A.ModelCacheMinutes != MinModelCacheMinutes {
					t.Fatalf("model_cache_minutes = %d, want %d", c.A.ModelCacheMinutes, MinModelCacheMinutes)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(ConfigPath(dir), []byte(tt.raw), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(dir)
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("clamped config failed Validate(): %v", err)
			}
			tt.then(t, cfg)
		})
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{name: "default ok", mutate: func(*Config) {}},
		{name: "port too low", mutate: func(c *Config) { c.Port = 1023 }, wantErr: true},
		{name: "port too high", mutate: func(c *Config) { c.Port = 65536 }, wantErr: true},
		{name: "concurrency 0", mutate: func(c *Config) { c.Plugins.Concurrency = 0 }, wantErr: true},
		{name: "concurrency 33", mutate: func(c *Config) { c.Plugins.Concurrency = 33 }, wantErr: true},
		{name: "bad startup mode", mutate: func(c *Config) { c.Plugins.StartupMode = "sometimes" }, wantErr: true},
		{name: "startup on demand", mutate: func(c *Config) { c.Plugins.StartupMode = StartupOnDemand }},
		{name: "bad time", mutate: func(c *Config) { c.Checkin.DefaultTime = "9:00" }, wantErr: true},
		{name: "hour out of range", mutate: func(c *Config) { c.Checkin.DefaultTime = "24:00" }, wantErr: true},
		{name: "retry count too high", mutate: func(c *Config) { c.Checkin.RetryCount = 6 }, wantErr: true},
		{name: "retry count negative", mutate: func(c *Config) { c.Checkin.RetryCount = -1 }, wantErr: true},
		{name: "retention zero", mutate: func(c *Config) { c.Retention.BodyPreviewDays = 0 }, wantErr: true},
		{name: "base url without scheme", mutate: func(c *Config) { c.A.BaseURL = "127.0.0.1:7864" }, wantErr: true},
		{name: "base url empty", mutate: func(c *Config) { c.A.BaseURL = "" }, wantErr: true},
		{name: "base url https", mutate: func(c *Config) { c.A.BaseURL = "https://example.test:7864" }},
		{name: "version zero", mutate: func(c *Config) { c.Version = 0 }, wantErr: true},
		{name: "model cache zero", mutate: func(c *Config) { c.A.ModelCacheMinutes = 0 }, wantErr: true},
		{name: "cors wildcard", mutate: func(c *Config) { c.Gateway.CorsAllowOrigins = []string{"*"} }},
		{
			name: "cors origin list",
			mutate: func(c *Config) {
				c.Gateway.CorsAllowOrigins = []string{"https://app.test", "http://localhost:3000"}
			},
		},
		{name: "cors non http scheme", mutate: func(c *Config) { c.Gateway.CorsAllowOrigins = []string{"ftp://app.test"} }, wantErr: true},
		{name: "cors missing host", mutate: func(c *Config) { c.Gateway.CorsAllowOrigins = []string{"https://"} }, wantErr: true},
		{name: "cors not a url", mutate: func(c *Config) { c.Gateway.CorsAllowOrigins = []string{"app.test"} }, wantErr: true},
		// gateway.default_plugin: empty is valid (no default), a plugin-id shape
		// is valid, anything else must be rejected. Whether the plugin is
		// installed is deliberately not checked (plugins can be added later).
		{name: "default plugin empty", mutate: func(c *Config) { c.Gateway.DefaultPlugin = "" }},
		{name: "default plugin id", mutate: func(c *Config) { c.Gateway.DefaultPlugin = "oc" }},
		{name: "default plugin dashed id", mutate: func(c *Config) { c.Gateway.DefaultPlugin = "my-plugin_2" }},
		{name: "default plugin uppercase", mutate: func(c *Config) { c.Gateway.DefaultPlugin = "OC" }, wantErr: true},
		{name: "default plugin leading digit", mutate: func(c *Config) { c.Gateway.DefaultPlugin = "1oc" }, wantErr: true},
		{name: "default plugin with prefix", mutate: func(c *Config) { c.Gateway.DefaultPlugin = "oc/model" }, wantErr: true},
		{name: "default plugin with space", mutate: func(c *Config) { c.Gateway.DefaultPlugin = " oc" }, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			tt.mutate(&cfg)
			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("Validate() = nil, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestNormalizeCorsOrigins(t *testing.T) {
	cfg := Default()
	cfg.Gateway.CorsAllowOrigins = []string{
		"  https://App.Example  ", "https://app.example/", "", "http://localhost:3000", "*",
	}
	cfg.Normalize()
	want := []string{"https://App.Example", "http://localhost:3000", "*"}
	if !reflect.DeepEqual(cfg.Gateway.CorsAllowOrigins, want) {
		t.Fatalf("origins = %q, want %q", cfg.Gateway.CorsAllowOrigins, want)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	empty := Default()
	empty.Gateway.CorsAllowOrigins = []string{"", "   "}
	empty.Normalize()
	if empty.Gateway.CorsAllowOrigins != nil {
		t.Fatalf("origins = %q, want nil so the gateway stays closed", empty.Gateway.CorsAllowOrigins)
	}
}

func TestNormalizeDefaultPlugin(t *testing.T) {
	cfg := Default()
	cfg.Gateway.DefaultPlugin = "  OC  "
	cfg.Normalize()
	if cfg.Gateway.DefaultPlugin != "oc" {
		t.Fatalf("default_plugin = %q, want oc", cfg.Gateway.DefaultPlugin)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	// Empty stays empty: it is the "no default" value, not a missing one.
	empty := Default()
	empty.Gateway.DefaultPlugin = "   "
	empty.Normalize()
	if empty.Gateway.DefaultPlugin != "" {
		t.Fatalf("default_plugin = %q, want an empty string", empty.Gateway.DefaultPlugin)
	}
	if err := empty.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	// LegacyAFallback ships on so an existing install keeps answering bare names
	// through A until the default-plugin slot (or S9b) replaces it.
	if !Default().Gateway.LegacyAFallback {
		t.Fatal("Default().Gateway.LegacyAFallback = false, want true")
	}
}

// TestDefaultPluginWireKeys pins the on-disk names: the settings page and the
// hand-edited config.json both use these keys.
func TestDefaultPluginWireKeys(t *testing.T) {
	dir := t.TempDir()
	cfg := Default()
	cfg.Gateway.DefaultPlugin = "oc"
	cfg.Gateway.LegacyAFallback = false
	if err := Save(dir, cfg); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	raw, err := os.ReadFile(ConfigPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	var onDisk struct {
		Gateway struct {
			DefaultPlugin string `json:"default_plugin"`
			LegacyA       *bool  `json:"legacy_a_fallback"`
		} `json:"gateway"`
	}
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("parse %s: %v", raw, err)
	}
	if onDisk.Gateway.DefaultPlugin != "oc" {
		t.Fatalf("gateway.default_plugin = %q, want oc (%s)", onDisk.Gateway.DefaultPlugin, raw)
	}
	if onDisk.Gateway.LegacyA == nil || *onDisk.Gateway.LegacyA {
		t.Fatalf("gateway.legacy_a_fallback = %v, want an explicit false (%s)", onDisk.Gateway.LegacyA, raw)
	}
	if got, err := Load(dir); err != nil ||
		got.Gateway.DefaultPlugin != "oc" || got.Gateway.LegacyAFallback {
		t.Fatalf("round trip = %+v (%v)", got.Gateway, err)
	}

	// An older config.json without the new keys keeps the compatibility default.
	if err := os.WriteFile(ConfigPath(dir), []byte(`{"gateway":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.Gateway.DefaultPlugin != "" || !got.Gateway.LegacyAFallback {
		t.Fatalf("absent keys = %+v, want no default plugin and the legacy fallback on", got.Gateway)
	}

	// A malformed value is rejected at load time, naming the field.
	if err := os.WriteFile(ConfigPath(dir), []byte(`{"gateway":{"default_plugin":"OC!"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "invalid default_plugin") {
		t.Fatalf("Load() = %v, want an invalid default_plugin error", err)
	}
}

func TestNotifyNormalization(t *testing.T) {
	cfg := Default()
	cfg.Notify.Channel = "  TELEGRAM "
	cfg.Notify.Events = []string{"quota_low", "checkin_fail", "quota_low", "unknown"}
	cfg.Notify.MinIntervalSeconds = 5
	cfg.Normalize()

	if cfg.Notify.Channel != DefaultNotifyChannel {
		t.Fatalf("unknown channel = %q, want %q", cfg.Notify.Channel, DefaultNotifyChannel)
	}
	if got := strings.Join(cfg.Notify.Events, ","); got != "quota_low,checkin_fail" {
		t.Fatalf("events = %q, want quota_low,checkin_fail", got)
	}
	if cfg.Notify.MinIntervalSeconds != MinNotifyInterval {
		t.Fatalf("min interval = %d, want %d", cfg.Notify.MinIntervalSeconds, MinNotifyInterval)
	}

	cfg.Notify.MinIntervalSeconds = MaxNotifyInterval + 1
	cfg.Normalize()
	if cfg.Notify.MinIntervalSeconds != MaxNotifyInterval {
		t.Fatalf("min interval = %d, want %d", cfg.Notify.MinIntervalSeconds, MaxNotifyInterval)
	}

	// nil keeps the default subscription; an explicit empty list notifies
	// nothing, so the user can turn every event off without disabling the
	// channel.
	empty := Default()
	empty.Notify.Events = nil
	empty.Normalize()
	if len(empty.Notify.Events) != len(DefaultNotifyEvents) {
		t.Fatalf("nil events = %v, want defaults", empty.Notify.Events)
	}
	empty.Notify.Events = []string{}
	empty.Normalize()
	if empty.Notify.Events == nil || len(empty.Notify.Events) != 0 {
		t.Fatalf("empty events = %v, want an empty non-nil list", empty.Notify.Events)
	}
}

func TestNotifyValidateRejectsBadValues(t *testing.T) {
	cfg := Default()
	cfg.Notify.Channel = "email"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "notify.channel") {
		t.Fatalf("Validate() = %v, want a notify.channel error", err)
	}

	cfg = Default()
	cfg.Notify.MinIntervalSeconds = 10
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "min_interval_seconds") {
		t.Fatalf("Validate() = %v, want a min_interval_seconds error", err)
	}

	cfg = Default()
	cfg.Notify.Events = []string{"checkin_fail", "telegram"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "notify.events") {
		t.Fatalf("Validate() = %v, want a notify.events error", err)
	}
}

func TestNotifyRoundTripJSON(t *testing.T) {
	dir := t.TempDir()
	cfg := Default()
	cfg.Notify.Enabled = true
	cfg.Notify.Channel = "wecom"
	cfg.Notify.Events = []string{"checkin_fail", "account_dead"}
	cfg.Notify.MinIntervalSeconds = 1200
	if err := Save(dir, cfg); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !reflect.DeepEqual(loaded.Notify, cfg.Notify) {
		t.Fatalf("Notify = %+v, want %+v", loaded.Notify, cfg.Notify)
	}
}

func TestValidateErrorMentionsField(t *testing.T) {
	cfg := Default()
	cfg.Plugins.Concurrency = 99
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "concurrency") {
		t.Fatalf("Validate() = %v, want error mentioning concurrency", err)
	}
}

func TestDataDir(t *testing.T) {
	t.Setenv("APPDATA", `C:\Users\tester\AppData\Roaming`)
	got, err := DataDir()
	if err != nil {
		t.Fatalf("DataDir() error = %v", err)
	}
	want := filepath.Join(`C:\Users\tester\AppData\Roaming`, "AIWorkGate")
	if got != want {
		t.Fatalf("DataDir() = %q, want %q", got, want)
	}

	t.Setenv("APPDATA", "")
	if _, err := DataDir(); err == nil {
		t.Fatal("DataDir() with empty APPDATA returned nil error")
	}
}

func TestEnsureDir(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "nested", "AIWorkGate")
	if err := EnsureDir(dir); err != nil {
		t.Fatalf("EnsureDir() error = %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		t.Fatalf("EnsureDir() did not create a directory: %v", err)
	}
	if err := EnsureDir(""); err == nil {
		t.Fatal("EnsureDir(\"\") returned nil error")
	}
}
