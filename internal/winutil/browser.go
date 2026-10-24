//go:build windows

package winutil

import (
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
	"syscall"
	"unicode"

	"golang.org/x/sys/windows"
)

// ValidateURL accepts only absolute http/https URLs and rejects characters
// that would be dangerous once the URL is pasted into a shell command line.
// The returned URL is the canonical form produced by net/url.
func ValidateURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("winutil: empty url")
	}
	for _, r := range raw {
		if r == '"' || r == '\'' || unicode.IsControl(r) || unicode.IsSpace(r) {
			return "", fmt.Errorf("winutil: url contains illegal character %q", r)
		}
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("winutil: invalid url %q: %w", raw, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return "", fmt.Errorf("winutil: unsupported url scheme %q (want http or https)", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("winutil: url %q has no host", raw)
	}
	return u.String(), nil
}

// OpenBrowser opens u in the user's default browser. The URL is validated
// first; non-http(s) input is rejected without launching anything.
func OpenBrowser(rawURL string) error {
	u, err := ValidateURL(rawURL)
	if err != nil {
		return err
	}

	// rundll32 hands the URL to the shell's FileProtocolHandler without an
	// intermediate cmd.exe, so no extra quoting/expansion rules apply.
	if err := startDetached("rundll32.exe", "url.dll,FileProtocolHandler", u); err == nil {
		return nil
	}

	// Fallback: cmd /c start "" "<url>". CmdLine is set explicitly because
	// os/exec would otherwise quote for CreateProcess, not for cmd.exe.
	cmd := exec.Command("cmd.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine:       `/c start "" "` + u + `"`,
		HideWindow:    true,
		CreationFlags: windows.CREATE_NO_WINDOW,
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("winutil: open browser: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// startDetached starts argv without a console window and without waiting for
// the child to exit.
func startDetached(argv ...string) error {
	name, args := argv[0], argv[1:]
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = ExecHideWindow()
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
