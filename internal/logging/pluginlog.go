package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// PluginLogWriter captures plugin stdout/stderr into
// logs\plugins\<id>-YYYY-MM-DD.log with daily and 10MB rotation, keeping the
// configured number of rotated generations.
type PluginLogWriter struct {
	dir         string
	maxSize     int64
	generations int
	now         func() time.Time

	mu    sync.Mutex
	files map[string]*dayFile
}

// NewPluginLogWriter prepares <dataDir>\logs\plugins.
func NewPluginLogWriter(dataDir string, list ...Option) (*PluginLogWriter, error) {
	o := resolveOptions(list)
	dir := filepath.Join(dataDir, logsDirName, pluginsDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("logging: create %s: %w", dir, err)
	}
	return &PluginLogWriter{
		dir:         dir,
		maxSize:     maxLogBytes,
		generations: o.generations,
		now:         time.Now,
		files:       make(map[string]*dayFile),
	}, nil
}

// SanitizePluginID lowercases id, maps everything outside [a-z0-9._-] to '_'
// and rejects empty results plus separators or ".." (path traversal).
func SanitizePluginID(id string) (string, error) {
	if id == "" {
		return "", fmt.Errorf("%w: empty", ErrInvalidPluginID)
	}
	if strings.ContainsAny(id, `/\:`) || strings.Contains(id, "..") {
		return "", fmt.Errorf("%w: %q contains a path separator or '..'", ErrInvalidPluginID, id)
	}
	var b strings.Builder
	b.Grow(len(id))
	for _, r := range strings.ToLower(id) {
		switch {
		case r >= 'a' && r <= 'z',
			r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), ".")
	if out == "" {
		return "", fmt.Errorf("%w: %q has no usable characters", ErrInvalidPluginID, id)
	}
	return out, nil
}

// Write appends one captured line for a plugin, adding a newline when the
// caller did not.
func (w *PluginLogWriter) Write(pluginID, line string) error {
	if w == nil {
		return nil
	}
	id, err := SanitizePluginID(pluginID)
	if err != nil {
		return err
	}
	if !strings.HasSuffix(line, "\n") {
		line += "\n"
	}
	now := w.now()

	w.mu.Lock()
	defer w.mu.Unlock()
	f := w.files[id]
	if f == nil {
		f = &dayFile{dir: w.dir, prefix: id + "-", ext: ".log", maxSize: w.maxSize, generations: w.generations}
		w.files[id] = f
	}
	return f.write(now, []byte(line))
}

// Tail returns the last n lines of a plugin's log for today, falling back to
// the rotated .1 file when needed. Only the last ~256KB of each file is read.
func (w *PluginLogWriter) Tail(pluginID string, n int) ([]string, error) {
	if w == nil {
		return nil, nil
	}
	id, err := SanitizePluginID(pluginID)
	if err != nil {
		return nil, err
	}
	if n <= 0 {
		n = 100
	}
	name := filepath.Join(w.dir, id+"-"+w.now().Format(dateLayout)+".log")

	w.mu.Lock()
	defer w.mu.Unlock()

	lines, err := tailLines(name, n)
	if err != nil {
		return nil, err
	}
	if len(lines) < n {
		prev, err := tailLines(name+".1", n-len(lines))
		if err != nil {
			return nil, err
		}
		if len(prev) > 0 {
			lines = append(prev, lines...)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

// Close releases every open plugin log file.
func (w *PluginLogWriter) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	var firstErr error
	for id, f := range w.files {
		if err := f.close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(w.files, id)
	}
	return firstErr
}
