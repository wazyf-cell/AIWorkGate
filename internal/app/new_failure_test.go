package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A construction failure used to panic instead of returning the error.
//
// New() names its return values, and every early `return nil, err` assigns nil
// to `a` before the deferred cleanup loop runs. The cleanup closures reached
// through `a` (`a.notify`, `a.dispatch`), so a nil dereference replaced the real
// error with a crash — the user saw a stack trace instead of "another instance is
// running". The defer now runs each cleanup behind a recover boundary, and the
// two closures that owned state capture it locally.
//
// This test pins the observable contract: a failing NewWithOptions returns an
// error and does not panic.
func TestNewWithOptionsFailureDoesNotPanic(t *testing.T) {
	// A data directory whose parent is a file can never be created, so
	// config.EnsureDir fails and NewWithOptions returns before serving.
	root := t.TempDir()
	blocker := filepath.Join(root, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	// "blocker/child" cannot exist because "blocker" is a regular file.
	dataDir := filepath.Join(blocker, "child")

	a, err := NewWithOptions(NewOptions{DataDir: dataDir})
	if err == nil {
		if a != nil {
			_ = a.Shutdown()
		}
		t.Fatal("NewWithOptions returned no error for an unusable data directory")
	}
	if a != nil {
		t.Fatalf("NewWithOptions returned a non-nil App alongside an error: %+v", a)
	}
}

// The same contract for the single-instance path, which is the case users hit in
// practice (double-clicking the tray app twice). It must surface
// ErrAlreadyRunning, not a crash. Skipped when the machine already has a host
// running, because then the first NewWithOptions would lose the lock too.
func TestNewWithOptionsSecondInstanceReturnsAlreadyRunning(t *testing.T) {
	dataDir := t.TempDir()

	first, err := NewWithOptions(NewOptions{DataDir: dataDir})
	if err != nil {
		if errors.Is(err, ErrAlreadyRunning) {
			t.Skip("another AIWorkGate instance is already running on this machine")
		}
		t.Skipf("cannot construct the first host in this environment: %v", err)
	}
	defer func() {
		if first != nil {
			_ = first.Shutdown()
		}
	}()

	second, err := NewWithOptions(NewOptions{DataDir: dataDir})
	if err == nil {
		if second != nil {
			_ = second.Shutdown()
		}
		t.Fatal("the second host acquired the instance lock")
	}
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second NewWithOptions error = %v, want ErrAlreadyRunning", err)
	}
	if second != nil {
		t.Fatalf("second NewWithOptions returned a non-nil App: %+v", second)
	}
}

// A data directory that cannot be created must be reported, never silently
// treated as "no configuration yet".
func TestNewWithOptionsRejectsUncreatableDataDir(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	_, err := NewWithOptions(NewOptions{DataDir: filepath.Join(blocker, "nested")})
	if err == nil {
		t.Fatal("expected an error for a data directory that cannot be created")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "create") {
		t.Logf("error text = %q (the EnsureDir failure should name the directory)", err)
	}
}
