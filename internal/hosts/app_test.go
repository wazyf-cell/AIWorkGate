package hosts

import (
	"strings"
	"testing"
)

func TestStartAppRejectsInvalidPaths(t *testing.T) {
	if err := StartApp(""); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("StartApp(empty) = %v, want empty-path error", err)
	}
	if err := StartApp(`C:\definitely\missing\A.exe`); err == nil {
		t.Fatal("StartApp(missing) = nil, want stat error")
	}
	dir := t.TempDir()
	if err := StartApp(dir); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("StartApp(dir) = %v, want directory error", err)
	}
}
