package plugins

import (
	"archive/zip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aiworkgate/internal/config"
	"aiworkgate/internal/model"
)

type zipEntry struct {
	name string
	body string
	mode os.FileMode
}

func makeZip(t *testing.T, entries []zipEntry) string {
	t.Helper()
	path := filepath.Join(testTempDir(t), "plugin.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create zip: %v", err)
	}
	zw := zip.NewWriter(f)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		h.SetMode(mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatalf("zip header %q: %v", e.name, err)
		}
		if _, err := w.Write([]byte(e.body)); err != nil {
			t.Fatalf("zip write %q: %v", e.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close zip file: %v", err)
	}
	return path
}

func customSource(t *testing.T, m model.Manifest, exe []byte) string {
	t.Helper()
	dir := testTempDir(t)
	writeManifestFile(t, dir, m)
	writeTestFile(t, filepath.Join(dir, filepath.FromSlash(m.Entry.Exe)), exe)
	return dir
}

func manifestJSON(t *testing.T, m model.Manifest) string {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	return string(data)
}

func TestInstallDirCopiesAndPersists(t *testing.T) {
	mgr := newTestManager(t)
	m := baseManifest()
	m.ID = "demo"
	m.Version = "1.2.3"
	m.Startup.Mode = model.StartupOnDemand
	m.Concurrency.Suggested = 3
	m.Checkin.DefaultTime = "07:15"
	m.Egress.Strict = true
	m.Entry.Exe = "bin/demo.exe"
	src := customSource(t, m, dummyExe)

	if err := mgr.InstallDir(src); err != nil {
		t.Fatalf("InstallDir: %v", err)
	}

	dest := mgr.installDir("demo")
	if !isRegularFile(filepath.Join(dest, ManifestFileName)) {
		t.Fatal("manifest not copied")
	}
	if !isRegularFile(filepath.Join(dest, "bin", "demo.exe")) {
		t.Fatal("entry executable not copied")
	}
	if info, err := os.Stat(mgr.pluginDataDir("demo")); err != nil || !info.IsDir() {
		t.Fatalf("plugin data dir missing: %v", err)
	}

	status, ok := mgr.Get("demo")
	if !ok {
		t.Fatal("installed plugin not listed")
	}
	if !status.Enabled || status.Failed || status.Running {
		t.Errorf("status = enabled:%v failed:%v running:%v, want enabled and stopped", status.Enabled, status.Failed, status.Running)
	}
	if status.InstalledAt <= 0 {
		t.Errorf("installed_at = %d, want > 0", status.InstalledAt)
	}
	if status.Manifest.Version != "1.2.3" {
		t.Errorf("version = %q, want 1.2.3", status.Manifest.Version)
	}

	row, ok := settingsRow(t, mgr, "demo")
	if !ok {
		t.Fatal("settings row missing")
	}
	want := struct {
		mode        string
		concurrency int
		checkin     string
		strict      bool
	}{model.StartupOnDemand, 3, "07:15", true}
	if row.Mode != want.mode || row.Concurrency != want.concurrency || row.CheckinTime != want.checkin || row.Strict != want.strict {
		t.Errorf("settings = %+v, want %+v", row, want)
	}
	if row.InstalledAt != status.InstalledAt {
		t.Errorf("settings installed_at = %d, status = %d", row.InstalledAt, status.InstalledAt)
	}
	if got := mgr.Concurrency("demo"); got != 3 {
		t.Errorf("Concurrency = %d, want 3", got)
	}
}

func TestUpdateDirKeepsDataAndSettings(t *testing.T) {
	mgr := newTestManager(t)
	m := baseManifest()
	m.ID = "demo"
	m.Startup.Mode = model.StartupOnDemand
	m.Entry.Exe = "demo.exe"
	if err := mgr.InstallDir(customSource(t, m, dummyExe)); err != nil {
		t.Fatalf("install: %v", err)
	}
	dataFile := filepath.Join(mgr.pluginDataDir("demo"), "state.json")
	writeTestFile(t, dataFile, []byte(`{"keep":true}`))
	if err := mgr.opts.Store.UpsertPluginSettings("demo", true, model.StartupOnDemand, 7, "10:00", false, 555); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	m.Version = "0.2.0"
	src := customSource(t, m, append(dummyExe, []byte(" v2")...))
	writeTestFile(t, filepath.Join(src, "extra.txt"), []byte("new file"))
	if err := mgr.UpdateDir(src); err != nil {
		t.Fatalf("UpdateDir: %v", err)
	}

	status, _ := mgr.Get("demo")
	if status.Manifest.Version != "0.2.0" {
		t.Errorf("version after update = %q, want 0.2.0", status.Manifest.Version)
	}
	if status.InstalledAt != 555 {
		t.Errorf("installed_at after update = %d, want preserved 555", status.InstalledAt)
	}
	if !isRegularFile(filepath.Join(mgr.installDir("demo"), "extra.txt")) {
		t.Error("new file from update missing")
	}
	if !isRegularFile(dataFile) {
		t.Error("plugin data was touched by UpdateDir")
	}
	row, _ := settingsRow(t, mgr, "demo")
	if row.Concurrency != 7 || row.CheckinTime != "10:00" {
		t.Errorf("settings after update = %+v, want concurrency 7 and 10:00", row)
	}
}

