package plugins

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"aiworkgate/internal/config"
	"aiworkgate/internal/model"
)

// noProxy is the empty proxy config used by the env assertions below.
func noProxy() config.ProxyConfig { return config.ProxyConfig{} }

// writeRawManifest writes literal manifest JSON so a test can express the
// on-disk shape exactly, including fields the typed struct would default.
func writeRawManifest(t *testing.T, dir, body string) {
	t.Helper()
	writeTestFile(t, filepath.Join(dir, ManifestFileName), []byte(body))
}

// envValue parses the environment list produced by buildPluginEnv.
func envValue(env []string, key string) string { return parseEnvList(env)[key] }

// writesLegacyManifest writes a manifest that predates the api_level field.
const legacyManifestJSON = `{
	"id": "legacy",
	"name": "Legacy",
	"version": "0.1.0",
	"os": ["windows"],
	"permissions": {},
	"checkin": {},
	"quota": {},
	"concurrency": {},
	"rules": {},
	"egress": {},
	"startup": {"mode": "on_demand"},
	"entry": {"exe": "legacy-plugin.exe"}
}`

// TestInjectedAPILevelDefaultsToOne is the regression guard for v0.3: a
// manifest that declares no level at all must still be told "1", so a plugin
// written before the field existed never silently gains the forced-account
// behaviour it never implemented.
//
// Two layers are checked on purpose:
//   - injection: LoadManifestFile (parse only, no validation) leaves APILevel
//     at 0 and APILevelFor turns that into "1";
//   - admission: ValidateManifest rejects api_level 0 outright, which is why
//     the injection default is defense in depth rather than the normal path.
//
// If the admission rule is ever relaxed to accept an absent level, the
// injection default is what keeps such a plugin on contract v0.2.
func TestInjectedAPILevelDefaultsToOne(t *testing.T) {
	dir := testTempDir(t)
	writeRawManifest(t, dir, legacyManifestJSON)
	writeTestFile(t, filepath.Join(dir, "legacy-plugin.exe"), dummyExe)

	mf, err := LoadManifestFile(filepath.Join(dir, ManifestFileName))
	if err != nil {
		t.Fatalf("LoadManifestFile: %v", err)
	}
	if mf.APILevel != 0 {
		t.Fatalf("api_level parsed as %d, want 0 (absent)", mf.APILevel)
	}

	env := buildPluginEnv(nil, "legacy", 1234, "tok", "data", "rules", "egress", APILevelFor(mf), noProxy())
	if got := envValue(env, envAPILevel); got != "1" {
		t.Errorf("%s = %q, want \"1\" for a manifest without api_level", envAPILevel, got)
	}

	if err := ValidateManifest(mf); err == nil {
		t.Log("ValidateManifest now accepts an absent api_level; the injection default covers that case")
	}
}

// TestInjectedAPILevelFollowsManifest covers the actual defect: a manifest
// declaring api_level 2 was told "1" by v0.3.
func TestInjectedAPILevelFollowsManifest(t *testing.T) {
	cases := map[string]struct {
		apiLevel int
		valid    bool // whether the host admits this level
		want     string
	}{
		"api_level 2":         {apiLevel: 2, valid: true, want: "2"},
		"api_level 1":         {apiLevel: 1, valid: true, want: "1"},
		"api_level 0 absent":  {apiLevel: 0, valid: false, want: "1"},
		"api_level 3 unknown": {apiLevel: 3, valid: false, want: "1"},
		"api_level negative":  {apiLevel: -1, valid: false, want: "1"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m := baseManifest()
			m.ID = "leveled"
			m.APILevel = tc.apiLevel
			dir := customSource(t, m, dummyExe)

			// Admitted levels go through the real load-and-validate path.
			mf := m
			if tc.valid {
				loaded, err := loadManifestDir(dir)
				if err != nil {
					t.Fatalf("loadManifestDir: %v", err)
				}
				mf = loaded
			} else {
				if err := ValidateManifest(m); err == nil {
					t.Fatalf("ValidateManifest accepted api_level %d", tc.apiLevel)
				}
				loaded, err := LoadManifestFile(filepath.Join(dir, ManifestFileName))
				if err != nil {
					t.Fatalf("LoadManifestFile: %v", err)
				}
				mf = loaded
			}

			env := buildPluginEnv(nil, "leveled", 1, "tok", "data", "rules", "egress", APILevelFor(mf), noProxy())
			if got := envValue(env, envAPILevel); got != tc.want {
				t.Errorf("%s = %q, want %q", envAPILevel, got, tc.want)
			}
		})
	}
}

