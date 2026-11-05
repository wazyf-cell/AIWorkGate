package opchost

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeAllocator stands in for the supervisor's allocatePort and the secret
// store's EnsurePluginToken, so the standalone path is tested without touching a
// real listener or credential store.
type fakeAllocator struct {
	port     int
	token    string
	portErr  error
	tokenErr error

	portCalls  int
	tokenCalls int
	lastID     string
}

func (f *fakeAllocator) AllocatePort() (int, error) {
	f.portCalls++
	return f.port, f.portErr
}

func (f *fakeAllocator) EnsureToken(pluginID string) (string, error) {
	f.tokenCalls++
	f.lastID = pluginID
	return f.token, f.tokenErr
}

func TestStandaloneCommand(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "plugin-data")
	alloc := &fakeAllocator{port: 18801, token: "standalone-test-token"}

	entry, args, err := StandaloneCommand(alloc, "trae-bridge", dir, dataDir)
	if err != nil {
		t.Fatalf("StandaloneCommand: %v", err)
	}
	if entry != DefaultBridgeExeName {
		t.Fatalf("entry = %q, want %q", entry, DefaultBridgeExeName)
	}
	want := []string{
		"--plugin-dir", dir,
		"--port", "18801",
		"--token", "standalone-test-token",
		"--data-dir", dataDir,
	}
	if !slices.Equal(args, want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
	if alloc.portCalls != 1 || alloc.tokenCalls != 1 {
		t.Fatalf("allocator calls = port:%d token:%d, want 1 and 1", alloc.portCalls, alloc.tokenCalls)
	}
	if alloc.lastID != "trae-bridge" {
		t.Fatalf("token asked for plugin %q, want trae-bridge", alloc.lastID)
	}
}

func TestStandaloneCommandErrors(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "plugin-data")
	cases := []struct {
		name    string
		alloc   Allocator
		id      string
		dir     string
		dataDir string
		wantErr string
	}{
		{"nil allocator", nil, "trae-bridge", dir, dataDir, "allocator is required"},
		{"empty id", &fakeAllocator{port: 18801, token: "t"}, "  ", dir, dataDir, "plugin id"},
		{"empty plugin dir", &fakeAllocator{port: 18801, token: "t"}, "trae-bridge", " ", dataDir, "plugin dir"},
		{"empty data dir", &fakeAllocator{port: 18801, token: "t"}, "trae-bridge", dir, "", "data dir"},
		{"port allocation fails", &fakeAllocator{portErr: errors.New("no ports")}, "trae-bridge", dir, dataDir, "allocate port"},
		{"port out of range", &fakeAllocator{port: 0, token: "t"}, "trae-bridge", dir, dataDir, "out of range"},
		{"token fails", &fakeAllocator{port: 18801, tokenErr: errors.New("no store")}, "trae-bridge", dir, dataDir, "ensure token"},
		{"empty token", &fakeAllocator{port: 18801, token: "  "}, "trae-bridge", dir, dataDir, "empty token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := StandaloneCommand(tc.alloc, tc.id, tc.dir, tc.dataDir); err == nil {
				t.Fatalf("StandaloneCommand(%q) = nil error, want one containing %q", tc.name, tc.wantErr)
			} else if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}
