package logging

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CleanupFiles deletes dated log files older than the retention windows:
// host-*.jsonl and logs\plugins\*.log use hostDays, api-*.jsonl uses apiDays.
// Rotation leftovers ("<name>.1" .. "<name>.N") age together with their base
// file. A non-positive retention disables that class of deletion.
func CleanupFiles(dataDir string, hostDays, apiDays int, now time.Time) error {
	logsDir := filepath.Join(dataDir, logsDirName)

	var errs []error
	errs = append(errs, cleanupHostAndAPILogs(logsDir, hostDays, apiDays, now))
	errs = append(errs, cleanupPluginLogs(filepath.Join(logsDir, pluginsDirName), hostDays, now))
	return errors.Join(errs...)
}

func cleanupHostAndAPILogs(dir string, hostDays, apiDays int, now time.Time) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("logging: cleanup %s: %w", dir, err)
	}
	var errs []error
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		base := baseLogName(name)
		var days int
		switch {
		case strings.HasPrefix(base, "host-") && strings.HasSuffix(base, ".jsonl"):
			days = hostDays
		case strings.HasPrefix(base, "api-") && strings.HasSuffix(base, ".jsonl"):
			days = apiDays
		default:
			continue
		}
		if err := removeIfExpired(filepath.Join(dir, name), name, days, now); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func cleanupPluginLogs(dir string, days int, now time.Time) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("logging: cleanup %s: %w", dir, err)
	}
	var errs []error
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(baseLogName(e.Name()), ".log") {
			continue
		}
		if err := removeIfExpired(filepath.Join(dir, e.Name()), e.Name(), days, now); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// removeIfExpired deletes path when the date embedded in its name is older
// than keepDays relative to now's local date.
func removeIfExpired(path, name string, keepDays int, now time.Time) error {
	if keepDays <= 0 {
		return nil
	}
	day, ok := fileDate(name, now.Location())
	if !ok {
		return nil
	}
	loc := now.Location()
	cutoff := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, -keepDays)
	if !day.Before(cutoff) {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("logging: remove %s: %w", path, err)
	}
	return nil
}

// fileDate extracts the trailing YYYY-MM-DD of a log file name, ignoring a
// rotation generation suffix. Plugin ids may contain dashes, so the date is
// taken from the end.
func fileDate(name string, loc *time.Location) (time.Time, bool) {
	base := baseLogName(name)
	for _, ext := range []string{".jsonl", ".log"} {
		base = strings.TrimSuffix(base, ext)
	}
	if len(base) < len(dateLayout) {
		return time.Time{}, false
	}
	stamp := base[len(base)-len(dateLayout):]
	t, err := time.ParseInLocation(dateLayout, stamp, loc)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
