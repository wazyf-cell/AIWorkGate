//go:build !windows

package hosts

import "syscall"

// hideWindow is a no-op outside Windows (the host itself is Windows-only;
// this keeps the package buildable for tests on other platforms).
func hideWindow() *syscall.SysProcAttr { return nil }
