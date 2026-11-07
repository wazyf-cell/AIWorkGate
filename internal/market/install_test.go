package market

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"aiworkgate/internal/model"
	"aiworkgate/internal/opchost"
)

var ctx = context.Background()

func makeMeta(pkg, latest string, versions ...string) PackageMetadata {
	vs := make(map[string]PackageVersion, len(versions))
	for _, v := range versions {
		vs[v] = PackageVersion{Name: pkg, Version: v, Dist: Dist{Shasum: "x"}}
	}
	return PackageMetadata{Name: pkg, DistTags: map[string]string{"latest": latest}, Versions: vs}
}

func TestPrepareNPMFullPipeline(t *testing.T) {
	pkg := "@magpie-community/opencode-trae-auth"
	tarball := buildTarball(t, []tarEntry{
		{name: "package/index.mjs", content: "export const plugin = {};\n"},
		{name: "package/package.json", content: `{"name":"opencode-trae-auth","version":"1.2.3"}`},
	})
	reg, _ := fakeNPMRegistry(t, makeMeta(pkg, "1.2.3", "1.2.3"), tarball)

	base := t.TempDir()
	finalDir := filepath.Join(base, "plugins", "opencode-trae-auth")
	bridge := fakeBridge(t)
	inst := NewInstaller(nil, filepath.Join(base, "scratch"))

	p, err := inst.Prepare(ctx, "npm:"+pkg, Options{
		Registry:         reg,
		FinalPluginDir:   finalDir,
		BridgeExecutable: bridge,
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer inst.Cleanup(p)

	// Manifest: generated bridge manifest, entry and args correct.
	mf := p.Manifest
	if mf.ID != "opencode-trae-auth" {
		t.Fatalf("id = %q, want opencode-trae-auth", mf.ID)
	}
	if mf.Entry.Exe != opchost.DefaultBridgeExeName {
		t.Fatalf("entry.exe = %q, want %q", mf.Entry.Exe, opchost.DefaultBridgeExeName)
	}
	wantArgs := []string{"--plugin-dir", finalDir}
	if !slices.Equal(mf.Entry.Args, wantArgs) {
		t.Fatalf("entry.args = %v, want %v", mf.Entry.Args, wantArgs)
	}
	for _, cap := range []string{model.CapUpstream, model.CapAccounts, model.CapCheckin} {
		if !slices.Contains(mf.Capabilities, cap) {
			t.Fatalf("capabilities %v is missing %q", mf.Capabilities, cap)
		}
	}

	// manifest.json is on disk in the prepared root, bridge exe deployed,
	// payload extracted.
	onDisk, err := os.ReadFile(filepath.Join(p.Root, "manifest.json"))
	if err != nil {
		t.Fatalf("manifest.json not written: %v", err)
	}
	var fromDisk model.Manifest
	if err := json.Unmarshal(onDisk, &fromDisk); err != nil {
		t.Fatalf("manifest.json does not parse: %v", err)
	}
	if fromDisk.ID != mf.ID {
		t.Fatalf("on-disk id = %q, want %q", fromDisk.ID, mf.ID)
	}
	gotBridge, err := os.ReadFile(filepath.Join(p.Root, opchost.DefaultBridgeExeName))
	if err != nil {
		t.Fatalf("bridge exe not deployed: %v", err)
	}
	if string(gotBridge) != "MZ fake bridge" {
		t.Fatalf("bridge exe content = %q, want the fake bytes", gotBridge)
	}
	if info, err := os.Stat(filepath.Join(p.Root, "index.mjs")); err != nil || info.IsDir() {
		t.Fatalf("payload not extracted: %v", err)
	}

	// Record: sha256 of the tarball, never enabled, provenance fields.
	sum := sha256.Sum256(tarball)
	wantSHA := hex.EncodeToString(sum[:])
	rec := p.Record
	if rec.SHA256 != wantSHA {
		t.Fatalf("sha256 = %q, want %q", rec.SHA256, wantSHA)
	}
	if rec.Enabled {
		t.Fatal("Enabled must be false on a fresh install (T3)")
	}
	if rec.Kind != "npm" || rec.PluginID != "opencode-trae-auth" || rec.Version != "1.2.3" {
		t.Fatalf("record = %+v", rec)
	}
	if rec.TarballURL == "" || !strings.HasSuffix(rec.TarballURL, "-1.2.3.tgz") {
		t.Fatalf("tarball url = %q, want the fake dist tarball", rec.TarballURL)
	}
	if rec.InstalledAt <= 0 {
		t.Fatalf("installed_at = %d, want a timestamp", rec.InstalledAt)
	}
}

func TestPrepareNPMHashMismatchRejectsAndCleansUp(t *testing.T) {
	pkg := "pkg"
	tarball := buildTarball(t, []tarEntry{{name: "package/index.mjs", content: "x"}})
	reg, _ := fakeNPMRegistry(t, makeMeta(pkg, "1.0.0", "1.0.0"), tarball)

	scratch := t.TempDir()
	inst := NewInstaller(nil, scratch)
	_, err := inst.Prepare(ctx, pkg, Options{
		Registry:         reg,
		FinalPluginDir:   filepath.Join(t.TempDir(), "plugins", "pkg"),
		BridgeExecutable: fakeBridge(t),
		ExpectedSHA256:   strings.Repeat("0", 64),
	})
	if err == nil {
		t.Fatal("Prepare with a wrong expected sha256 = nil error, want a mismatch error")
	}
	if !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("error = %q, want a sha256 mismatch", err)
	}
	entries, rerr := os.ReadDir(scratch)
	if rerr != nil {
		t.Fatalf("read scratch: %v", rerr)
	}
	if len(entries) != 0 {
		t.Fatalf("staging was not cleaned up: %v", entries)
	}
}

func TestPrepareNPMTraversalRejected(t *testing.T) {
	pkg := "evil"
	tarball := buildTarball(t, []tarEntry{{name: "../evil.txt", content: "x"}})
	reg, _ := fakeNPMRegistry(t, makeMeta(pkg, "1.0.0", "1.0.0"), tarball)
	_, err := NewInstaller(nil, "").Prepare(ctx, pkg, Options{
		Registry:         reg,
		FinalPluginDir:   filepath.Join(t.TempDir(), "plugins", "evil"),
		BridgeExecutable: fakeBridge(t),
	})
	if err == nil {
		t.Fatal("traversal tarball = nil error")
	}
	if !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("error = %q, want it to say escapes", err)
	}
}