func TestInstallFromSingleTopLevelDir(t *testing.T) {
	mgr := newTestManager(t)
	base := testTempDir(t)
	m := baseManifest()
	m.ID = "nested"
	m.Startup.Mode = model.StartupOnDemand
	m.Entry.Exe = "nested.exe"
	payload := filepath.Join(base, "plugin-root")
	writeManifestFile(t, payload, m)
	writeTestFile(t, filepath.Join(payload, "nested.exe"), dummyExe)

	if err := mgr.InstallDir(base); err != nil {
		t.Fatalf("InstallDir: %v", err)
	}
	if _, ok := mgr.Get("nested"); !ok {
		t.Fatal("nested plugin not installed")
	}
}

func TestInstallRejectsInvalidManifest(t *testing.T) {
	mgr := newTestManager(t)

	badLevel := baseManifest()
	badLevel.ID = "badlevel"
	badLevel.APILevel = 3
	if err := mgr.InstallDir(customSource(t, badLevel, dummyExe)); err == nil {
		t.Fatal("expected api_level rejection")
	}

	missingEntry := baseManifest()
	missingEntry.ID = "missingentry"
	missingEntry.Entry.Exe = "not-there.exe"
	src := testTempDir(t)
	writeManifestFile(t, src, missingEntry)
	if err := mgr.InstallDir(src); err == nil {
		t.Fatal("expected missing entry rejection")
	}

	if len(mgr.List()) != 0 {
		t.Fatalf("rejected plugins must not be installed, got %+v", mgr.List())
	}
}

func TestInstallZipRejectsTraversal(t *testing.T) {
	cases := map[string]zipEntry{
		"parent traversal": {name: "../evil.txt", body: "x"},
		"nested traversal": {name: "demo/../../evil.txt", body: "x"},
		"absolute path":    {name: "/evil.txt", body: "x"},
		"windows absolute": {name: `C:\evil.txt`, body: "x"},
		"backslash escape": {name: `..\evil.txt`, body: "x"},
		"symlink":          {name: "demo/link", body: "../outside", mode: os.ModeSymlink | 0o777},
	}
	for name, entry := range cases {
		t.Run(name, func(t *testing.T) {
			mgr := newTestManager(t)
			m := baseManifest()
			m.ID = "demo"
			m.Startup.Mode = model.StartupOnDemand
			m.Entry.Exe = "demo.exe"
			entries := []zipEntry{
				{name: "demo/manifest.json", body: manifestJSON(t, m)},
				{name: "demo/demo.exe", body: string(dummyExe)},
				entry,
			}
			zipPath := makeZip(t, entries)
			if err := mgr.InstallZip(zipPath); err == nil {
				t.Fatal("expected the archive to be rejected")
			}
			if _, ok := mgr.Get("demo"); ok {
				t.Fatal("rejected archive must not install the plugin")
			}
			if _, err := os.Stat(filepath.Join(mgr.pluginsDir(), "evil.txt")); err == nil {
				t.Fatal("archive escaped the installation directory")
			}
			if _, err := os.Stat(filepath.Join(mgr.dataDir, "evil.txt")); err == nil {
				t.Fatal("archive wrote outside the data directory")
			}
		})
	}
}

// zipFixture builds a valid one-plugin archive and returns its path plus the
// manifest it carries.
func zipFixture(t *testing.T, id string) (string, model.Manifest) {
	t.Helper()
	m := baseManifest()
	m.ID = id
	m.Startup.Mode = model.StartupOnDemand
	m.Entry.Exe = id + ".exe"
	return makeZip(t, []zipEntry{
		{name: id + "/manifest.json", body: manifestJSON(t, m)},
		{name: id + "/" + id + ".exe", body: string(dummyExe), mode: 0o755},
	}), m
}

