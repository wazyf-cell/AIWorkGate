// Package hosts integrates the local "AI Work 助手" (A) installation into
// AIWorkGate without modifying A (contract v0.2 §8.3, ledger F69/F73/F74/F75):
//
//   - discovery of A's data dir and gateway settings (read-only SQLite),
//   - an HTTP client for A's gateway (/health, /v1/models),
//   - a port probe used to decide whether A is running,
//   - "one-click check-in" through A's own CLI: A.exe --task-run <key>.
//
// Nothing in this package writes to A's files: the SQLite handle is opened
// with mode=ro and A's API key is never read (it lives in A's Stronghold
// vault, F73) — the caller supplies it.
package hosts

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// PortProbeTimeout bounds the TCP probe used by PortOpen.
const PortProbeTimeout = 300 * time.Millisecond

// ADataDir returns A's data directory: %APPDATA%\AIWorkAssistant.
// The directory is not required to exist; callers get a *DiscoverError with
// kind DiscoverErrDataDir when %APPDATA% is unavailable.
func ADataDir() (string, error) {
	appData := strings.TrimSpace(os.Getenv("APPDATA"))
	if appData == "" {
		return "", newDiscoverError(DiscoverErrDataDir, "", errors.New("%APPDATA% is not set"))
	}
	return filepath.Join(appData, appDirName), nil
}

// PortOpen reports whether 127.0.0.1:port accepts a TCP connection within
// PortProbeTimeout. Any dial failure (refused, timeout, bad port) yields false.
func PortOpen(port int) bool {
	if port < 1 || port > 65535 {
		return false
	}
	conn, err := net.DialTimeout("tcp",
		net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), PortProbeTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
