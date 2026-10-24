//go:build windows

package winutil

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// ExecHideWindow returns the SysProcAttr used for plugin and helper child
// processes: it suppresses the console window Windows would otherwise create
// for console executables spawned by a GUI binary.
//
// Note that CREATE_NO_WINDOW also removes Ctrl+C delivery to the child; the
// host must stop plugins with its own protocol or TerminateProcess.
func ExecHideWindow() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_NO_WINDOW,
	}
}
