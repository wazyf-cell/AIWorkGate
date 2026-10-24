package usage

import (
	"os"
	"testing"
	"time"
)

// testTempDir is t.TempDir with a retrying cleanup: on Windows a just-closed
// SQLite handle can keep the directory delete-pending briefly.
func testTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "aiworkgate-usage-test-*")
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
