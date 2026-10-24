//go:build windows

// Package winutil holds the small Win32 helpers the AIWorkGate host needs:
// single-instance mutex, HKCU Run autostart entries, browser launching and
// console-window hiding for child processes. It is Windows-only (the v1 host
// targets Windows exclusively) and uses no cgo.
package winutil
