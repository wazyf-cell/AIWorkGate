//go:build windows

package notify

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"

	"github.com/gen2brain/beeep"

	"aiworkgate/internal/tray"
)

// toastIconFile is the branded logo handed to the Windows toast. It is written
// once next to config.json (%APPDATA%\AIWorkGate) because the toast API needs
// an absolute file path that outlives the call.
const toastIconFile = "toast-icon.png"

var (
	toastIconOnce sync.Once
	toastIconPath string
)

// sendDesktopToast raises one Windows toast. A missing icon path is a cosmetic
// loss, not an error: beeep accepts "" and still shows the message.
func sendDesktopToast(title, body string) error {
	beeep.AppName = toastAppName
	return beeep.Notify(title, body, toastIcon())
}

// toastIcon returns the absolute path of the tray icon re-encoded as PNG, or ""
// when it cannot be materialised.
func toastIcon() string {
	toastIconOnce.Do(func() { toastIconPath = writeToastIcon() })
	return toastIconPath
}

// writeToastIcon materialises the generated tray icon as a PNG file. The tray
// icon is an ICO, which the toast API rejects, so it is converted first.
func writeToastIcon() string {
	logo, err := pngFromICO(tray.IconBytes())
	if err != nil {
		return ""
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	dir = filepath.Join(dir, toastAppName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	path := filepath.Join(dir, toastIconFile)
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, logo) {
		return path
	}
	if err := os.WriteFile(path, logo, 0o600); err != nil {
		return ""
	}
	return path
}
