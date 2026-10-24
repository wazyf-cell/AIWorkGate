package logging

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPluginLogWriteAndTail(t *testing.T) {
	dir := testTempDir(t)
	w, err := NewPluginLogWriter(dir)
	if err != nil {
		t.Fatalf("NewPluginLogWriter: %v", err)
	}
	defer w.Close()
	w.now = fixedNow

	for i, line := range []string{"line 1", "line 2", "line 3", "line 4"} {
		if err := w.Write("mock", line); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}

	path := filepath.Join(dir, "logs", "plugins", "mock-2026-10-21.log")
	if b, err := os.ReadFile(path); err != nil {
		t.Fatalf("read plugin log: %v", err)
	} else if !strings.HasSuffix(string(b), "line 4\n") {
		t.Fatalf("plugin log content = %q", string(b))
	}

	lines, err := w.Tail("mock", 2)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(lines) != 2 || lines[0] != "line 3" || lines[1] != "line 4" {
		t.Fatalf("tail = %v", lines)
	}

	all, err := w.Tail("mock", 10)
	if err != nil || len(all) != 4 {
		t.Fatalf("tail all = %v err=%v", all, err)
	}

	// Missing plugin log: no lines, no error.
	none, err := w.Tail("ghost", 5)
	if err != nil || len(none) != 0 {
		t.Fatalf("missing tail = %v err=%v", none, err)
	}
}

func TestPluginLogSanitizesAndRejectsTraversal(t *testing.T) {
	dir := testTempDir(t)
	w, err := NewPluginLogWriter(dir)
	if err != nil {
		t.Fatalf("NewPluginLogWriter: %v", err)
	}
	defer w.Close()
	w.now = fixedNow

	bad := []string{"", "..", "../../evil", `..\..\evil`, "a/b", `a\b`, "a:b", ".", "..", "..."}
	for _, id := range bad {
		if err := w.Write(id, "x"); !errors.Is(err, ErrInvalidPluginID) {
			t.Fatalf("Write(%q) err = %v, want ErrInvalidPluginID", id, err)
		}
		if _, err := w.Tail(id, 1); !errors.Is(err, ErrInvalidPluginID) {
			t.Fatalf("Tail(%q) err = %v, want ErrInvalidPluginID", id, err)
		}
	}

	// Nothing escaped the plugins directory.
	if _, err := os.Stat(filepath.Join(dir, "logs", "evil-2026-10-21.log")); err == nil {
		t.Fatalf("traversal wrote outside plugins dir")
	}

	if err := w.Write("Trae AI", "hello"); err != nil {
		t.Fatalf("Write sanitized: %v", err)
	}
	sanitized := filepath.Join(dir, "logs", "plugins", "trae_ai-2026-10-21.log")
	if _, err := os.Stat(sanitized); err != nil {
		t.Fatalf("sanitized file missing: %v", err)
	}
	lines, err := w.Tail("Trae AI", 1)
	if err != nil || len(lines) != 1 || lines[0] != "hello" {
		t.Fatalf("tail sanitized = %v err=%v", lines, err)
	}
}

func TestPluginLogRotationAndTailAcrossFiles(t *testing.T) {
	dir := testTempDir(t)
	w, err := NewPluginLogWriter(dir)
	if err != nil {
		t.Fatalf("NewPluginLogWriter: %v", err)
	}
	defer w.Close()
	w.now = fixedNow
	w.maxSize = 120

	for i := 0; i < 20; i++ {
		if err := w.Write("mock", "0123456789-0123456789"); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}

	base := filepath.Join(dir, "logs", "plugins", "mock-2026-10-21.log")
	if _, err := os.Stat(base + ".1"); err != nil {
		t.Fatalf("rotated plugin log missing: %v", err)
	}

	lines, err := w.Tail("mock", 3)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(lines) != 3 {
		t.Fatalf("tail len = %d, want 3", len(lines))
	}
	for _, l := range lines {
		if l != "0123456789-0123456789" {
			t.Fatalf("unexpected line %q", l)
		}
	}

	// Asking for more lines than fit in the current file reaches into .1.
	many, err := w.Tail("mock", 200)
	if err != nil {
		t.Fatalf("Tail many: %v", err)
	}
	if len(many) <= len(lines) || len(many) > 20 {
		t.Fatalf("tail across files returned %d lines", len(many))
	}
	if many[len(many)-1] != "0123456789-0123456789" {
		t.Fatalf("tail tail = %q", many[len(many)-1])
	}
}