func TestPrepareNPMSymlinkRejected(t *testing.T) {
	pkg := "evil"
	tarball := buildTarball(t, []tarEntry{{name: "package/link", link: "/etc/passwd"}})
	reg, _ := fakeNPMRegistry(t, makeMeta(pkg, "1.0.0", "1.0.0"), tarball)
	_, err := NewInstaller(nil, "").Prepare(ctx, pkg, Options{
		Registry:         reg,
		FinalPluginDir:   filepath.Join(t.TempDir(), "plugins", "evil"),
		BridgeExecutable: fakeBridge(t),
	})
	if err == nil {
		t.Fatal("symlink tarball = nil error")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("error = %q, want it to reject the non-regular entry", err)
	}
}

func TestPrepareNPMVersionConstraint(t *testing.T) {
	pkg := "@scope/pkg"
	tarball := buildTarball(t, []tarEntry{{name: "package/index.mjs", content: "x"}})
	reg, _ := fakeNPMRegistry(t, makeMeta(pkg, "2.0.0", "1.0.0", "1.2.3", "1.3.0", "2.0.0"), tarball)
	inst := NewInstaller(nil, "")
	opts := Options{
		Registry:         reg,
		FinalPluginDir:   filepath.Join(t.TempDir(), "plugins", "pkg"),
		BridgeExecutable: fakeBridge(t),
	}

	p, err := inst.Prepare(ctx, "@scope/pkg@^1.2.0", opts)
	if err != nil {
		t.Fatalf("Prepare ^1.2.0: %v", err)
	}
	defer inst.Cleanup(p)
	if p.Record.Version != "1.3.0" {
		t.Fatalf("^1.2.0 resolved to %q, want the highest matching 1.3.0", p.Record.Version)
	}

	p, err = inst.Prepare(ctx, "@scope/pkg", opts)
	if err != nil {
		t.Fatalf("Prepare latest: %v", err)
	}
	defer inst.Cleanup(p)
	if p.Record.Version != "2.0.0" {
		t.Fatalf("no constraint resolved to %q, want dist-tags.latest 2.0.0", p.Record.Version)
	}
}

