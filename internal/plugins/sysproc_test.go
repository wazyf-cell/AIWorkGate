package plugins

// Cross-platform tests for pluginProcAttr (P8-① uid isolation).
//
// These run everywhere, including Windows (no build tag). The Windows
// implementation ignores sandboxUser and must keep hideWindow semantics
// byte-for-byte; the Linux side is exercised further in sysproc_linux_test.go
// (build-tagged !windows) because it needs real os/user and root semantics.

import (
	"runtime"
	"testing"
)

// TestPluginProcAttrOffByDefault pins the opt-in contract: an empty
// Options.PluginUser must produce today's exact behavior on every platform
// and must never log a warning ("" = off is not an error).
func TestPluginProcAttrOffByDefault(t *testing.T) {
	attr, reason := pluginProcAttr("")
	if reason != "" {
		t.Fatalf("opt-in off must not report a reason, got %q", reason)
	}
	if runtime.GOOS == "windows" {
		if attr == nil || !attr.HideWindow {
			t.Fatalf("windows: off must keep hideWindow attr, got %#v", attr)
		}
		return
	}
	if attr != nil {
		t.Fatalf("non-windows: off must yield nil attr (spawn as host), got %#v", attr)
	}
}

// TestPluginProcAttrWindowsKeepsHideWindow guards the Windows behavior
// boundary: the sandbox parameter is ignored there and the historical
// hideWindow attribute is returned even when a sandbox user is configured.
func TestPluginProcAttrWindowsKeepsHideWindow(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-specific semantics")
	}
	attr, reason := pluginProcAttr("aiworkgate-plugin")
	if reason != "" {
		t.Fatalf("windows must ignore sandboxUser entirely, got reason %q", reason)
	}
	if attr == nil || !attr.HideWindow {
		t.Fatalf("windows must keep hideWindow attr, got %#v", attr)
	}
}

// TestPluginProcAttrWarnCallbackShape documents the reason-string shape the
// supervisor turns into a warning; kept here so the contract is greppable in
// tests on any platform.
func TestPluginProcAttrWarnCallbackShape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows ignores sandboxUser")
	}
	attr, reason := pluginProcAttr("p8-no-such-user-zz")
	if attr != nil {
		t.Fatalf("missing user must yield nil attr, got %#v", attr)
	}
	if reason == "" {
		t.Fatal("missing user must yield a non-empty reason for the supervisor log")
	}
	if len(reason) > 400 {
		t.Fatalf("reason must stay short for log lines, got %d bytes", len(reason))
	}
}
