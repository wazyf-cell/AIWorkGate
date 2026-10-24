package plugins

import (
	"encoding/json"
	"strings"
	"testing"

	"aiworkgate/internal/model"
)

func baseManifest() model.Manifest {
	return model.Manifest{
		ID:           "demo",
		Name:         "Demo",
		Version:      "0.1.0",
		APILevel:     model.APIVersion,
		OS:           []string{"windows"},
		Capabilities: []string{model.CapUpstream, model.CapAccounts},
		Models:       []model.ModelInfo{{ID: "demo-chat", Name: "Demo Chat"}},
		Checkin:      model.CheckinConfig{DefaultTime: "09:00"},
		Quota:        model.QuotaConfig{PollMinutes: intPtr(30)},
		Concurrency:  model.ConcurrencyConfig{Suggested: 2},
		Startup:      model.StartupConfig{Mode: model.StartupAlways, IdleTimeoutS: 600},
		Entry:        model.Entry{Exe: "demo.exe"},
	}
}

func TestValidateManifestAcceptsValid(t *testing.T) {
	cases := map[string]func(*model.Manifest){
		"minimal": func(m *model.Manifest) {},
		"empty startup mode": func(m *model.Manifest) {
			m.Startup.Mode = ""
		},
		"on demand": func(m *model.Manifest) {
			m.Startup.Mode = model.StartupOnDemand
		},
		"no checkin time": func(m *model.Manifest) {
			m.Checkin.DefaultTime = ""
		},
		"no concurrency suggestion": func(m *model.Manifest) {
			m.Concurrency.Suggested = 0
		},
		"no os list": func(m *model.Manifest) {
			m.OS = nil
		},
		"windows among others": func(m *model.Manifest) {
			m.OS = []string{"linux", "Windows"}
		},
		"subdirectory entry": func(m *model.Manifest) {
			m.Entry.Exe = "bin/demo.exe"
			m.Entry.Args = []string{"--flag"}
		},
		"two char id": func(m *model.Manifest) {
			m.ID = "ab"
		},
		"32 char id": func(m *model.Manifest) {
			m.ID = "a" + strings.Repeat("b", 31)
		},
		"model ids with allowed punctuation": func(m *model.Manifest) {
			m.Models = []model.ModelInfo{{ID: "gpt-4o"}, {ID: "qwen2.5:7b_x"}}
		},
		"api level 1": func(m *model.Manifest) { m.APILevel = 1 },
		"api level 2": func(m *model.Manifest) { m.APILevel = 2 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := baseManifest()
			mutate(&m)
			if err := ValidateManifest(m); err != nil {
				t.Fatalf("expected valid manifest, got %v", err)
			}
		})
	}
}

func TestValidateManifestRejectsInvalid(t *testing.T) {
	cases := map[string]func(*model.Manifest){
		"empty id":           func(m *model.Manifest) { m.ID = "" },
		"short id":           func(m *model.Manifest) { m.ID = "a" },
		"uppercase id":       func(m *model.Manifest) { m.ID = "Demo" },
		"underscore id":      func(m *model.Manifest) { m.ID = "de_mo" },
		"leading dash":       func(m *model.Manifest) { m.ID = "-demo" },
		"long id":            func(m *model.Manifest) { m.ID = strings.Repeat("a", 33) },
		"bad api level":      func(m *model.Manifest) { m.APILevel = 3 },
		"zero api level":     func(m *model.Manifest) { m.APILevel = 0 },
		"negative api level": func(m *model.Manifest) { m.APILevel = -1 },
		"missing exe":        func(m *model.Manifest) { m.Entry.Exe = "" },
		"absolute exe":       func(m *model.Manifest) { m.Entry.Exe = `C:\Windows\System32\evil.exe` },
		"escaping exe":       func(m *model.Manifest) { m.Entry.Exe = "../evil.exe" },
		"nested escape":      func(m *model.Manifest) { m.Entry.Exe = "bin/../../evil.exe" },
		"slash model":        func(m *model.Manifest) { m.Models = []model.ModelInfo{{ID: "demo/chat"}} },
		"cjk model":          func(m *model.Manifest) { m.Models = []model.ModelInfo{{ID: "模型"}} },
		"empty model":        func(m *model.Manifest) { m.Models = []model.ModelInfo{{ID: ""}} },
		"bad checkin":        func(m *model.Manifest) { m.Checkin.DefaultTime = "9:00" },
		"bad startup":        func(m *model.Manifest) { m.Startup.Mode = "sometimes" },
		"bad concurrency": func(m *model.Manifest) {
			m.Concurrency.Suggested = 33
		},
		"negative concurrency": func(m *model.Manifest) {
			m.Concurrency.Suggested = -1
		},
		"negative idle": func(m *model.Manifest) { m.Startup.IdleTimeoutS = -1 },
		"linux only":    func(m *model.Manifest) { m.OS = []string{"linux"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := baseManifest()
			mutate(&m)
			if err := ValidateManifest(m); err == nil {
				t.Fatalf("expected an error for %+v", m)
			}
		})
	}
}