// TestPrepareGitHubSpecUnreachableWithoutTransport pins the P2 acceptance
// "GitHub 源在无代理时报「源不可达（可配代理）」而非静默失败": with a transport
// that cannot reach the host (dead port), a github spec fails loudly with
// ErrGitHubUnreachable — it never silently succeeds or hangs. (The happy path
// is covered in github_test.go since D-04.)
func TestPrepareGitHubSpecRejected(t *testing.T) {
	dead, err := NewGitHubSource("http://127.0.0.1:1", nil)
	if err != nil {
		t.Fatalf("NewGitHubSource: %v", err)
	}
	_, err = NewInstaller(nil, "").Prepare(ctx, "github:foo/bar", Options{GitHub: dead})
	if !errors.Is(err, ErrGitHubUnreachable) {
		t.Fatalf("github spec error = %v, want ErrGitHubUnreachable", err)
	}
	text := err.Error()
	if !strings.Contains(text, "unreachable") || !strings.Contains(text, "proxy") {
		t.Fatalf("error text = %q, want an explicit unreachable/proxy message", text)
	}
}

// TestParseSpecGitHubBranch pins the D-04 branch suffix parsing.
func TestParseSpecGitHubBranch(t *testing.T) {
	cases := []struct {
		raw    string
		owner  string
		repo   string
		branch string
	}{
		{"github:acme/widget", "acme", "widget", ""},
		{"gh:acme/widget", "acme", "widget", ""},
		{"github:acme/widget.git", "acme", "widget", ""},
		{"github:acme/widget@dev", "acme", "widget", "dev"},
		{"gh:acme/widget@release-1.2", "acme", "widget", "release-1.2"},
		{"https://github.com/acme/widget", "acme", "widget", ""},
	}
	for _, tc := range cases {
		sp, err := ParseSpec(tc.raw)
		if err != nil {
			t.Fatalf("ParseSpec(%q): %v", tc.raw, err)
		}
		if sp.Kind != KindGitHub || sp.Owner != tc.owner || sp.Repo != tc.repo || sp.Branch != tc.branch {
			t.Fatalf("ParseSpec(%q) = %+v, want owner=%s repo=%s branch=%s", tc.raw, sp, tc.owner, tc.repo, tc.branch)
		}
	}
	// 非法分支名在解析层就拒绝。
	if _, err := ParseSpec("github:acme/widget@../evil"); err == nil {
		t.Fatal("invalid branch must be rejected at parse time")
	}
}

func TestPrepareLocalGeneratesBridgeManifest(t *testing.T) {
	src := filepath.Join(t.TempDir(), "my-plugin")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "plugin.mjs"), []byte("export {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	inst := NewInstaller(nil, filepath.Join(base, "scratch"))
	p, err := inst.Prepare(ctx, src, Options{
		FinalPluginDir:   filepath.Join(base, "plugins", "my-plugin"),
		BridgeExecutable: fakeBridge(t),
	})
	if err != nil {
		t.Fatalf("Prepare local: %v", err)
	}
	defer inst.Cleanup(p)
	if p.Record.Kind != "local" {
		t.Fatalf("kind = %q, want local", p.Record.Kind)
	}
	if p.Manifest.ID != "my-plugin" {
		t.Fatalf("id = %q, want my-plugin", p.Manifest.ID)
	}
	if p.Record.SHA256 != "" {
		t.Fatalf("local install must carry no tarball sha256, got %q", p.Record.SHA256)
	}
	if _, err := os.Stat(filepath.Join(p.Root, opchost.DefaultBridgeExeName)); err != nil {
		t.Fatalf("bridge exe not deployed for a generated local manifest: %v", err)
	}
	if _, err := os.Stat(filepath.Join(p.Root, "plugin.mjs")); err != nil {
		t.Fatalf("local payload not copied: %v", err)
	}
}

func TestPrepareLocalKeepsShippedManifest(t *testing.T) {
	src := t.TempDir()
	manifest := model.Manifest{ID: "my-local", APILevel: 1, Entry: model.Entry{Exe: "main.exe"}}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "manifest.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "main.exe"), []byte("MZ"), 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := NewInstaller(nil, "").Prepare(ctx, src, Options{})
	if err != nil {
		t.Fatalf("Prepare local with manifest: %v", err)
	}
	defer NewInstaller(nil, "").Cleanup(p)
	if p.Manifest.ID != "my-local" || p.Manifest.Entry.Exe != "main.exe" {
		t.Fatalf("shipped manifest not kept: %+v", p.Manifest)
	}
	if p.Record.Kind != "local" {
		t.Fatalf("kind = %q, want local", p.Record.Kind)
	}
}

func TestPrepareLocalMissingDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	_, err := NewInstaller(nil, "").Prepare(ctx, "file:"+missing, Options{})
	if err == nil || !strings.Contains(err.Error(), "not an existing directory") {
		t.Fatalf("missing local source error = %v", err)
	}
}
