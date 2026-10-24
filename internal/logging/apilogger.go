package logging

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"aiworkgate/internal/model"
	"aiworkgate/internal/store"
)

// APILogger persists request log rows to SQLite and mirrors them into
// logs\api-YYYY-MM-DD.jsonl. Bodies are dropped unless debug is on; when on
// they are truncated (4KB request / 8KB response) and redacted.
type APILogger struct {
	st    *store.Store
	mu    sync.Mutex
	file  *dayFile
	now   func() time.Time
	debug atomic.Bool
}

// NewAPILogger prepares the API log sink. A nil store keeps file-only logging
// (used by tests); the web UI always passes a real store.
func NewAPILogger(st *store.Store, dataDir string, debug bool, list ...Option) (*APILogger, error) {
	o := resolveOptions(list)
	dir := filepath.Join(dataDir, logsDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("logging: create %s: %w", dir, err)
	}
	a := &APILogger{
		st:   st,
		file: &dayFile{dir: dir, prefix: "api-", ext: ".jsonl", maxSize: maxLogBytes, generations: o.generations},
		now:  time.Now,
	}
	a.debug.Store(debug)
	return a, nil
}

// SetDebug toggles body capture at runtime (config reload).
func (a *APILogger) SetDebug(v bool) {
	if a != nil {
		a.debug.Store(v)
	}
}

// Debug reports whether bodies are being captured.
func (a *APILogger) Debug() bool {
	return a != nil && a.debug.Load()
}

// Write stores one request log. The SQLite row and the JSONL line are both
// attempted; their errors are joined so one failing sink does not hide the
// other.
func (a *APILogger) Write(rec model.APIRequestLog) error {
	if a == nil {
		return nil
	}
	now := a.now()
	if rec.TS == "" {
		rec.TS = now.Format(time.RFC3339)
	}
	if a.debug.Load() {
		rec.BodyReq = RedactBody(Truncate(rec.BodyReq, BodyReqLimit))
		rec.BodyResp = RedactBody(Truncate(rec.BodyResp, BodyRespLimit))
	} else {
		// D97: without Debug only a small redacted preview is retained.
		rec.BodyReq = RedactBody(Truncate(rec.BodyReq, BodyPreviewLimit))
		rec.BodyResp = RedactBody(Truncate(rec.BodyResp, BodyPreviewLimit))
	}

	var dbErr error
	if a.st != nil {
		dbErr = a.st.InsertAPIRequestLog(rec)
	}

	line, marshalErr := json.Marshal(rec)
	if marshalErr != nil {
		return errors.Join(dbErr, fmt.Errorf("logging: marshal api log: %w", marshalErr))
	}
	line = append(line, '\n')

	a.mu.Lock()
	fileErr := a.file.write(now, line)
	a.mu.Unlock()

	return errors.Join(dbErr, fileErr)
}

// Close releases the current API log file.
func (a *APILogger) Close() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.file.close()
}
