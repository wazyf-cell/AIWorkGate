package market

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseSpecKinds(t *testing.T) {
	local := t.TempDir()
	realFile := filepath.Join(local, "plugin.mjs")
	if err := os.WriteFile(realFile, []byte("export {}"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		raw        string
		kind       Kind
		name       string
		constraint string
		dir        string
		owner      string
		repo       string
	}{
		{"lodash", KindNPM, "lodash", "", "", "", ""},
		{"@magpie-community/opencode-trae-auth", KindNPM, "@magpie-community/opencode-trae-auth", "", "", "", ""},
		{"@m/p@^1.2.3", KindNPM, "@m/p", "^1.2.3", "", "", ""},
		{"npm:foo@2.0.0", KindNPM, "foo", "2.0.0", "", "", ""},
		{"github:owner/repo", KindGitHub, "", "", "", "owner", "repo"},
		{"gh:o/r", KindGitHub, "", "", "", "o", "r"},
		{"https://github.com/o/r", KindGitHub, "", "", "", "o", "r"},
		{local, KindLocal, "", "", local, "", ""},
		{"file:" + local, KindLocal, "", "", local, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			spec, err := ParseSpec(tc.raw)
			if err != nil {
				t.Fatalf("ParseSpec(%q): %v", tc.raw, err)
			}
			if spec.Kind != tc.kind {
				t.Fatalf("kind = %v, want %v", spec.Kind, tc.kind)
			}
			if spec.Name != tc.name {
				t.Fatalf("name = %q, want %q", spec.Name, tc.name)
			}
			if spec.Constraint != tc.constraint {
				t.Fatalf("constraint = %q, want %q", spec.Constraint, tc.constraint)
			}
			if spec.Dir != tc.dir {
				t.Fatalf("dir = %q, want %q", spec.Dir, tc.dir)
			}
			if spec.Owner != tc.owner || spec.Repo != tc.repo {
				t.Fatalf("github = %q/%q, want %q/%q", spec.Owner, spec.Repo, tc.owner, tc.repo)
			}
		})
	}
}

func TestParseSpecRejects(t *testing.T) {
	for _, raw := range []string{"", "   ", "@scope only", "has space", "github:"} {
		t.Run(raw, func(t *testing.T) {
			if _, err := ParseSpec(raw); err == nil {
				t.Fatalf("ParseSpec(%q) = nil error, want one", raw)
			}
		})
	}
}

func TestPluginIDFor(t *testing.T) {
	cases := map[string]string{
		"@magpie-community/opencode-trae-auth": "opencode-trae-auth",
		"@scope/pkg":                           "pkg",
		"opencode-trae-auth":                   "opencode-trae-auth",
		"Foo_Bar":                              "foo-bar",
		"tag/name":                             "tag-name",
		"a.b.c":                                "a-b-c",
	}
	for pkg, want := range cases {
		got, err := PluginIDFor(pkg)
		if err != nil {
			t.Fatalf("PluginIDFor(%q): %v", pkg, err)
		}
		if got != want {
			t.Fatalf("PluginIDFor(%q) = %q, want %q", pkg, got, want)
		}
	}
}

func TestPluginIDForRejects(t *testing.T) {
	for _, pkg := range []string{"", "!!!"} {
		if id, err := PluginIDFor(pkg); err == nil {
			t.Fatalf("PluginIDFor(%q) = %q, want an error", pkg, id)
		}
	}
}

func TestPluginIDForLongNameHashes(t *testing.T) {
	// Longer than the host's 32-char limit: a deterministic suffix, never a
	// lossy truncation and never an empty mapping.
	pkg := "@magpie-community/opencode-a-very-long-provider-name-here-auth"
	a, err := PluginIDFor(pkg)
	if err != nil {
		t.Fatalf("PluginIDFor: %v", err)
	}
	b, err := PluginIDFor(pkg)
	if err != nil {
		t.Fatalf("PluginIDFor again: %v", err)
	}
	if a != b {
		t.Fatalf("PluginIDFor is not deterministic: %q != %q", a, b)
	}
	if len(a) > 32 || !pluginIDPattern.MatchString(a) {
		t.Fatalf("hashed id %q must be <= 32 chars and match the plugin id rule", a)
	}
}
