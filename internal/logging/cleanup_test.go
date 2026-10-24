package logging

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mustWrite(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestCleanupFiles(t *testing.T) {
	dir := testTempDir(t)
	logs := filepath.Join(dir, "logs")
	plugins := filepath.Join(logs, "plugins")
	now := fixedNow()

	oldHost := filepath.Join(logs, "host-2026-09-01.jsonl")
	oldHostRot := filepath.Join(logs, "host-2026-09-01.jsonl.1")
	newHost := filepath.Join(logs, "host-2026-10-20.jsonl")
	oldAPI := filepath.Join(logs, "api-2026-10-10.jsonl")
	newAPI := filepath.Join(logs, "api-2026-10-19.jsonl")
	oldPlugin := filepath.Join(plugins, "mock-2026-09-01.log")
	oldPluginRot := filepath.Join(plugins, "mock-2026-09-01.log.1")
	newPlugin := filepath.Join(plugins, "mock-2026-10-21.log")
	unrelated := filepath.Join(logs, "notes.txt")

	for _, p := range []string{oldHost, oldHostRot, newHost, oldAPI, newAPI, oldPlugin, oldPluginRot, newPlugin, unrelated} {
		mustWrite(t, p)
	}

	// host retention 30 days, api retention 7 days.
	if err := CleanupFiles(dir, 30, 7, now); err != nil {
		t.Fatalf("CleanupFiles: %v", err)
	}

	gone := []string{oldHost, oldHostRot, oldPlugin, oldPluginRot}
	for _, p := range gone {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s should have been deleted (err=%v)", p, err)
		}
	}
	kept := []string{newHost, newAPI, newPlugin, unrelated}
	for _, p := range kept {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s should have been kept: %v", p, err)
		}
	}
	// api-2026-10-10 is 11 days old with a 7 day window.
	if _, err := os.Stat(oldAPI); !os.IsNotExist(err) {
		t.Fatalf("old api log should have been deleted (err=%v)", err)
	}
}

func TestCleanupFilesRespectsHostWindow(t *testing.T) {
	dir := testTempDir(t)
	logs := filepath.Join(dir, "logs")
	now := fixedNow()

	twentyDays := filepath.Join(logs, "host-2026-10-01.jsonl")
	fortyDays := filepath.Join(logs, "host-2026-09-11.jsonl")
	apiTwenty := filepath.Join(logs, "api-2026-10-01.jsonl")
	for _, p := range []string{twentyDays, fortyDays, apiTwenty} {
		mustWrite(t, p)
	}

	if err := CleanupFiles(dir, 30, 90, now); err != nil {
		t.Fatalf("CleanupFiles: %v", err)
	}
	if _, err := os.Stat(twentyDays); err != nil {
		t.Fatalf("20-day host log should be kept: %v", err)
	}
	if _, err := os.Stat(fortyDays); !os.IsNotExist(err) {
		t.Fatalf("40-day host log should be deleted")
	}
	if _, err := os.Stat(apiTwenty); err != nil {
		t.Fatalf("20-day api log should be kept with a 90 day window: %v", err)
	}

	// Disabled retention keeps everything.
	if err := CleanupFiles(dir, 0, 0, now.AddDate(1, 0, 0)); err != nil {
		t.Fatalf("CleanupFiles disabled: %v", err)
	}
	if _, err := os.Stat(twentyDays); err != nil {
		t.Fatalf("host log deleted with retention disabled")
	}
}

func TestCleanupFilesMissingDirIsFine(t *testing.T) {
	dir := testTempDir(t)
	if err := CleanupFiles(dir, 30, 30, time.Now()); err != nil {
		t.Fatalf("CleanupFiles on empty data dir: %v", err)
	}
}
