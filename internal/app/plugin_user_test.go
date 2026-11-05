package app

import (
	"sync"
	"testing"

	"aiworkgate/internal/config"
)

// P8-①：config.PluginsConfig.SandboxUser 必须原样流到 plugins.Options.PluginUser。
// 通过 pluginOptions() 这个装配缝直接验证（不 spawn 插件子进程，Linux 专用字段
// 在本机 Windows 上同样无需真实 uid）。
func TestPluginOptionsCarriesSandboxUser(t *testing.T) {
	mk := func(user string) *App {
		return &App{
			cfg:   config.Config{Plugins: config.PluginsConfig{SandboxUser: user}},
			cfgMu: sync.RWMutex{},
		}
	}

	opts := mk("aiworkgate-plugin").pluginOptions(t.TempDir(), nil, nil, nil, nil)
	if opts.PluginUser != "aiworkgate-plugin" {
		t.Fatalf("PluginUser = %q, want aiworkgate-plugin (config → app → plugins wiring)", opts.PluginUser)
	}
	if opts.DataDir == "" {
		t.Fatal("DataDir empty; option assembly regressed")
	}

	// 缺省空 = off：不设置 SandboxUser 时不得凭空冒出用户。
	if off := mk("").pluginOptions(t.TempDir(), nil, nil, nil, nil); off.PluginUser != "" {
		t.Fatalf("default PluginUser = %q, want empty (off)", off.PluginUser)
	}
}

// TestPluginOptionsKeepsOtherFields pins that the seam keeps the other options
// wired (the P8 sandbox wiring must not disturb existing behaviour).
func TestPluginOptionsKeepsOtherFields(t *testing.T) {
	a := &App{
		cfg:   config.Default(),
		cfgMu: sync.RWMutex{},
	}
	opts := a.pluginOptions(t.TempDir(), nil, nil, nil, nil)
	if opts.DirectEgress == nil {
		t.Fatal("DirectEgress hook lost in pluginOptions seam")
	}
	if opts.Config == nil {
		t.Fatal("Config snapshot func lost in pluginOptions seam")
	}
}
