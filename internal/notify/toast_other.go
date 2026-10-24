//go:build !windows

package notify

// sendDesktopToast is a no-op outside Windows: AIWorkGate is a Windows tray
// host and the non-Windows build only exists so the package stays testable.
func sendDesktopToast(string, string) error {
	return nil
}