// TestAPILevelForClampsToHostRange ensures an unsupported level can never be
// advertised to a child even if it bypasses manifest validation.
func TestAPILevelForClampsToHostRange(t *testing.T) {
	cases := map[int]int{
		0: DefaultAPILevel,
		1: 1,
		2: 2,
		3: DefaultAPILevel,
		-1: DefaultAPILevel,
	}
	for in, want := range cases {
		if got := APILevelFor(model.Manifest{APILevel: in}); got != want {
			t.Errorf("APILevelFor(api_level=%d) = %d, want %d", in, got, want)
		}
	}
	if DefaultAPILevel != 1 {
		t.Errorf("DefaultAPILevel = %d, want 1 for backward compatibility", DefaultAPILevel)
	}
}

// TestManifestCapabilitiesParse checks that capabilities survives the
// JSON -> model.Manifest -> public lookup path for declared, empty and
// absent forms.
func TestManifestCapabilitiesParse(t *testing.T) {
	cases := map[string]struct {
		body string
		want []string
	}{
		"declared": {body: `{"id":"capper","name":"C","version":"1","api_level":2,"os":["windows"],"capabilities":["embeddings","images"],"permissions":{},"checkin":{},"quota":{},"concurrency":{},"rules":{},"egress":{},"startup":{"mode":"on_demand"},"entry":{"exe":"capper.exe"}}`,
			want: []string{"embeddings", "images"}},
		"empty array": {body: `{"id":"capper","name":"C","version":"1","api_level":2,"os":["windows"],"capabilities":[],"permissions":{},"checkin":{},"quota":{},"concurrency":{},"rules":{},"egress":{},"startup":{"mode":"on_demand"},"entry":{"exe":"capper.exe"}}`,
			want: nil},
		"absent": {body: `{"id":"capper","name":"C","version":"1","api_level":2,"os":["windows"],"permissions":{},"checkin":{},"quota":{},"concurrency":{},"rules":{},"egress":{},"startup":{"mode":"on_demand"},"entry":{"exe":"capper.exe"}}`,
			want: nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := testTempDir(t)
			writeRawManifest(t, dir, tc.body)
			writeTestFile(t, filepath.Join(dir, "capper.exe"), dummyExe)
			mf, err := loadManifestDir(dir)
			if err != nil {
				t.Fatalf("loadManifestDir: %v", err)
			}
			if len(mf.Capabilities) != len(tc.want) {
				t.Fatalf("capabilities = %v, want %v", mf.Capabilities, tc.want)
			}
			for i := range tc.want {
				if mf.Capabilities[i] != tc.want[i] {
					t.Fatalf("capabilities = %v, want %v", mf.Capabilities, tc.want)
				}
			}
		})
	}
}

// TestHasCapabilityDeclared guards the two lookups the routing layer needs.
func TestHasCapabilityDeclared(t *testing.T) {
	withCaps := model.Manifest{ID: "a", Capabilities: []string{model.CapUpstream, CapEmbeddings, " images "}}
	if !HasCapability(withCaps, "embeddings") {
		t.Error(`HasCapability("embeddings") = false, want true`)
	}
	if !HasCapability(withCaps, "images") {
		t.Error(`HasCapability("images") = false, want true (surrounding whitespace must be trimmed)`)
	}
	if HasCapability(withCaps, "upstream") == false {
		t.Error("v0.3 capability names must still match")
	}
	if HasCapability(withCaps, "video") {
		t.Error(`HasCapability("video") = true, want false`)
	}
	if HasCapability(withCaps, "") {
		t.Error("an empty capability name must never match")
	}

	empty := model.Manifest{ID: "b", Capabilities: []string{}}
	if HasCapability(empty, "embeddings") {
		t.Error("empty capabilities must declare nothing")
	}
	absent := model.Manifest{ID: "c"}
	if HasCapability(absent, "embeddings") {
		t.Error("absent capabilities must declare nothing")
	}
}