func TestNormalizeManifestDefaults(t *testing.T) {
	m := baseManifest()
	m.Startup.Mode = ""
	m.Entry.Exe = `bin\demo.exe`
	got := normalizeManifest(m)
	if got.Startup.Mode != model.StartupAlways {
		t.Fatalf("startup mode = %q, want always", got.Startup.Mode)
	}
	if got.Entry.Exe != "bin/demo.exe" {
		t.Fatalf("entry exe = %q, want forward slashes", got.Entry.Exe)
	}
}

func intPtr(v int) *int { return &v }

// knownManifestJSON declares every top-level key and every nested field the
// host decodes, so a growth of either vocabulary without a matching update of
// the known-key tables is caught as a false positive here.
const knownManifestJSON = `{
  "id": "demo",
  "name": "Demo",
  "version": "0.1.0",
  "api_level": 2,
  "description": "demo plugin",
  "author": "someone",
  "homepage": "https://example.invalid",
  "os": ["windows"],
  "capabilities": ["upstream"],
  "permissions": {
    "read_paths": ["%LOCALAPPDATA%/Demo"],
    "launch_apps": ["demo-client.exe"],
    "network_domains": ["example.invalid"],
    "use_proxy": false,
    "os_identity": false,
    "require_admin": false
  },
  "models": [{
    "id": "demo-chat",
    "name": "Demo Chat",
    "context_length": 128000,
    "context_window": 128000,
    "tools": true,
    "vision": false,
    "vendor": "demo",
    "multiplier": 1,
    "thinking": ["high"],
    "free": true,
    "display": "Demo Chat",
    "supports_image": false
  }],
  "checkin": {"default_time": "09:00", "supports_history": true},
  "quota": {"poll_minutes": 30},
  "concurrency": {"suggested": 2},
  "rules": {"packs": ["default"]},
  "egress": {"strict": false},
  "startup": {"mode": "always", "idle_timeout_s": 600},
  "entry": {"exe": "demo.exe", "args": ["--port", "0"]},
  "login": {
    "methods": ["api_key"],
    "reads_local_state": false,
    "requires_app": false,
    "auto_refresh": false,
    "notes": ""
  }
}`

func TestUnknownManifestKeysReportsTypos(t *testing.T) {
	data := []byte(`{
	  "id": "demo",
	  "capabilites": ["upstream"],
	  "startup": {"mode": "always", "modee": "always"}
	}`)
	got := UnknownManifestKeys(data)
	want := []string{"capabilites", "startup.modee"}
	if len(got) != len(want) {
		t.Fatalf("unknown keys = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unknown keys = %v, want %v", got, want)
		}
	}
}

func TestUnknownManifestKeysAcceptsKnownManifest(t *testing.T) {
	if got := UnknownManifestKeys([]byte(knownManifestJSON)); len(got) != 0 {
		t.Fatalf("known manifest reported unknown keys: %v", got)
	}
	// The same document round-tripped through model.Manifest must still parse,
	// otherwise the fixture above proves nothing about the real decoder.
	var mf model.Manifest
	if err := json.Unmarshal([]byte(knownManifestJSON), &mf); err != nil {
		t.Fatalf("fixture does not decode: %v", err)
	}
	if mf.ID != "demo" || len(mf.Login.Methods) != 1 {
		t.Fatalf("fixture decoded unexpectedly: %+v", mf)
	}
}

func TestUnknownManifestKeysSkipsNonObjectBlocks(t *testing.T) {
	// A block of the wrong type is rejected elsewhere; it must not be reported
	// here as a pile of unknown keys.
	data := []byte(`{"id": "demo", "startup": "always", "models": {"id": "x"}, "quota": null}`)
	if got := UnknownManifestKeys(data); len(got) != 0 {
		t.Fatalf("non-object blocks reported unknown keys: %v", got)
	}
}

func TestUnknownManifestKeysMalformedJSON(t *testing.T) {
	for _, data := range []string{``, `{`, `[]`, `{"id": }`, `not json`} {
		if got := UnknownManifestKeys([]byte(data)); got != nil {
			t.Fatalf("UnknownManifestKeys(%q) = %v, want nil", data, got)
		}
	}
}
