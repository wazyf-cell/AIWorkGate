//go:build windows

package winutil

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// RunKeyPath is the HKCU Run key AIWorkGate (and the AI Work assistant) write
// autostart entries to.
const RunKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`

// SetAutostart registers exePath under name in HKCU\...\Run. The stored value
// is the quoted path so Windows resolves executables that live in folders with
// spaces (e.g. `C:\Program Files\AIWorkGate`). exePath must be a bare path:
// quotes are rejected so a value can never smuggle in extra command-line
// arguments.
func SetAutostart(name, exePath string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("winutil: autostart value name is empty")
	}
	exePath = strings.TrimSpace(exePath)
	if exePath == "" {
		return errors.New("winutil: autostart exe path is empty")
	}
	if strings.ContainsAny(exePath, "\"\r\n") {
		return fmt.Errorf("winutil: autostart exe path contains illegal characters: %q", exePath)
	}

	k, _, err := registry.CreateKey(registry.CURRENT_USER, RunKeyPath, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("winutil: open HKCU run key: %w", err)
	}
	defer k.Close()

	if err := k.SetStringValue(name, quoteExePath(exePath)); err != nil {
		return fmt.Errorf("winutil: write autostart %q: %w", name, err)
	}
	return nil
}

// RemoveAutostart deletes the Run value name. Removing an absent entry is not
// an error so callers can call it unconditionally.
func RemoveAutostart(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("winutil: autostart value name is empty")
	}

	k, err := registry.OpenKey(registry.CURRENT_USER, RunKeyPath, registry.SET_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("winutil: open HKCU run key: %w", err)
	}
	defer k.Close()

	if err := k.DeleteValue(name); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return fmt.Errorf("winutil: delete autostart %q: %w", name, err)
	}
	return nil
}

// AutostartPath returns the executable registered under name. found is false
// when the value does not exist (a missing Run key counts as not found). Any
// arguments stored after the path are dropped; only the exe path is returned.
func AutostartPath(name string) (path string, found bool, err error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", false, errors.New("winutil: autostart value name is empty")
	}

	k, err := registry.OpenKey(registry.CURRENT_USER, RunKeyPath, registry.QUERY_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("winutil: open HKCU run key: %w", err)
	}
	defer k.Close()

	raw, _, err := k.GetStringValue(name)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("winutil: read autostart %q: %w", name, err)
	}

	exe, _ := parseRunCommand(raw)
	return exe, true, nil
}

// ListRunEntries returns every string value of the HKCU Run key, exactly as
// stored (quotes and arguments included). Non-string values are skipped.
func ListRunEntries() (map[string]string, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, RunKeyPath, registry.QUERY_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("winutil: open HKCU run key: %w", err)
	}
	defer k.Close()

	names, err := k.ReadValueNames(-1)
	if err != nil {
		return nil, fmt.Errorf("winutil: enumerate HKCU run values: %w", err)
	}

	entries := make(map[string]string, len(names))
	for _, n := range names {
		v, _, err := k.GetStringValue(n)
		if err != nil {
			// Values of other types (or removed mid-enumeration) are skipped.
			continue
		}
		entries[n] = v
	}
	return entries, nil
}

// quoteExePath wraps a bare executable path in double quotes.
func quoteExePath(exePath string) string {
	return `"` + exePath + `"`
}

// parseRunCommand splits a Run value into the executable path and its
// arguments. Quoted paths win; otherwise the first whitespace-delimited token
// is the path (matching how CreateProcess sees an unquoted command line).
func parseRunCommand(raw string) (exePath string, args []string) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	if s[0] == '"' {
		if end := strings.IndexByte(s[1:], '"'); end >= 0 {
			return s[1 : 1+end], strings.Fields(s[2+end:])
		}
		return s[1:], nil // unterminated quote: best effort
	}
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return "", nil
	}
	return fields[0], fields[1:]
}
