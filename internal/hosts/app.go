package hosts

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// StartApp launches A's GUI executable without waiting for it. A keeps its own
// window and tray, so no hidden-window attribute is used. An already running
// instance is handled by A's own single-instance plugin.
func StartApp(exePath string) error {
	exePath = strings.TrimSpace(exePath)
	if exePath == "" {
		return errors.New("hosts: empty A executable path")
	}
	if info, err := os.Stat(exePath); err != nil {
		return fmt.Errorf("hosts: A executable %q: %w", exePath, err)
	} else if info.IsDir() {
		return fmt.Errorf("hosts: A executable %q is a directory", exePath)
	}
	cmd := exec.Command(exePath)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("hosts: start A: %w", err)
	}
	return cmd.Process.Release()
}
