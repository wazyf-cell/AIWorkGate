//go:build !windows

package plugins

import (
	"os"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// hideWindow is a no-op outside Windows; the host itself ships Windows-only,
// this keeps the package testable on other platforms.
func hideWindow() *syscall.SysProcAttr { return nil }

// defaultPortVerifyTimeout mirrors the Windows value: the wait for a child's
// listener before the host reports the plugin as failed to start.
const defaultPortVerifyTimeout = 5 * time.Second

// pluginProcAttr builds the SysProcAttr used to spawn one plugin child.
//
// P8-① (uid isolation, docs/p8/00 §4.4): when the host opted into a sandbox
// user via Options.PluginUser AND runs as root AND that user exists, the
// child is spawned under the plugin account's UID/GID instead of the host's.
// Anything else returns a nil attr (spawn as the host, today's behavior) plus
// a non-empty reason the caller should log; an empty reason means "opt-in is
// off", which is not an error.
//
// The user-name to UID/GID resolution is deliberately exposed through the
// pluginProcAttr signature so the pure lookup logic is callable from tests
// (see sysproc_linux_test.go); on Windows this value is ignored.
func pluginProcAttr(sandboxUser string) (*syscall.SysProcAttr, string) {
	sandboxUser = strings.TrimSpace(sandboxUser)
	if sandboxUser == "" {
		return hideWindow(), "" // 未启用（缺省关闭，避免现有部署行为突变）
	}
	if os.Geteuid() != 0 {
		return nil, "host is not running as root; plugin user isolation requires root"
	}
	u, err := user.Lookup(sandboxUser)
	if err != nil {
		return nil, "plugin sandbox user " + sandboxUser + " not found; falling back to host uid"
	}
	uid, errU := strconv.ParseUint(u.Uid, 10, 32)
	gid, errG := strconv.ParseUint(u.Gid, 10, 32)
	if errU != nil || errG != nil {
		return nil, "plugin sandbox user " + sandboxUser + " has non-numeric uid/gid (uid=" + u.Uid + " gid=" + u.Gid + ")"
	}
	return &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid: uint32(uid), Gid: uint32(gid),
			// NoSetGroups=false 由 Go 的 exec 代为清除补充组，避免继承宿主组身份。
			NoSetGroups: false,
		},
	}, ""
}

// EnsureJobObject is a no-op outside Windows: job objects are the Windows-only
// guard against orphaned plugin children.
func EnsureJobObject() error { return nil }

// assignToJob is a no-op outside Windows.
func assignToJob(*os.Process) error { return nil }

// gracefulStop interrupts the process and reports whether a graceful path was
// attempted.
func gracefulStop(proc *os.Process) bool {
	if proc == nil {
		return false
	}
	return proc.Signal(os.Interrupt) == nil
}

// killProcessTree force-terminates the process.
func killProcessTree(proc *os.Process) {
	if proc == nil {
		return
	}
	_ = proc.Kill()
}

// verifyPortOwner is a no-op outside Windows: the token-race guard is built on
// the Windows TCP table and the host itself ships Windows-only. Keeping the
// stub here lets the package cross-compile for tests.
func verifyPortOwner(port int, pid int, timeout time.Duration) error { return nil }
