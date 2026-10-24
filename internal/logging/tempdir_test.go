package logging

import (
	"os"
	"testing"
	"time"
)

// testTempDir is t.TempDir with a retrying cleanup: on Windows an open SQLite
// handle or a just-closed log file can leave the directory delete-pending for a
// few hundred milliseconds, which t.TempDir reports as a test failure.
func testTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "aiworkgate-logging-test-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() {
		for i := 0; i < 50; i++ {
			if err := os.RemoveAll(dir); err == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		_ = os.RemoveAll(dir)
	})
	return dir
}