func TestInstallZipValid(t *testing.T) {
	mgr := newTestManager(t)
	zipPath, _ := zipFixture(t, "zipped")
	if err := mgr.InstallZip(zipPath); err != nil {
		t.Fatalf("InstallZip: %v", err)
	}
	status, ok := mgr.Get("zipped")
	if !ok {
		t.Fatal("zipped plugin not installed")
	}
	// InstallZip now defaults a fresh install to stopped: the archive comes from
	// outside the trust boundary, so starting it is a separate user decision.
	if status.Enabled {
		t.Error("a fresh zip install must not be enabled by default")
	}
	if status.Running {
		t.Error("a fresh zip install must not be running")
	}
	if !isRegularFile(filepath.Join(mgr.installDir("zipped"), "zipped.exe")) {
		t.Fatal("executable missing after zip install")
	}
}

// TestInstallZipOptsEnabled proves the explicit opt-in still works, so the
// default above is a default rather than a regression.
func TestInstallZipOptsEnabled(t *testing.T) {
	mgr := newTestManager(t)
	zipPath, _ := zipFixture(t, "opted")
	if err := mgr.InstallZipOpts(zipPath, InstallOpts{Enabled: true}); err != nil {
		t.Fatalf("InstallZipOpts: %v", err)
	}
	status, ok := mgr.Get("opted")
	if !ok || !status.Enabled {
		t.Fatalf("opted status = %+v (ok=%v), want enabled", status, ok)
	}
}

// TestInstallZipDefaultStaysStopped is the T3 acceptance case stated as its own
// test: a valid archive installs successfully yet the plugin is neither enabled
// nor running, so nothing outside the trust boundary executes until the user
// starts it.
func TestInstallZipDefaultStaysStopped(t *testing.T) {
	mgr := newTestManager(t)
	zipPath, _ := zipFixture(t, "stopped")
	if err := mgr.InstallZip(zipPath); err != nil {
		t.Fatalf("InstallZip: %v", err)
	}
	status, ok := mgr.Get("stopped")
	if !ok {
		t.Fatal("plugin was not installed")
	}
	if status.Running {
		t.Error("a fresh upload is running; it must stay stopped")
	}
	row, ok := settingsRow(t, mgr, "stopped")
	if !ok {
		t.Fatal("settings row missing")
	}
	if row.Enabled {
		t.Error("a fresh upload is enabled in settings; it must default to disabled")
	}
	// The installed files must still be there: "stopped" is not "not installed".
	if !isRegularFile(filepath.Join(mgr.installDir("stopped"), "stopped.exe")) {
		t.Error("executable missing after install")
	}
}

