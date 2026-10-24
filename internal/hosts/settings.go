package hosts

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // read-only access to A's SQLite data dir
)

const (
	// DefaultGatewayPort is A's factory default for the built-in API gateway.
	// Callers must fall back to it whenever discovery returns an error
	// (contract §8.3).
	DefaultGatewayPort = 7864

	// SettingsKey is the kv row holding A's gateway settings JSON
	// ({"port":7864,"host":"127.0.0.1","default_model":"..."}).
	SettingsKey = "api_gateway_settings"

	// appDirName is A's data directory name under %APPDATA%.
	appDirName = "AIWorkAssistant"

	// dbFileName is A's SQLite file, relative to the data directory.
	dbFileName = "data/aiwork.sqlite"

	// readOnlyQuery opens the file strictly read-only and tolerates short
	// write locks held by a concurrently running A GUI.
	readOnlyQuery = "mode=ro&_pragma=busy_timeout(3000)"
)

// DiscoverErrKind classifies why A's gateway settings could not be read.
type DiscoverErrKind string

const (
	// DiscoverErrDataDir means the A data directory could not be resolved
	// (%APPDATA% unset or empty).
	DiscoverErrDataDir DiscoverErrKind = "data_dir"
	// DiscoverErrDBMissing means data\aiwork.sqlite does not exist.
	DiscoverErrDBMissing DiscoverErrKind = "db_missing"
	// DiscoverErrDBLocked means the database exists but is locked/busy.
	DiscoverErrDBLocked DiscoverErrKind = "db_locked"
	// DiscoverErrDBOpen means the database exists but cannot be read
	// (corrupt, permissions, schema mismatch, ...).
	DiscoverErrDBOpen DiscoverErrKind = "db_open"
	// DiscoverErrKeyMissing means the kv table or the settings row is absent.
	DiscoverErrKeyMissing DiscoverErrKind = "key_missing"
	// DiscoverErrParse means the stored JSON is unusable (bad JSON, port out
	// of range, empty default model).
	DiscoverErrParse DiscoverErrKind = "parse"
)

// DiscoverError is the typed error returned by ADataDir, DiscoverPort and
// DiscoverDefaultModel. Callers match it with errors.As and then decide to
// fall back (port -> DefaultGatewayPort).
type DiscoverError struct {
	Kind DiscoverErrKind
	Path string // file or directory involved, empty when unknown
	Err  error
}

func (e *DiscoverError) Error() string {
	var b strings.Builder
	b.WriteString("hosts: ")
	b.WriteString(string(e.Kind))
	if e.Path != "" {
		b.WriteString(" (")
		b.WriteString(e.Path)
		b.WriteString(")")
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

func (e *DiscoverError) Unwrap() error { return e.Err }

func newDiscoverError(kind DiscoverErrKind, path string, err error) *DiscoverError {
	return &DiscoverError{Kind: kind, Path: path, Err: err}
}

// DatabasePath returns A's SQLite path for a data directory.
func DatabasePath(dataDir string) string {
	return filepath.Join(dataDir, filepath.FromSlash(dbFileName))
}

// GatewaySettings is the subset of A's api_gateway_settings JSON the host
// needs. Unknown fields are ignored on purpose (A may add more).
type GatewaySettings struct {
	Port         int    `json:"port"`
	Host         string `json:"host,omitempty"`
	DefaultModel string `json:"default_model,omitempty"`
}

// DiscoverPort reads A's configured gateway port from its SQLite kv table.
// On any failure the caller should fall back to DefaultGatewayPort; the
// returned error is always a *DiscoverError.
func DiscoverPort(dataDir string) (int, error) {
	s, err := readGatewaySettings(dataDir)
	if err != nil {
		return 0, err
	}
	if s.Port < 1 || s.Port > 65535 {
		return 0, newDiscoverError(DiscoverErrParse, DatabasePath(dataDir),
			fmt.Errorf("port %d out of range 1-65535", s.Port))
	}
	return s.Port, nil
}

// DiscoverDefaultModel reads A's configured default model name.
func DiscoverDefaultModel(dataDir string) (string, error) {
	s, err := readGatewaySettings(dataDir)
	if err != nil {
		return "", err
	}
	model := strings.TrimSpace(s.DefaultModel)
	if model == "" {
		return "", newDiscoverError(DiscoverErrParse, DatabasePath(dataDir),
			errors.New("default_model is empty"))
	}
	return model, nil
}

func readGatewaySettings(dataDir string) (GatewaySettings, error) {
	var out GatewaySettings
	if strings.TrimSpace(dataDir) == "" {
		return out, newDiscoverError(DiscoverErrDataDir, "", errors.New("empty data dir"))
	}
	dbPath := DatabasePath(dataDir)
	info, err := os.Stat(dbPath)
	if err != nil {
		kind := DiscoverErrDBOpen
		if errors.Is(err, os.ErrNotExist) {
			kind = DiscoverErrDBMissing
		}
		return out, newDiscoverError(kind, dbPath, err)
	}
	if info.IsDir() {
		return out, newDiscoverError(DiscoverErrDBOpen, dbPath, errors.New("path is a directory"))
	}

	db, err := sql.Open("sqlite", readOnlyDSN(dbPath))
	if err != nil {
		return out, newDiscoverError(DiscoverErrDBOpen, dbPath, err)
	}
	defer db.Close()
	// One connection: a single short read, no pooling, no background work.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	var content string
	err = db.QueryRow(`SELECT content FROM kv WHERE key = ?`, SettingsKey).Scan(&content)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return out, newDiscoverError(DiscoverErrKeyMissing, dbPath,
			fmt.Errorf("kv row %q not found", SettingsKey))
	case err != nil:
		return out, newDiscoverError(classifyDBError(err), dbPath, err)
	}

	if err := json.Unmarshal([]byte(content), &out); err != nil {
		return GatewaySettings{}, newDiscoverError(DiscoverErrParse, dbPath, err)
	}
	return out, nil
}

// classifyDBError separates transient locks from structural failures.
func classifyDBError(err error) DiscoverErrKind {
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "locked") || strings.Contains(msg, "busy") {
		return DiscoverErrDBLocked
	}
	return DiscoverErrDBOpen
}

// readOnlyDSN builds a SQLite URI DSN: file:///C:/.../aiwork.sqlite?mode=ro...
// The driver enables SQLITE_OPEN_URI, so mode=ro is honoured and the file is
// never created or written.
func readOnlyDSN(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	if !strings.HasPrefix(u.Path, "/") {
		u.Path = "/" + u.Path
	}
	u.RawQuery = readOnlyQuery
	return u.String()
}
