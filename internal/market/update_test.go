package market

import (
	"strings"
	"testing"
)

func TestCheckUpdateFindsDiff(t *testing.T) {
	pkg := "@magpie-community/opencode-trae-auth"
	reg, _ := fakeNPMRegistry(t, makeMeta(pkg, "1.2.0", "1.0.0", "1.2.0"),
		buildTarball(t, []tarEntry{{name: "package/index.mjs", content: "x"}}))

	info, err := CheckUpdate(ctx, reg, pkg, "1.0.0", UpdatePolicyPrompt)
	if err != nil {
		t.Fatalf("CheckUpdate: %v", err)
	}
	if !info.Available {
		t.Fatal("1.0.0 -> 1.2.0 must be available")
	}
	if info.From != "1.0.0" || info.To != "1.2.0" {
		t.Fatalf("diff = %s -> %s, want 1.0.0 -> 1.2.0", info.From, info.To)
	}
	if info.PluginID != "opencode-trae-auth" {
		t.Fatalf("plugin_id = %q", info.PluginID)
	}
	if info.Policy != UpdatePolicyPrompt {
		t.Fatalf("policy = %q, want prompt", info.Policy)
	}
	if info.TarballURL == "" || !strings.HasSuffix(info.TarballURL, "-1.2.0.tgz") {
		t.Fatalf("tarball url = %q", info.TarballURL)
	}
}

func TestCheckUpdateUpToDateAndPolicyStructure(t *testing.T) {
	pkg := "pkg"
	reg, _ := fakeNPMRegistry(t, makeMeta(pkg, "1.2.0", "1.0.0", "1.2.0"), nil)

	info, err := CheckUpdate(ctx, reg, pkg, "1.2.0", UpdatePolicyAuto)
	if err != nil {
		t.Fatalf("CheckUpdate: %v", err)
	}
	if info.Available {
		t.Fatal("current == latest must not be available")
	}
	if info.To != "1.2.0" || info.Policy != UpdatePolicyAuto {
		t.Fatalf("up-to-date info = %+v", info)
	}

	// 旧版本回滚场景不会报错，只是不"可升级"（从 2.0.0 到 1.2.0）。
	info, err = CheckUpdate(ctx, reg, pkg, "2.0.0", UpdatePolicyPrompt)
	if err != nil {
		t.Fatalf("CheckUpdate (newer installed): %v", err)
	}
	if info.Available {
		t.Fatal("a downgrade must not be reported as available")
	}
}

func TestCheckUpdatePrereleaseNeverCounts(t *testing.T) {
	pkg := "pkg"
	// dist-tags.latest stays on the newest stable (1.0.0) while a prerelease
	// (2.0.0-rc1) exists: npm semantics, so no update and latest == 1.0.0.
	reg, _ := fakeNPMRegistry(t, makeMeta(pkg, "1.0.0", "1.0.0", "2.0.0-rc1"), nil)
	info, err := CheckUpdate(ctx, reg, pkg, "1.0.0", UpdatePolicyPrompt)
	if err != nil {
		t.Fatalf("CheckUpdate: %v", err)
	}
	if info.Available {
		t.Fatalf("a prerelease must never count as an update; info = %+v", info)
	}
	if info.To != "1.0.0" {
		t.Fatalf("to = %q, want the latest dist-tag 1.0.0 (not the prerelease)", info.To)
	}
}