// TestInstallZipUpdateKeepsEnabled pins the other half of the rule: an update of
// an already installed plugin must not resurrect/reset the user's enable choice.
func TestInstallZipUpdateKeepsEnabled(t *testing.T) {
	mgr := newTestManager(t)
	zipPath, _ := zipFixture(t, "keeper")
	if err := mgr.InstallZipOpts(zipPath, InstallOpts{Enabled: true}); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := mgr.Disable("keeper"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if err := mgr.InstallZipOpts(zipPath, InstallOpts{}); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	status, _ := mgr.Get("keeper")
	if status.Enabled {
		t.Error("an update re-enabled a plugin the user had disabled")
	}
}

func TestUninstallRemovesInstallDir(t *testing.T) {
	mgr := newTestManager(t)
	if err := mgr.InstallDir(writePluginSource(t, "demo", model.StartupOnDemand, dummyExe)); err != nil {
		t.Fatalf("install: %v", err)
	}
	dataDir := mgr.pluginDataDir("demo")
	writeTestFile(t, filepath.Join(dataDir, "keep.txt"), []byte("data"))

	if err := mgr.Uninstall("demo", false); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(mgr.installDir("demo")); !os.IsNotExist(err) {
		t.Errorf("install dir still present: %v", err)
	}
	if !isRegularFile(filepath.Join(dataDir, "keep.txt")) {
		t.Error("plugin data should be preserved when deleteData is false")
	}
	if _, ok := mgr.Get("demo"); ok {
		t.Error("uninstalled plugin still listed")
	}
	if _, ok := settingsRow(t, mgr, "demo"); ok {
		t.Error("plugin settings were not deleted")
	}

	if err := mgr.InstallDir(writePluginSource(t, "demo", model.StartupOnDemand, dummyExe)); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	if err := mgr.Uninstall("demo", true); err != nil {
		t.Fatalf("Uninstall with data: %v", err)
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Errorf("plugin data dir still present: %v", err)
	}
}

func TestRuntimeDirAndPluginEnv(t *testing.T) {
	mgr := newTestManager(t)
	dir := mgr.RuntimeDir("demo")
	if dir == "" {
		t.Fatal("RuntimeDir returned empty path")
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("runtime dir missing: %v", err)
	}
	if got := mgr.RulesFile("demo"); filepath.Base(got) != "rules.json" || filepath.Dir(got) != dir {
		t.Errorf("RulesFile = %q", got)
	}
	if got := mgr.EgressFile("demo"); filepath.Base(got) != "egress.json" || filepath.Dir(got) != dir {
		t.Errorf("EgressFile = %q", got)
	}
	if got := mgr.RuntimeDir("../escape"); got != "" {
		t.Errorf("RuntimeDir accepted traversal id: %q", got)
	}

	proxy := config.ProxyConfig{
		Enabled: true,
		HTTP:    "http://127.0.0.1:7890",
		HTTPS:   "http://127.0.0.1:7891",
		NoProxy: []string{"127.0.0.1", "localhost", "*.local"},
	}
	env := buildPluginEnv(
		[]string{"PATH=C:\\Windows", "AIWORKGATE_PLUGIN_ID=stale", "HTTP_PROXY=http://old:1"},
		"demo", 4567, "tok", `C:\data\plugin-data\demo`,
		`C:\data\runtime\demo\rules.json`, `C:\data\runtime\demo\egress.json`, 1, proxy,
	)
	got := parseEnvList(env)
	want := map[string]string{
		"AIWORKGATE_PLUGIN_ID":       "demo",
		"AIWORKGATE_PLUGIN_PORT":     "4567",
		"AIWORKGATE_PLUGIN_TOKEN":    "tok",
		"AIWORKGATE_PLUGIN_DATA_DIR": `C:\data\plugin-data\demo`,
		"AIWORKGATE_RULES_FILE":      `C:\data\runtime\demo\rules.json`,
		"AIWORKGATE_EGRESS_FILE":     `C:\data\runtime\demo\egress.json`,
		"AIWORKGATE_API_LEVEL":       "1",
		"HTTP_PROXY":                 "http://127.0.0.1:7890",
		"HTTPS_PROXY":                "http://127.0.0.1:7891",
		"NO_PROXY":                   "127.0.0.1,localhost,*.local",
		"PATH":                       `C:\Windows`,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("env %s = %q, want %q", k, got[k], v)
		}
	}
	if n := strings.Count(strings.Join(env, "\n"), "AIWORKGATE_PLUGIN_ID="); n != 1 {
		t.Errorf("AIWORKGATE_PLUGIN_ID appears %d times, want 1", n)
	}

	noProxy := buildPluginEnv(nil, "demo", 1, "t", "d", "r", "e", 1, config.ProxyConfig{})
	if got := parseEnvList(noProxy)["HTTP_PROXY"]; got != "" {
		t.Errorf("proxy vars injected while disabled: %q", got)
	}
	disabled := parseEnvList(buildPluginEnv([]string{"HTTP_PROXY=http://keep:1"}, "demo", 1, "t", "d", "r", "e", 1, config.ProxyConfig{}))
	if disabled["HTTP_PROXY"] != "http://keep:1" {
		t.Errorf("existing HTTP_PROXY should be kept when proxy is disabled, got %q", disabled["HTTP_PROXY"])
	}
}

func TestAddZipEntrySizeRejectsOverflow(t *testing.T) {
	cases := []struct {
		name  string
		total uint64
		size  uint64
		want  uint64
		ok    bool
	}{
		{"empty entry", 0, 0, 0, true},
		{"exact cap", 0, maxZipBytes, maxZipBytes, true},
		{"over cap", 0, maxZipBytes + 1, 0, false},
		{"fills remaining", 10, maxZipBytes - 10, maxZipBytes, true},
		{"past remaining", 10, maxZipBytes - 9, 10, false},
		// A Zip64 size cast to int64 would be negative; the helper must reject it.
		{"zip64 high bit", 0, 1 << 63, 0, false},
		{"zip64 max", 0, ^uint64(0), 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := addZipEntrySize(tc.total, tc.size)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("addZipEntrySize(%d, %d) = (%d, %v), want (%d, %v)",
					tc.total, tc.size, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func parseEnvList(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			out[kv[:i]] = kv[i+1:]
		}
	}
	return out
}
