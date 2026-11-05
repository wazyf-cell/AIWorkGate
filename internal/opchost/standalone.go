package opchost

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Allocator is the host's port/token authority as an injectable seam. It exists
// so this package never re-implements allocation: the production implementation
// is the supervisor's allocatePort (internal/plugins/supervisor.go:479-494) plus
// secrets.Store.EnsurePluginToken (internal/secrets/secrets.go:151-175), and tests
// inject a fake.
//
// A manifest never needs it — the supervisor allocates and injects on every
// launch. It is used only by StandaloneCommand, the hand-run/debug path.
type Allocator interface {
	// AllocatePort reserves a free loopback port (reserve and release, exactly
	// like the supervisor, which verifies ownership after start).
	AllocatePort() (int, error)
	// EnsureToken returns the persisted bearer token of pluginID, creating it on
	// first use.
	EnsureToken(pluginID string) (string, error)
}

// StandaloneCommand resolves the bridge executable of a plugin directory and
// builds the four-flag argument vector used by a hand-run or debug launch:
//
//	magpie-bridge.exe --plugin-dir <dir> --port <n> --token <t> --data-dir <d>
//
// It is the standalone counterpart of BridgeArgs: the manifest carries only
// --plugin-dir because the supervisor injects everything else as environment
// variables, while a process started by hand has no supervisor to do that.
//
// The returned entry is relative to pluginDir, matching manifest.entry.exe, and
// the caller joins it onto pluginDir exactly as the supervisor does. Nothing is
// written to disk and no process is started.
func StandaloneCommand(alloc Allocator, pluginID, pluginDir, dataDir string) (entry string, args []string, err error) {
	if alloc == nil {
		return "", nil, errors.New("opchost: standalone: allocator is required")
	}
	pluginID = strings.TrimSpace(pluginID)
	if pluginID == "" {
		return "", nil, errors.New("opchost: standalone: plugin id is required")
	}
	pluginDir = strings.TrimSpace(pluginDir)
	if pluginDir == "" {
		return "", nil, errors.New("opchost: standalone: plugin dir is required")
	}
	dataDir = strings.TrimSpace(dataDir)
	if dataDir == "" {
		return "", nil, errors.New("opchost: standalone: data dir is required")
	}
	entry, err = ResolveBridgeEntry(pluginDir, "")
	if err != nil {
		return "", nil, err
	}
	port, err := alloc.AllocatePort()
	if err != nil {
		return "", nil, fmt.Errorf("opchost: standalone %q: allocate port: %w", pluginID, err)
	}
	if port <= 0 || port > 65535 {
		return "", nil, fmt.Errorf("opchost: standalone %q: allocator returned port %d out of range", pluginID, port)
	}
	token, err := alloc.EnsureToken(pluginID)
	if err != nil {
		return "", nil, fmt.Errorf("opchost: standalone %q: ensure token: %w", pluginID, err)
	}
	if strings.TrimSpace(token) == "" {
		return "", nil, fmt.Errorf("opchost: standalone %q: allocator returned an empty token", pluginID)
	}
	return entry, StandaloneArgs(pluginDir, port, token, dataDir), nil
}

// StandaloneArgs returns the four-flag debug argument vector. It is exported
// next to BridgeArgs so the difference between the two forms is explicit:
// BridgeArgs is what goes into manifest.entry.args, StandaloneArgs is only for a
// process started by hand.
func StandaloneArgs(pluginDir string, port int, token, dataDir string) []string {
	return []string{
		argPluginDir, pluginDir,
		"--port", strconv.Itoa(port),
		"--token", token,
		"--data-dir", dataDir,
	}
}
