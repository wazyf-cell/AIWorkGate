package logging

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Level values written into the host log.
const (
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// Logger writes the host JSONL log: logs\host-YYYY-MM-DD.jsonl with the
// fields ts (RFC3339), level, module, msg plus arbitrary extra fields.
type Logger struct {
	mu   sync.Mutex
	file *dayFile
	now  func() time.Time
}

// NewLogger prepares <dataDir>\logs and returns a logger. The file is opened
// lazily on the first write.
func NewLogger(dataDir string, list ...Option) (*Logger, error) {
	o := resolveOptions(list)
	dir := filepath.Join(dataDir, logsDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("logging: create %s: %w", dir, err)
	}
	return &Logger{
		file: &dayFile{dir: dir, prefix: "host-", ext: ".jsonl", maxSize: maxLogBytes, generations: o.generations},
		now:  time.Now,
	}, nil
}

// Info logs an informational entry.
func (l *Logger) Info(module, msg string, fields map[string]any) {
	_ = l.Log(LevelInfo, module, msg, fields)
}

// Warn logs a warning entry.
func (l *Logger) Warn(module, msg string, fields map[string]any) {
	_ = l.Log(LevelWarn, module, msg, fields)
}

// Error logs an error entry.
func (l *Logger) Error(module, msg string, fields map[string]any) {
	_ = l.Log(LevelError, module, msg, fields)
}

// Log writes one JSON line. Reserved keys (ts, level, module, msg) always win
// over same-named fields.
func (l *Logger) Log(level, module, msg string, fields map[string]any) error {
	if l == nil {
		return nil
	}
	entry := make(map[string]any, len(fields)+4)
	for k, v := range fields {
		entry[k] = v
	}
	now := l.now()
	entry["ts"] = now.Format(time.RFC3339)
	entry["level"] = level
	entry["module"] = module
	entry["msg"] = msg

	line, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("logging: marshal host log: %w", err)
	}
	line = append(line, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	return l.file.write(now, line)
}

// Close releases the current host log file.
func (l *Logger) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.file.close()
}
