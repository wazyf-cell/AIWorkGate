package opchost

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"aiworkgate/internal/model"
)

const snapshotPluginDir = "/srv/aiworkgate/plugins/trae-bridge"

func snapshotOptions() Options {
	return Options{
		ID:             "trae-bridge",
		Name:           "Trae Bridge",
		Version:        "0.1.0",
		Description:    "aiworkgate plugin contract bridge over a magpie .mjs plugin",
		APILevel:       model.MinAPILevel,
		Capabilities:   []string{model.CapAccounts, model.CapCheckin},
		NetworkDomains: []string{"trae.ai", "*.trae.ai"},
		StartupMode:    model.StartupOnDemand,
		IdleTimeoutS:   600,
		PluginDir:      snapshotPluginDir,
	}
}

func TestGenerateBridgeManifestMatchesSnapshot(t *testing.T) {
	mf, err := GenerateBridgeManifest(snapshotOptions())
	if err != nil {
		t.Fatalf("GenerateBridgeManifest: %v", err)
	}
	got, err := json.MarshalIndent(mf, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	got = append(got, '\n')
	want, err := os.ReadFile(filepath.Join("testdata", "manifest_bridge.golden.json"))
	if err != nil {
		t.Fatalf("read golden manifest: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("manifest snapshot mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestGenerateBridgeManifestDefaults(t *testing.T) {
	mf, err := GenerateBridgeManifest(Options{
		ID:        " demo-bridge ",
		PluginDir: "/srv/aiworkgate/plugins/demo-bridge",
	})
	if err != nil {
		t.Fatalf("GenerateBridgeManifest: %v", err)
	}
	if mf.ID != "demo-bridge" {
		t.Fatalf("id = %q, want the trimmed id", mf.ID)
	}
	if mf.APILevel != model.MinAPILevel {
		t.Fatalf("api_level = %d, want %d", mf.APILevel, model.MinAPILevel)
	}
	if !slices.Equal(mf.Capabilities, DefaultBridgeCapabilities) {
		t.Fatalf("capabilities = %v, want %v", mf.Capabilities, DefaultBridgeCapabilities)
	}
	if mf.Entry.Exe != DefaultBridgeExeName {
		t.Fatalf("entry.exe = %q, want %q", mf.Entry.Exe, DefaultBridgeExeName)
	}
	// "" is normalized to "always" by the host, so the generator must not invent
	// a mode of its own.
	if mf.Startup.Mode != "" {
		t.Fatalf("startup.mode = %q, want empty (host default)", mf.Startup.Mode)
	}
	wantArgs := []string{"--plugin-dir", "/srv/aiworkgate/plugins/demo-bridge"}
	if !slices.Equal(mf.Entry.Args, wantArgs) {
		t.Fatalf("entry.args = %v, want %v", mf.Entry.Args, wantArgs)
	}
}

// TestGenerateBridgeManifestIsDeterministic proves the manifest carries no
// allocation: the supervisor owns the port and the token, so two generations from
// the same options must be byte-identical.
func TestGenerateBridgeManifestIsDeterministic(t *testing.T) {
	first, err := GenerateBridgeManifest(snapshotOptions())
	if err != nil {
		t.Fatalf("first GenerateBridgeManifest: %v", err)
	}
	second, err := GenerateBridgeManifest(snapshotOptions())
	if err != nil {
		t.Fatalf("second GenerateBridgeManifest: %v", err)
	}
	a, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal first: %v", err)
	}
	b, err := json.Marshal(second)
	if err != nil {
		t.Fatalf("marshal second: %v", err)
	}
	if string(a) != string(b) {
		t.Fatalf("manifest is not deterministic; port/token must be injected by the supervisor, not baked in:\n%s\n%s", a, b)
	}
}

// TestGeneratedManifestCarriesNoCredentials pins the corrected contract: the
// manifest holds the plugin directory only. Port, token and data directory are
// environment variables injected by the supervisor on every launch.
func TestGeneratedManifestCarriesNoCredentials(t *testing.T) {
	mf, err := GenerateBridgeManifest(snapshotOptions())
	if err != nil {
		t.Fatalf("GenerateBridgeManifest: %v", err)
	}
	blob, err := json.Marshal(mf)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	text := strings.ToLower(string(blob))
	for _, forbidden := range []string{"--port", "--token", "--data-dir", "aiworkgate_plugin_token", "token"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("manifest must not carry %q:\n%s", forbidden, blob)
		}
	}
}

func TestBridgeArgs(t *testing.T) {
	got := BridgeArgs(`C:\data\plugins\trae-bridge`)
	want := []string{"--plugin-dir", `C:\data\plugins\trae-bridge`}
	if !slices.Equal(got, want) {
		t.Fatalf("BridgeArgs = %v, want %v", got, want)
	}
}

func TestResolveBridgeEntry(t *testing.T) {
	dir := t.TempDir()
	inside := filepath.Join(dir, DefaultBridgeExeName)
	outside := filepath.Join(filepath.Dir(dir), DefaultBridgeExeName)

	cases := []struct {
		name      string
		pluginDir string
		bridgeExe string
		want      string
		wantErr   bool
	}{
		{"default is the in-directory name", dir, "", DefaultBridgeExeName, false},
		{"relative subdirectory", dir, "bin/" + DefaultBridgeExeName, "bin/" + DefaultBridgeExeName, false},
		{"absolute inside the plugin dir", dir, inside, DefaultBridgeExeName, false},
		{"dot-dot escape rejected", dir, "../" + DefaultBridgeExeName, "", true},
		{"absolute outside the plugin dir rejected", dir, outside, "", true},
		{"absolute needs an absolute plugin dir", "plugins/trae-bridge", "/opt/bridge/magpie-bridge.exe", "", true},
		{"drive-qualified path rejected", dir, "C:/bridge/" + DefaultBridgeExeName, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveBridgeEntry(tc.pluginDir, tc.bridgeExe)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ResolveBridgeEntry(%q, %q) = %q, want an error", tc.pluginDir, tc.bridgeExe, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveBridgeEntry(%q, %q): %v", tc.pluginDir, tc.bridgeExe, err)
			}
			if got != tc.want {
				t.Fatalf("ResolveBridgeEntry(%q, %q) = %q, want %q", tc.pluginDir, tc.bridgeExe, got, tc.want)
			}
		})
	}
}

func TestGenerateBridgeManifestRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Options)
		wantErr string
	}{
		{"empty id", func(o *Options) { o.ID = "" }, "id"},
		{"uppercase id", func(o *Options) { o.ID = "Trae-Bridge" }, "id"},
		{"api_level above the contract", func(o *Options) { o.APILevel = model.MaxAPILevel + 1 }, "api_level"},
		{"missing plugin dir", func(o *Options) { o.PluginDir = "   " }, "plugin dir"},
		{"unknown startup mode", func(o *Options) { o.StartupMode = "sometimes" }, "startup mode"},
		{"negative idle timeout", func(o *Options) { o.IdleTimeoutS = -1 }, "idle timeout"},
		{"escaping bridge exe", func(o *Options) { o.BridgeExe = "../" + DefaultBridgeExeName }, "escapes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := snapshotOptions()
			tc.mutate(&opts)
			if _, err := GenerateBridgeManifest(opts); err == nil {
				t.Fatalf("GenerateBridgeManifest(%+v) = nil error, want one containing %q", opts, tc.wantErr)
			} else if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}