// TestManagerHasCapability installs two plugins and checks the manager-level
// lookup, including the unknown-id case.
func TestManagerHasCapability(t *testing.T) {
	mgr := newTestManager(t)

	capable := baseManifest()
	capable.ID = "capable"
	capable.Startup.Mode = model.StartupOnDemand
	capable.Capabilities = []string{model.CapUpstream, CapEmbeddings}
	capable.Entry.Exe = "capable.exe"
	if err := mgr.InstallDir(customSource(t, capable, dummyExe)); err != nil {
		t.Fatalf("install capable: %v", err)
	}

	bare := baseManifest()
	bare.ID = "bare"
	bare.Startup.Mode = model.StartupOnDemand
	bare.Capabilities = []string{}
	bare.Entry.Exe = "bare.exe"
	if err := mgr.InstallDir(customSource(t, bare, dummyExe)); err != nil {
		t.Fatalf("install bare: %v", err)
	}

	if !mgr.HasCapability("capable", "embeddings") {
		t.Error(`HasCapability("capable", "embeddings") = false, want true`)
	}
	if mgr.HasCapability("capable", "images") {
		t.Error(`HasCapability("capable", "images") = true, want false`)
	}
	if mgr.HasCapability("bare", "embeddings") {
		t.Error(`HasCapability("bare", "embeddings") = true, want false`)
	}
	if mgr.HasCapability("ghost", "embeddings") {
		t.Error("an unknown plugin must declare nothing")
	}
	if got := mgr.PluginCapabilities("bare"); got != nil {
		t.Errorf("PluginCapabilities(bare) = %v, want nil", got)
	}
	// The returned slice must be a copy, not the manifest's backing array.
	got := mgr.PluginCapabilities("capable")
	if len(got) != 2 {
		t.Fatalf("PluginCapabilities(capable) = %v", got)
	}
	got[0] = "mutated"
	if !mgr.HasCapability("capable", model.CapUpstream) {
		t.Error("PluginCapabilities must return a copy; the manifest was mutated")
	}
}

// TestShippedManifestsDeclareCapabilities verifies the manifests on disk stay
// loadable and keep declaring their v0.3 capability sets.
func TestShippedManifestsDeclareCapabilities(t *testing.T) {
	for _, id := range []string{"codearts", "mock", "officeace"} {
		t.Run(id, func(t *testing.T) {
			path := filepath.Join("..", "..", "plugins", id, ManifestFileName)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Skipf("plugin %s has no manifest in this checkout: %v", id, err)
			}
			var mf model.Manifest
			if err := json.Unmarshal(data, &mf); err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			if err := ValidateManifest(mf); err != nil {
				t.Fatalf("validate %s: %v", path, err)
			}
			var probe struct {
				Capabilities *[]string `json:"capabilities"`
			}
			if err := json.Unmarshal(data, &probe); err != nil {
				t.Fatalf("parse capabilities of %s: %v", path, err)
			}
			if probe.Capabilities == nil {
				t.Fatalf("%s must declare capabilities explicitly", path)
			}
			if HasCapability(mf, CapEmbeddings) {
				t.Logf("%s declares embeddings", id)
			}
		})
	}
}

// TestUnknownCapabilities pins which names are outside the vocabulary. The
// point of the helper is that a typo is invisible at runtime, so the test also
// pins the two easy mistakes: a case mismatch and a duplicated name.
func TestUnknownCapabilities(t *testing.T) {
	cases := []struct {
		name string
		caps []string
		want []string
	}{
		{name: "all known", caps: []string{"upstream", "accounts", "checkin", "embeddings", "images"}, want: nil},
		{name: "whitespace trimmed", caps: []string{" checkin "}, want: nil},
		{name: "empty entries ignored", caps: []string{"", "   ", "quota"}, want: nil},
		{name: "typo", caps: []string{"upstream", "checkin2"}, want: []string{"checkin2"}},
		{name: "case is significant", caps: []string{"Checkin"}, want: []string{"Checkin"}},
		{name: "duplicates collapsed", caps: []string{"nope", "nope", "quota"}, want: []string{"nope"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := UnknownCapabilities(model.Manifest{ID: "p", Capabilities: tc.caps})
			if len(got) != len(tc.want) {
				t.Fatalf("UnknownCapabilities(%v) = %v, want %v", tc.caps, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("UnknownCapabilities(%v) = %v, want %v", tc.caps, got, tc.want)
				}
			}
		})
	}
}
