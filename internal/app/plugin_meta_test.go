package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"aiworkgate/internal/config"
	"aiworkgate/internal/model"
	"aiworkgate/internal/plugins"
	"aiworkgate/internal/webui"
)

// pluginMetaApp installs one stopped plugin named "mock" so the callback sees a
// real manager entry without launching a process.
func pluginMetaApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	pluginDir := filepath.Join(dir, "plugins", "mock")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatalf("mkdir plugin: %v", err)
	}
	raw, err := json.Marshal(model.Manifest{
		ID:       "mock",
		Name:     "Mock",
		Version:  "0.0.1",
		APILevel: 1,
		Startup:  model.StartupConfig{Mode: model.StartupOnDemand},
		Entry:    model.Entry{Exe: "mock.exe"},
	})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, plugins.ManifestFileName), raw, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "mock.exe"), []byte("MZ"), 0o644); err != nil {
		t.Fatalf("write entry: %v", err)
	}
	mgr := plugins.New(plugins.Options{
		DataDir: dir,
		Config:  func() config.Config { return config.Default() },
	})
	t.Cleanup(func() { _ = mgr.Close() })
	if err := mgr.Discover(); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	return &App{dataDir: dir, pluginMgr: mgr}
}

func TestPluginMetaMapsStoppedPlugin(t *testing.T) {
	app := pluginMetaApp(t)
	_, err := app.pluginMeta("mock")
	if !errors.Is(err, webui.ErrPluginNotRunning) {
		t.Fatalf("pluginMeta(stopped) = %v, want ErrPluginNotRunning", err)
	}
	// Reading uptime must not wake an on-demand plugin.
	if st, ok := app.pluginMgr.Get("mock"); !ok || st.Running {
		t.Fatalf("plugin state after meta = %+v (found %v), want installed and stopped", st, ok)
	}
}

func TestPluginMetaMapsUnknownPlugin(t *testing.T) {
	app := pluginMetaApp(t)
	if _, err := app.pluginMeta("ghost"); !errors.Is(err, webui.ErrPluginNotFound) {
		t.Fatalf("pluginMeta(ghost) = %v, want ErrPluginNotFound", err)
	}
}
