//go:build !windows

package plugins

// Linux-only tests for P8-① uid isolation (sysproc_other.go).
//
// These tests need real os/user and root semantics, so they only execute on a
// Linux host. On the Windows build host they are not compiled at all (build
// tag); Linux-side verification there is limited to compiling this file into
// an ELF test binary inside a standalone probe module (the repo's full tree
// cannot cross-compile today: internal/secrets/dpapi.go imports
// golang.org/x/sys/windows unconditionally - a pre-existing Linux gap owned by
// P4, not by this card). Runtime behavior on a real server stays UNKNOWN until
// P7 部署实跑; each test self-skips rather than pretending to pass.

import (
	"os"
	"os/user"
	"strconv"
	"strings"
	"testing"
)

// TestPluginProcAttrMissingUserNeverCrashes pins the crash-safety contract for
// any euid: a configured-but-unusable account falls back to the host uid with
// a non-empty reason - never panic, never a half-applied credential.
func TestPluginProcAttrMissingUserNeverCrashes(t *testing.T) {
	attr, reason := pluginProcAttr("p8-no-such-user-zz-404")
	if attr != nil {
		t.Fatalf("unusable user must yield nil attr (spawn as host), got %#v", attr)
	}
	if reason == "" {
		t.Fatal("unusable user must yield a non-empty reason for the supervisor log")
	}
	if len(reason) > 400 {
		t.Fatalf("reason must stay short for log lines, got %d bytes", len(reason))
	}
}

// TestPluginProcAttrMissingUserFallsBack pins the lookup path specifically as
// root, where it is reached at all (on a non-root host the root gate answers
// first, so asserting "not found" there would be wrong).
func TestPluginProcAttrMissingUserFallsBack(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("需要 root：非 root 时先命中 root 闸门，测不到用户解析")
	}
	attr, reason := pluginProcAttr("p8-no-such-user-zz-404")
	if attr != nil {
		t.Fatalf("missing user must yield nil attr (spawn as host), got %#v", attr)
	}
	if !strings.Contains(reason, "not found") {
		t.Fatalf("reason should name the failure, got %q", reason)
	}
}

// TestPluginProcAttrRequiresRoot pins the root gate: without euid 0 the
// function must refuse to switch uid even when the user exists.
func TestPluginProcAttrRequiresRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("skipped while running as root: this test asserts the non-root gate")
	}
	attr, reason := pluginProcAttr("root")
	if attr != nil {
		t.Fatalf("non-root must not produce a credential attribute, got %#v", attr)
	}
	if !strings.Contains(reason, "root") {
		t.Fatalf("reason should mention the root requirement, got %q", reason)
	}
}

// TestPluginProcAttrSandboxUserApplied executes the real switch path: only
// possible as root with the aiworkgate-plugin account present. On machines
// without either it self-skips instead of pretending to pass.
func TestPluginProcAttrSandboxUserApplied(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("需要 root：换 uid 语义只能在 root 下成立（脚本用 sudo 跑）")
	}
	const want = "aiworkgate-plugin"
	u, err := user.Lookup(want)
	if err != nil {
		t.Skipf("系统不存在用户 %s（先跑 scripts/p8/p8-create-plugin-user.sh --apply）；跳过真实换 uid 断言", want)
	}
	wantUID, _ := strconv.ParseUint(u.Uid, 10, 32)
	wantGID, _ := strconv.ParseUint(u.Gid, 10, 32)

	attr, reason := pluginProcAttr(want)
	if reason != "" {
		t.Fatalf("enabled+root+exists must yield no reason, got %q", reason)
	}
	if attr == nil || attr.Credential == nil {
		t.Fatalf("enabled+root+exists must yield a Credential attribute, got %#v", attr)
	}
	if attr.Credential.Uid != uint32(wantUID) || attr.Credential.Gid != uint32(wantGID) {
		t.Fatalf("credential uid/gid = %d/%d, want %d/%d",
			attr.Credential.Uid, attr.Credential.Gid, wantUID, wantGID)
	}
	if attr.Credential.NoSetGroups {
		t.Fatal("NoSetGroups must stay false so Go clears supplementary groups on exec")
	}
}

// TestPluginProcAttrTrimmedUser pins that a whitespace-padded config value is
// treated as "off" (both supervisor.go and pluginProcAttr trim; the function
// must not mistake padding for a real account name).
func TestPluginProcAttrTrimmedUser(t *testing.T) {
	attr, reason := pluginProcAttr("   ")
	if attr != nil || reason != "" {
		t.Fatalf("whitespace-only user must behave as off, got attr=%#v reason=%q", attr, reason)
	}
}
