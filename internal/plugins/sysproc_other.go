//go:build !windows

package plugins

import (
	"os"
	"syscall"
	"time"
)

// hideWindow is a no-op outside Windows; the host itself ships Windows-only,
// this keeps the package testable on other platforms.
func hideWindow() *syscall.SysProcAttr { return nil }

// pluginProcAttr is a no-op outside Windows.
func pluginProcAttr() *syscall.SysProcAttr { return hideWindow() }

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
