// Package logging writes the host's human-readable logs: a JSONL host log,
// a combined SQLite + JSONL API request log, and per-plugin stdout/stderr
// capture. Rotation is daily plus a 10MB cap per file, with the number of
// rotated generations configurable per writer.
package logging

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	dateLayout = "2006-01-02"

	// maxLogBytes is the per-file cap before the current file is moved aside
	// to "<name>.1" and the older generations shift up by one.
	maxLogBytes = 10 << 20

	// defaultGenerations is how many rotated files a daily log keeps (.1 ... .N),
	// so a busy stream retains about (N+1)*10MB per day: 40MB by default.
	defaultGenerations = 3

	// tailReadBytes is how much of the end of a plugin log Tail reads.
	tailReadBytes = 256 << 10

	logsDirName    = "logs"
	pluginsDirName = "plugins"
)

// ErrInvalidPluginID is returned when a plugin id cannot be used in a file
// name (path traversal attempts, empty ids).
var ErrInvalidPluginID = errors.New("logging: invalid plugin id")

// generationSuffix matches the ".N" rotation suffix dayFile adds.
var generationSuffix = regexp.MustCompile(`\.\d+$`)

// baseLogName strips a trailing rotation generation from a log file name, so
// callers can recover the day and stream a rotated file belongs to.
func baseLogName(name string) string { return generationSuffix.ReplaceAllString(name, "") }

// Option configures a log writer.
type Option func(*options)

type options struct{ generations int }

// WithGenerations sets how many rotated files each daily log keeps (.1 ... .N).
// Values below one fall back to defaultGenerations.
func WithGenerations(n int) Option { return func(o *options) { o.generations = n } }

// resolveOptions applies the package defaults to the caller's options.
func resolveOptions(list []Option) options {
	o := options{generations: defaultGenerations}
	for _, fn := range list {
		if fn != nil {
			fn(&o)
		}
	}
	if o.generations < 1 {
		o.generations = defaultGenerations
	}
	return o
}

// dayFile appends to <dir>\<prefix>YYYY-MM-DD<ext>, rolling to a new file when
// the local date changes and rotating to "<name>.1"..<"<name>.N at maxSize.
type dayFile struct {
	dir         string
	prefix      string
	ext         string
	maxSize     int64
	generations int

	file *os.File
	name string
	size int64
}

// maxGenerations resolves the generation cap, keeping zero-value dayFile
// literals on the package default.
func (d *dayFile) maxGenerations() int {
	if d.generations > 0 {
		return d.generations
	}
	return defaultGenerations
}

func (d *dayFile) write(now time.Time, b []byte) error {
	name := filepath.Join(d.dir, d.prefix+now.Format(dateLayout)+d.ext)
	if d.file == nil || d.name != name {
		if err := d.close(); err != nil {
			return err
		}
		f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return fmt.Errorf("logging: open %s: %w", name, err)
		}
		st, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return fmt.Errorf("logging: stat %s: %w", name, err)
		}
		d.file, d.name, d.size = f, name, st.Size()
	}
	if d.maxSize > 0 && d.size >= d.maxSize {
		if err := d.rotate(); err != nil {
			return err
		}
	}
	n, err := d.file.Write(b)
	d.size += int64(n)
	if err != nil {
		return fmt.Errorf("logging: write %s: %w", d.name, err)
	}
	return nil
}

// rotate moves the live file to <name>.1, shifting the existing generations up
// by one and deleting whatever falls off the end of the chain.
func (d *dayFile) rotate() error {
	if d.file == nil {
		return nil
	}
	name := d.name
	if err := d.file.Close(); err != nil {
		d.file = nil
		return fmt.Errorf("logging: close %s: %w", name, err)
	}
	d.file = nil

	generations := d.maxGenerations()
	if err := os.Remove(genName(name, generations)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("logging: remove %s: %w", genName(name, generations), err)
	}
	// Highest first, so every rename lands on a slot that is already free.
	for i := generations - 1; i >= 1; i-- {
		src := genName(name, i)
		if err := os.Rename(src, genName(name, i+1)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("logging: shift %s: %w", src, err)
		}
	}
	if err := os.Rename(name, genName(name, 1)); err != nil {
		return fmt.Errorf("logging: rotate %s: %w", name, err)
	}
	f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("logging: reopen %s: %w", name, err)
	}
	d.file, d.size = f, 0
	return nil
}

// genName returns the nth rotated generation of base.
func genName(base string, n int) string { return base + "." + strconv.Itoa(n) }

func (d *dayFile) close() error {
	if d.file == nil {
		return nil
	}
	name := d.name
	err := d.file.Close()
	d.file = nil
	d.size = 0
	if err != nil {
		return fmt.Errorf("logging: close %s: %w", name, err)
	}
	return nil
}

// tailLines returns up to n trailing non-empty-terminated lines of path,
// reading at most tailReadBytes from the end. A missing file yields no lines
// and no error.
func tailLines(path string, n int) ([]string, error) {
	if n <= 0 {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("logging: tail %s: %w", path, err)
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("logging: tail %s: %w", path, err)
	}
	size := st.Size()
	start := size - tailReadBytes
	if start < 0 {
		start = 0
	}
	buf := make([]byte, size-start)
	if len(buf) > 0 {
		if _, err := f.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("logging: tail %s: read: %w", path, err)
		}
	}
	lines := strings.Split(string(buf), "\n")
	if start > 0 && len(lines) > 0 {
		// The first line is very likely cut in half; drop it.
		lines = lines[1:]
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}
