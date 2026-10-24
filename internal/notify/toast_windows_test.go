//go:build windows

package notify

import (
	"bytes"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"aiworkgate/internal/tray"
)

// TestTrayIconReencodedAsPNG proves the branded toast logo is derived from the
// generated tray icon, since the Windows toast API rejects ICO.
func TestTrayIconReencodedAsPNG(t *testing.T) {
	logo, err := pngFromICO(tray.IconBytes())
	if err != nil {
		t.Fatalf("pngFromICO: %v", err)
	}
	if !bytes.HasPrefix(logo, pngSignature) {
		t.Fatal("toast icon is not a PNG")
	}
	img, err := png.Decode(bytes.NewReader(logo))
	if err != nil {
		t.Fatalf("decode PNG: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 16 || b.Dy() != 16 {
		t.Fatalf("icon size = %dx%d, want 16x16", b.Dx(), b.Dy())
	}
	opaque := false
	for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y && !opaque; y++ {
		for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a != 0 {
				opaque = true
				break
			}
		}
	}
	if !opaque {
		t.Fatal("toast icon is fully transparent")
	}
}

// TestManualToast is a manual probe, not part of the automated suite: it puts a
// real notification on the desktop, so it only runs when explicitly asked:
//
//	AIWORKGATE_TOAST_PROBE=1 go test ./internal/notify/ -run TestManualToast -count=1 -v
func TestManualToast(t *testing.T) {
	if os.Getenv("AIWORKGATE_TOAST_PROBE") != "1" {
		t.Skip("manual probe; set AIWORKGATE_TOAST_PROBE=1 to show a real toast")
	}
	if err := sendDesktopToast("AIWorkGate 测试通知", "这是一条手动验证的桌面通知，说明系统通知可用。"); err != nil {
		t.Fatalf("sendDesktopToast: %v", err)
	}
}

// TestWriteToastIconMaterialisesFile checks the icon is written once under the
// app data directory and reused when its bytes already match.
func TestWriteToastIconMaterialisesFile(t *testing.T) {
	t.Setenv("APPDATA", t.TempDir())

	path := writeToastIcon()
	if path == "" {
		t.Fatal("writeToastIcon returned no path")
	}
	if filepath.Dir(path) != filepath.Join(os.Getenv("APPDATA"), toastAppName) {
		t.Fatalf("icon path = %q, want it under the data dir", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read icon: %v", err)
	}
	if !bytes.HasPrefix(data, pngSignature) {
		t.Fatal("written icon is not a PNG")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat icon: %v", err)
	}
	if again := writeToastIcon(); again != path {
		t.Fatalf("second path = %q, want %q", again, path)
	}
	if after, err := os.Stat(path); err != nil || after.ModTime() != info.ModTime() {
		t.Fatal("existing icon was rewritten")
	}
}
