package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"aiworkgate/internal/config"
	"aiworkgate/internal/egress"
	"aiworkgate/internal/model"
	"aiworkgate/internal/plugins"
	"aiworkgate/internal/store"
)

// Deleting an account must also drop its proxy rule from egress.json, not just
// from the overlay rows: the plugin reads that file, so a stale entry routed
// the account through a proxy that no longer belongs to it.
func TestAccountRemovalRewritesEgressFile(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	pluginDir := filepath.Join(dir, "plugins", "mock")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatalf("mkdir plugin: %v", err)
	}
	mf := model.Manifest{
		ID:           "mock",
		Name:         "Mock",
		Version:      "0.0.1",
		APILevel:     1,
		Capabilities: []string{model.CapEgress},
		Startup:      model.StartupConfig{Mode: model.StartupOnDemand},
		Entry:        model.Entry{Exe: "mock.exe"},
	}
	raw, err := json.Marshal(mf)
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
	em := egress.NewManager(st)
	t.Cleanup(func() { _ = em.CloseAll() })

	app := &App{dataDir: dir, store: st, pluginMgr: mgr, egressMgr: em}

	if err := st.SetAccountEgress(model.AccountEgress{
		PluginID: "mock", AccountID: "acc_1", Mode: model.EgressProxy, ProxyID: "px_1",
	}); err != nil {
		t.Fatalf("SetAccountEgress: %v", err)
	}
	if err := app.writeEgressForPlugin("mock", nil); err != nil {
		t.Fatalf("writeEgressForPlugin: %v", err)
	}
	readFile := func() model.EgressFile {
		data, err := os.ReadFile(mgr.EgressFile("mock"))
		if err != nil {
			t.Fatalf("read egress.json: %v", err)
		}
		var f model.EgressFile
		if err := json.Unmarshal(data, &f); err != nil {
			t.Fatalf("decode egress.json: %v", err)
		}
		return f
	}
	if _, ok := readFile().Accounts["acc_1"]; !ok {
		t.Fatal("assigned account missing from egress.json before deletion")
	}

	// This is what accountDelete does after the plugin confirms the removal.
	if err := st.DeleteAccountEgress("mock", "acc_1"); err != nil {
		t.Fatalf("DeleteAccountEgress: %v", err)
	}
	app.syncEgressAfterAccountRemoval("mock")

	if _, ok := readFile().Accounts["acc_1"]; ok {
		t.Fatal("deleted account kept its proxy rule in egress.json")
	}
}
