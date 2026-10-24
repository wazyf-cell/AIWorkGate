package app

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"aiworkgate/internal/fsutil"
	"aiworkgate/internal/hosts"
	"aiworkgate/internal/model"
)

const (
	// maxTailBytes bounds how much of the end of a log file Tail reads.
	maxTailBytes = 256 << 10
	// maxPortOffset is how many ports above cfg.Port may be tried.
	maxPortOffset = 10
)

// listenFirstAvailable tries ports start..start+tries-1 with bind and returns
// the first listener it could open. It is the testable core of port selection.
func listenFirstAvailable(start, tries int, bind func(int) (net.Listener, error)) (net.Listener, int, error) {
	if bind == nil {
		return nil, 0, errors.New("app: nil port binder")
	}
	if tries < 1 {
		tries = 1
	}
	if start < 1 {
		start = 1
	}
	var lastErr error
	last := start
	for i := 0; i < tries; i++ {
		port := start + i
		if port > 65535 {
			break
		}
		last = port
		ln, err := bind(port)
		if err == nil && ln != nil {
			return ln, port, nil
		}
		if err == nil {
			err = errors.New("binder returned a nil listener")
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no candidate port available")
	}
	return nil, 0, fmt.Errorf("no free port in %d-%d: %w", start, last, lastErr)
}

// listenGateway binds host on cfg.Port or one of the following 10 ports.
func listenGateway(host string, start, extra int) (net.Listener, int, error) {
	bind := func(port int) (net.Listener, error) {
		return net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	}
	return listenFirstAvailable(start, extra+1, bind)
}

// gatewayPortPath returns <dataDir>\run\gateway.port.
func gatewayPortPath(dataDir string) string {
	return filepath.Join(dataDir, portDirName, portFileName)
}

// writeGatewayPort atomically records the actual listening port.
func writeGatewayPort(dataDir string, port int) error {
	path := gatewayPortPath(dataDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("app: create %s: %w", filepath.Dir(path), err)
	}
	if err := fsutil.WriteFileAtomic(path, []byte(strconv.Itoa(port)), 0o644); err != nil {
		return fmt.Errorf("app: write %s: %w", path, err)
	}
	return nil
}

// readGatewayPort returns the port recorded by a running instance, or 0.
func readGatewayPort(dataDir string) int {
	raw, err := os.ReadFile(gatewayPortPath(dataDir))
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || port < 1 || port > 65535 {
		return 0
	}
	return port
}

// tailFileLines returns up to n trailing lines of path, reading at most
// maxTailBytes from the end. A missing file yields no lines and no error.
func tailFileLines(path string, n int) ([]string, error) {
	if n <= 0 {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	start := size - maxTailBytes
	if start < 0 {
		start = 0
	}
	buf := make([]byte, size-start)
	if len(buf) > 0 {
		if _, err := f.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
	}
	lines := strings.Split(string(buf), "\n")
	if start > 0 && len(lines) > 0 {
		lines = lines[1:] // the first line is very likely cut in half
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

// parseTimeQuery accepts unix seconds or a local date/time string. A date-only
// "to" bound expands to the last second of that day.
func parseTimeQuery(raw string, endOfDay bool) (int64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if n <= 0 {
			return 0, false
		}
		return n, true
	}
	layouts := []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04",
		"2006-01-02",
	}
	for _, layout := range layouts {
		t, err := time.ParseInLocation(layout, raw, time.Local)
		if err != nil {
			continue
		}
		if layout == "2006-01-02" && endOfDay {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, t.Location()).Add(-time.Second)
		}
		return t.Unix(), true
	}
	return 0, false
}

// adminAuthRequired reports whether an admin (non-/v1) request must present
// the LAN password. When LAN mode is configured but no password exists,
// VerifyLANPassword always fails, so non-loopback requests stay denied.
func adminAuthRequired(listenLAN, isLoopback, isV1 bool) bool {
	return listenLAN && !isLoopback && !isV1
}

// isV1Path reports whether path belongs to the OpenAI-compatible API.
func isV1Path(path string) bool {
	return path == "/v1" || strings.HasPrefix(path, "/v1/")
}

// isLoopbackRemote reports whether a request's RemoteAddr is loopback. A value
// that cannot be parsed is treated as remote (fail closed).
func isLoopbackRemote(remote string) bool {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return false
	}
	host := remote
	if h, _, err := net.SplitHostPort(remote); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// findClientKey looks a key id up in a client key list.
func findClientKey(keys []model.ClientKey, id string) (model.ClientKey, bool) {
	for _, key := range keys {
		if key.ID == id {
			return key, true
		}
	}
	return model.ClientKey{}, false
}

// portFromBaseURL extracts an explicit port from a base URL, falling back to
// A's default gateway port.
func portFromBaseURL(raw string) int {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return hosts.DefaultGatewayPort
	}
	p := u.Port()
	if p == "" {
		return hosts.DefaultGatewayPort
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 {
		return hosts.DefaultGatewayPort
	}
	return n
}

// newProxyID returns "px_" plus 8 hex characters.
func newProxyID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "px_" + strconv.FormatUint(uint64(time.Now().UnixNano())&0xffffffff, 16)
	}
	return "px_" + hex.EncodeToString(b[:])
}
