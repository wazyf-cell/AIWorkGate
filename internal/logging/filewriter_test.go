package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// markerLine is 44 bytes, so a small maxSize rotates on a predictable count.
func markerLine(i int) string {
	return fmt.Sprintf("%02d-%s\n", i, strings.Repeat("p", 40))
}

// dayName renders the live file a dayFile with these fields writes to.
func dayName(dir, prefix, ext string, now time.Time) string {
	return filepath.Join(dir, prefix+now.Format(dateLayout)+ext)
}

// markers returns the two-character marker of every line in a log file.
func markers(t *testing.T, path string) string {
	t.Helper()
	var got []string
	for _, line := range readLines(t, path) {
		if len(line) >= 2 {
			got = append(got, line[:2])
		}
	}
	return strings.Join(got, ",")
}

// writeMarkers appends count marker lines to a dayFile.
func writeMarkers(t *testing.T, d *dayFile, now time.Time, count int) {
	t.Helper()
	for i := 0; i < count; i++ {
		if err := d.write(now, []byte(markerLine(i))); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if err := d.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestDayFileRotateShiftsAndPrunes(t *testing.T) {
	dir := testTempDir(t)
	now := fixedNow()
	d := &dayFile{dir: dir, prefix: "x-", ext: ".log", maxSize: 100, generations: 2}
	// 44-byte lines: the file crosses the 100 byte cap every third write.
	writeMarkers(t, d, now, 12)

	base := dayName(dir, "x-", ".log", now)
	for _, want := range []struct {
		path string
		want string
	}{
		{base, "09,10,11"},
		{base + ".1", "06,07,08"},
		{base + ".2", "03,04,05"},
	} {
		if got := markersOf(t, want.path); got != want.want {
			t.Errorf("%s holds %q, want %q", filepath.Base(want.path), got, want.want)
		}
	}
	if _, err := os.Stat(base + ".3"); !os.IsNotExist(err) {
		t.Errorf("generation .3 exists although only 2 are kept (err=%v)", err)
	}
	// The oldest chunk fell off the chain instead of being rewritten in place.
	for _, path := range []string{base, base + ".1", base + ".2"} {
		if strings.Contains(markersOf(t, path), "00") {
			t.Errorf("%s still holds the pruned markers", filepath.Base(path))
		}
	}
}

func TestDayFileZeroValueKeepsDefaultGenerations(t *testing.T) {
	dir := testTempDir(t)
	now := fixedNow()
	d := &dayFile{dir: dir, prefix: "host-", ext: ".jsonl", maxSize: 100}
	if got := d.maxGenerations(); got != defaultGenerations {
		t.Fatalf("maxGenerations = %d, want the default %d", got, defaultGenerations)
	}
	writeMarkers(t, d, now, 15)

	base := dayName(dir, "host-", ".jsonl", now)
	for i := 1; i <= defaultGenerations; i++ {
		if _, err := os.Stat(genName(base, i)); err != nil {
			t.Errorf("generation .%d missing: %v", i, err)
		}
	}
	if _, err := os.Stat(genName(base, defaultGenerations+1)); !os.IsNotExist(err) {
		t.Errorf("generation .%d exists, want it pruned (err=%v)", defaultGenerations+1, err)
	}
}

func TestWritersDefaultToThreeGenerations(t *testing.T) {
	if defaultGenerations != 3 {
		t.Fatalf("defaultGenerations = %d, want 3 so a stream keeps ~40MB per day", defaultGenerations)
	}
	dir := testTempDir(t)

	l, err := NewLogger(dir)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer l.Close()
	if got := l.file.maxGenerations(); got != 3 {
		t.Errorf("host log generations = %d, want 3", got)
	}

	w, err := NewPluginLogWriter(dir)
	if err != nil {
		t.Fatalf("NewPluginLogWriter: %v", err)
	}
	defer w.Close()
	if got := w.generations; got != 3 {
		t.Errorf("plugin log generations = %d, want 3", got)
	}

	a, err := NewAPILogger(nil, dir, false)
	if err != nil {
		t.Fatalf("NewAPILogger: %v", err)
	}
	defer a.Close()
	if got := a.file.maxGenerations(); got != 3 {
		t.Errorf("api log generations = %d, want 3", got)
	}
}

func TestWritersAcceptGenerationOption(t *testing.T) {
	dir := testTempDir(t)

	l, err := NewLogger(dir, WithGenerations(5))
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer l.Close()
	if got := l.file.maxGenerations(); got != 5 {
		t.Errorf("host log generations = %d, want 5", got)
	}

	w, err := NewPluginLogWriter(dir, WithGenerations(1))
	if err != nil {
		t.Fatalf("NewPluginLogWriter: %v", err)
	}
	defer w.Close()
	if err := w.Write("mock", "one"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	w.mu.Lock()
	got := w.files["mock"].maxGenerations()
	w.mu.Unlock()
	if got != 1 {
		t.Errorf("plugin dayFile generations = %d, want 1", got)
	}

	// Nonsensical values fall back to the default instead of disabling rotation.
	for _, invalid := range []int{0, -2} {
		bad, err := NewLogger(dir, WithGenerations(invalid))
		if err != nil {
			t.Fatalf("NewLogger(%d): %v", invalid, err)
		}
		if got := bad.file.maxGenerations(); got != defaultGenerations {
			t.Errorf("NewLogger(WithGenerations(%d)) = %d, want %d", invalid, got, defaultGenerations)
		}
		bad.Close()
	}
}

// TestRotationLeavesOtherDaysAloneAndCleanupPrunesGenerations covers a host log
// dir holding days of rotated files: rotation must only touch today's chain, and
// retention must still delete every generation of an expired day.
func TestRotationLeavesOtherDaysAloneAndCleanupPrunesGenerations(t *testing.T) {
	dir := testTempDir(t)
	logs := filepath.Join(dir, logsDirName)
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	now := fixedNow()

	var older []string
	for day := 1; day <= 5; day++ {
		name := dayName(logs, "host-", ".jsonl", now.AddDate(0, 0, -day))
		for _, path := range []string{name, name + ".1", name + ".2", name + ".3"} {
			mustWrite(t, path)
			older = append(older, path)
		}
	}
	// One expired api log generation each, to cover the api- branch.
	apiName := dayName(logs, "api-", ".jsonl", now.AddDate(0, 0, -20))
	mustWrite(t, apiName)
	mustWrite(t, apiName+".2")

	d := &dayFile{dir: logs, prefix: "host-", ext: ".jsonl", maxSize: 100, generations: 3}
	writeMarkers(t, d, now, 12)

	for _, path := range older {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("rotation touched %s: %v", filepath.Base(path), err)
		}
	}
	today := dayName(logs, "host-", ".jsonl", now)
	if _, err := os.Stat(genName(today, 4)); !os.IsNotExist(err) {
		t.Fatalf("today's chain grew past its cap (err=%v)", err)
	}

	// hostDays=2 keeps today, yesterday and the day before; apiDays=2 expires the
	// api log, and every ".N" generation must age with its base file.
	if err := CleanupFiles(dir, 2, 2, now); err != nil {
		t.Fatalf("CleanupFiles: %v", err)
	}
	for day := 3; day <= 5; day++ {
		name := dayName(logs, "host-", ".jsonl", now.AddDate(0, 0, -day))
		for _, path := range []string{name, name + ".1", name + ".2", name + ".3"} {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("%s survived retention (err=%v)", filepath.Base(path), err)
			}
		}
	}
	for day := 1; day <= 2; day++ {
		name := dayName(logs, "host-", ".jsonl", now.AddDate(0, 0, -day))
		for _, path := range []string{name, name + ".1", name + ".2", name + ".3"} {
			if _, err := os.Stat(path); err != nil {
				t.Errorf("%s was deleted inside the window: %v", filepath.Base(path), err)
			}
		}
	}
	for _, path := range []string{apiName, apiName + ".2"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("expired api file %s survived (err=%v)", filepath.Base(path), err)
		}
	}
}

func TestBaseLogNameStripsAnyGeneration(t *testing.T) {
	for name, want := range map[string]string{
		"host-2026-10-21.jsonl":    "host-2026-10-21.jsonl",
		"host-2026-10-21.jsonl.1":  "host-2026-10-21.jsonl",
		"host-2026-10-21.jsonl.12": "host-2026-10-21.jsonl",
		"mock-2026-10-21.log.3":    "mock-2026-10-21.log",
		"notes.txt":                "notes.txt",
		"aiworkgate.db.bak-v2-v3":  "aiworkgate.db.bak-v2-v3",
	} {
		if got := baseLogName(name); got != want {
			t.Errorf("baseLogName(%q) = %q, want %q", name, got, want)
		}
	}
}

// markersOf reads a file's markers, tolerating a missing file as empty.
func markersOf(t *testing.T, path string) string {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("read %s: %v", filepath.Base(path), err)
	}
	return markers(t, path)
}
