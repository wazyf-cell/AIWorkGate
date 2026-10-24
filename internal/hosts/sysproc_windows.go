//go:build windows

package hosts

import "syscall"

// createNoWindow is CREATE_NO_WINDOW: A.exe is a GUI binary, but a console
// child would still flash a window when launched from a non-console host.
const createNoWindow = 0x08000000

// hideWindow keeps A's CLI task from showing a console window.
func hideWindow() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
}
