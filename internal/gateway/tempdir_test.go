package gateway

import (
	"os"
	"testing"
	"time"
)

// testTempDir is t.TempDir with a retrying cleanup: a Windows SQLite handle can
// keep the directory delete-pending for a few hundred milliseconds after the
// test ends, which t.TempDir reports as a failure.
func testTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "aiworkgate-gateway-test-*")
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
