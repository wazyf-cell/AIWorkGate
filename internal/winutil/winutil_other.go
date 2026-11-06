//go:build !windows

// Linux/macOS equivalents of the small Windows helpers the host needs.
// Autostart uses the XDG autostart directory, the browser goes through
// xdg-open/open, and single-instance is an advisory flock on a temp file.

package winutil

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

// ValidateURL accepts only absolute http(s) URLs with a host.
func ValidateURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("winutil: empty URL")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("winutil: invalid URL %q", raw)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("winutil: only http(s) URLs are allowed")
	}
	return parsed.String(), nil
}

// OpenBrowser hands the URL to the desktop opener.
func OpenBrowser(rawURL string) error {
	target, err := ValidateURL(rawURL)
	if err != nil {
		return err
	}
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	cmd := exec.Command(opener, target)
	return cmd.Start()
}

// ExecHideWindow has no meaning off Windows.
func ExecHideWindow() *syscall.SysProcAttr { return nil }

func autostartDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "autostart"), nil
}

func autostartFile(name string) (string, error) {
	dir, err := autostartDir()
	if err != nil {
		return "", err
	}
	name = strings.TrimSpace(strings.ReplaceAll(name, string(filepath.Separator), "_"))
	if name == "" {
		return "", errors.New("winutil: empty autostart name")
	}
	return filepath.Join(dir, name+".desktop"), nil
}

// SetAutostart writes an XDG desktop entry that starts exe on login.
func SetAutostart(name, exePath string) error {
	path, err := autostartFile(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	entry := fmt.Sprintf("[Desktop Entry]\nType=Application\nName=%s\nExec=%q\nX-GNOME-Autostart-enabled=true\n", name, exePath)
	return os.WriteFile(path, []byte(entry), 0o600)
}

// RemoveAutostart deletes the desktop entry (missing is fine).
func RemoveAutostart(name string) error {
	path, err := autostartFile(name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// AutostartPath reports whether the desktop entry exists.
func AutostartPath(name string) (string, bool, error) {
	path, err := autostartFile(name)
	if err != nil {
		return "", false, err
	}
	_, statErr := os.Stat(path)
	if statErr == nil {
		return path, true, nil
	}
	if errors.Is(statErr, os.ErrNotExist) {
		return path, false, nil
	}
	return path, false, statErr
}

// ListRunEntries reads the autostart directory into name → Exec command.
func ListRunEntries() (map[string]string, error) {
	dir, err := autostartDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	out := make(map[string]string, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".desktop") {
			continue
		}
		raw, readErr := os.ReadFile(filepath.Join(dir, entry.Name()))
		if readErr != nil {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".desktop")
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, "Exec=") {
				out[name] = strings.TrimSpace(strings.TrimPrefix(line, "Exec="))
				break
			}
		}
	}
	return out, nil
}

// AcquireSingleInstance takes an advisory lock; release must be called once.
func AcquireSingleInstance(name string) (release func(), alreadyRunning bool, err error) {
	name = strings.TrimSpace(strings.ReplaceAll(name, string(filepath.Separator), "_"))
	if name == "" {
		name = "aiworkgate"
	}
	path := filepath.Join(os.TempDir(), name+".lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return func() {}, true, nil
		}
		return nil, false, err
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, false, nil
}
