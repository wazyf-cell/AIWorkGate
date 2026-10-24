package hosts

import (
	"database/sql"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// newTempAData creates <tmp>\data\aiwork.sqlite with A's kv schema and stores
// settings as the api_gateway_settings row (empty settings = no row).
func newTempAData(t *testing.T, settings string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(DatabasePath(dir)), 0o755); err != nil {
		t.Fatalf("mkdir data: %v", err)
	}
	// file: URI + mode=rwc so a plain Windows path keeps working.
	dsn := "file:" + filepath.ToSlash(DatabasePath(dir)) + "?mode=rwc"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close sqlite: %v", err)
		}
	}()
	if _, err := db.Exec(`CREATE TABLE kv (
		key TEXT PRIMARY KEY,
		content TEXT,
		updated_at TEXT
	)`); err != nil {
		t.Fatalf("create kv: %v", err)
	}
	if settings != "" {
		if _, err := db.Exec(
			`INSERT INTO kv (key, content, updated_at) VALUES (?, ?, ?)`,
			SettingsKey, settings, "2026-10-20T00:00:00Z"); err != nil {
			t.Fatalf("insert settings: %v", err)
		}
	}
	return dir
}

func discoverKind(t *testing.T, err error) DiscoverErrKind {
	t.Helper()
	var de *DiscoverError
	if !errors.As(err, &de) {
		t.Fatalf("error %v is not *DiscoverError", err)
	}
	return de.Kind
}

func TestDiscoverPortAndDefaultModel(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		port    int
		model   string
	}{
		{
			name:    "defaults",
			payload: `{"port":7864,"host":"127.0.0.1","default_model":"qoder-gpt-5"}`,
			port:    7864,
			model:   "qoder-gpt-5",
		},
		{
			name:    "custom port with extra fields",
			payload: `{"port":7871,"default_model":"trae-claude-sonnet","updated_at":1760000000,"extra":{"x":1}}`,
			port:    7871,
			model:   "trae-claude-sonnet",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := newTempAData(t, tc.payload)
			port, err := DiscoverPort(dir)
			if err != nil {
				t.Fatalf("DiscoverPort: %v", err)
			}
			if port != tc.port {
				t.Fatalf("port = %d, want %d", port, tc.port)
			}
			model, err := DiscoverDefaultModel(dir)
			if err != nil {
				t.Fatalf("DiscoverDefaultModel: %v", err)
			}
			if model != tc.model {
				t.Fatalf("model = %q, want %q", model, tc.model)
			}
		})
	}
}

func TestDiscoverMissingDatabase(t *testing.T) {
	dir := t.TempDir() // no data\aiwork.sqlite at all
	port, err := DiscoverPort(dir)
	if err == nil {
		t.Fatalf("expected error, got port %d", port)
	}
	if kind := discoverKind(t, err); kind != DiscoverErrDBMissing {
		t.Fatalf("kind = %q, want %q (%v)", kind, DiscoverErrDBMissing, err)
	}
	if _, err := DiscoverDefaultModel(dir); err == nil {
		t.Fatal("expected error from DiscoverDefaultModel")
	}
}

func TestDiscoverMissingSettingsRow(t *testing.T) {
	dir := newTempAData(t, "") // table exists, row does not
	if _, err := DiscoverPort(dir); err == nil {
		t.Fatal("expected error")
	} else if kind := discoverKind(t, err); kind != DiscoverErrKeyMissing {
		t.Fatalf("kind = %q, want %q (%v)", kind, DiscoverErrKeyMissing, err)
	}
}

func TestDiscoverBadSettings(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{"invalid json", `{"port":7864,`},
		{"port out of range", `{"port":70000,"default_model":"m"}`},
		{"port missing", `{"default_model":"m"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := newTempAData(t, tc.payload)
			if _, err := DiscoverPort(dir); err == nil {
				t.Fatal("expected error")
			} else if kind := discoverKind(t, err); kind != DiscoverErrParse {
				t.Fatalf("kind = %q, want %q (%v)", kind, DiscoverErrParse, err)
			}
		})
	}

	dir := newTempAData(t, `{"port":7864}`)
	if _, err := DiscoverDefaultModel(dir); err == nil {
		t.Fatal("expected error for empty default_model")
	} else if kind := discoverKind(t, err); kind != DiscoverErrParse {
		t.Fatalf("kind = %q, want %q (%v)", kind, DiscoverErrParse, err)
	}
}

func TestADataDir(t *testing.T) {
	base := `C:\Users\tester\AppData\Roaming`
	t.Setenv("APPDATA", base)
	dir, err := ADataDir()
	if err != nil {
		t.Fatalf("ADataDir: %v", err)
	}
	want := filepath.Join(base, "AIWorkAssistant")
	if dir != want {
		t.Fatalf("ADataDir = %q, want %q", dir, want)
	}

	t.Setenv("APPDATA", "")
	if _, err := ADataDir(); err == nil {
		t.Fatal("expected error when APPDATA is empty")
	} else if kind := discoverKind(t, err); kind != DiscoverErrDataDir {
		t.Fatalf("kind = %q, want %q (%v)", kind, DiscoverErrDataDir, err)
	}
}

func TestPortOpen(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	openPort := ln.Addr().(*net.TCPAddr).Port
	if !PortOpen(openPort) {
		t.Fatalf("PortOpen(%d) = false, want true", openPort)
	}
	// A port nothing listens on: grab one, close it, then probe it.
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	closedPort := ln2.Addr().(*net.TCPAddr).Port
	if err := ln2.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for PortOpen(closedPort) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond) // TIME_WAIT / listener teardown
	}
	if PortOpen(closedPort) {
		t.Fatalf("PortOpen(%d) = true, want false", closedPort)
	}
	_ = ln.Close()

	if PortOpen(0) || PortOpen(-1) || PortOpen(70000) {
		t.Fatal("PortOpen must reject out-of-range ports")
	}
}
