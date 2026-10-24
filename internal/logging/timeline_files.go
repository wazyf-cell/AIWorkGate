package logging

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
)

// runtimeFile is one host JSONL or plugin log file that contributes events to
// the timeline. Rotated (.1) and current files are independent sources: each
// has its own byte cursor.
type runtimeFile struct {
	path     string
	day      string
	pluginID string
}

// runtimeFileState is one file's contribution to one page plus where the next
// page should resume.
type runtimeFileState struct {
	spec      runtimeFile
	items     []TimelineItem
	next      int64
	more      bool
	truncated bool
	err       error
}

// runtimeFiles lists the files covered by the filter. Missing files cost one
// failed open each; that is cheaper than stat-ing every path twice.
func (t *Timeline) runtimeFiles(f TimelineFilter) []runtimeFile {
	if t == nil || t.DataDir == "" {
		return nil
	}
	days := daysBetween(f.FromDay, f.ToDay)
	out := make([]runtimeFile, 0, len(days)*2)
	for _, day := range days {
		base := filepath.Join(t.DataDir, logsDirName, "host-"+day+".jsonl")
		for _, path := range []string{base + ".1", base} {
			out = append(out, runtimeFile{path: path, day: day})
		}
	}
	if t.Plugins == nil {
		return out
	}
	for _, id := range t.pluginIDs() {
		safe, err := SanitizePluginID(id)
		if err != nil {
			continue
		}
		for _, day := range days {
			base := filepath.Join(t.DataDir, logsDirName, pluginsDirName, safe+"-"+day+".log")
			for _, path := range []string{base + ".1", base} {
				out = append(out, runtimeFile{path: path, day: day, pluginID: id})
			}
		}
	}
	return out
}

// readRuntimeFile reads up to want lines ending at end (negative means "the
// current end of file"). The returned state's next offset is where a following
// page resumes.
func (t *Timeline) readRuntimeFile(spec runtimeFile, end int64, want int) runtimeFileState {
	state := runtimeFileState{spec: spec}
	if end < 0 {
		end = fileSize(spec.path)
	}
	lines, next, exhausted, truncated, err := readLinesBefore(spec.path, end, want, maxTimelineFileBytes)
	if err != nil {
		state.err = err
		return state
	}
	state.next = next
	state.more = !exhausted
	state.truncated = truncated
	state.items = make([]TimelineItem, 0, len(lines))
	for index, line := range lines {
		var (
			item TimelineItem
			ok   bool
		)
		if spec.pluginID != "" {
			item, ok = pluginItem(spec.pluginID, line.text, spec.day, index)
		} else {
			item, ok = hostItem(line.text, spec.day, index)
		}
		if !ok {
			continue
		}
		item.filePath = spec.path
		item.fileOffset = line.offset
		item.fileEnd = line.end
		state.items = append(state.items, item)
	}
	return state
}

// fileLine is one physical line with its byte offset, so a cursor can point
// between two lines without re-reading the whole file.
type fileLine struct {
	text   string
	offset int64
	// end is the offset just past this line (the next line's start), which is
	// a safe resume boundary: a page that ends at end includes this line.
	end int64
}

// readLinesBefore reads up to want non-empty lines ending before end, oldest
// first, and returns the offset where the next (older) read should end.
//
// The file is read backwards in chunks, so page N does not re-read page 1:
// after returning the newest `want` lines it points at the start of the oldest
// one returned. exhausted reports that the start of the file was reached;
// truncated reports that maxBytes was not enough to find `want` lines (the next
// page continues from the same boundary, so data is delayed, never lost).
func readLinesBefore(path string, end int64, want int, maxBytes int64) ([]fileLine, int64, bool, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, true, false, nil
		}
		return nil, 0, false, false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, false, false, err
	}
	size := st.Size()
	if end <= 0 || end > size {
		end = size
	}
	if end == 0 {
		return nil, 0, true, false, nil
	}
	if want < 1 {
		want = 1
	}
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}

	const chunkSize = 128 << 10
	buf := make([]byte, 0, chunkSize)
	bufStart := end
	var read int64
	for {
		if len(splitFileLines(buf, bufStart, size)) >= want || bufStart == 0 || read >= maxBytes {
			break
		}
		n := int64(chunkSize)
		if n > bufStart {
			n = bufStart
		}
		chunk := make([]byte, n)
		if _, err := f.ReadAt(chunk, bufStart-n); err != nil && err != io.EOF {
			return nil, 0, false, false, err
		}
		buf = append(chunk, buf...)
		bufStart -= n
		read += n
	}

	lines := splitFileLines(buf, bufStart, size)
	truncated := read >= maxBytes && len(lines) < want && bufStart > 0
	// Reaching the start of the buffer is not the same as returning everything:
	// when the file fits in one chunk the buffer starts at byte 0 while `want`
	// may still have trimmed older lines. Only an untrimmed read of the file
	// start means there is nothing older left.
	exhausted := bufStart == 0 && len(lines) <= want
	if len(lines) > want {
		lines = lines[len(lines)-want:]
	}
	next := int64(0)
	if !exhausted {
		if len(lines) > 0 {
			next = lines[0].offset
		} else {
			// Only blank lines in the window: still move the cursor back.
			next = bufStart
		}
	}
	return lines, next, exhausted, truncated, nil
}

// splitFileLines cuts the buffer into non-empty lines with absolute offsets. A
// buffer that starts mid-file may begin in the middle of a line; that partial
// head is dropped (the next read ends at a line boundary and picks it up in
// full). The final line is only complete when the buffer reaches EOF.
func splitFileLines(buf []byte, bufStart, size int64) []fileLine {
	if len(buf) == 0 {
		return nil
	}
	atEOF := bufStart+int64(len(buf)) == size
	start := 0
	if bufStart > 0 {
		idx := bytes.IndexByte(buf, '\n')
		if idx < 0 {
			return nil
		}
		start = idx + 1
	}
	var out []fileLine
	for i := start; i < len(buf); {
		j := bytes.IndexByte(buf[i:], '\n')
		var end, next int
		if j >= 0 {
			end = i + j
			next = end + 1
		} else {
			if !atEOF {
				break
			}
			end = len(buf)
			next = len(buf)
		}
		raw := buf[i:end]
		if len(bytes.TrimSpace(raw)) > 0 {
			out = append(out, fileLine{
				text:   string(raw),
				offset: bufStart + int64(i),
				end:    bufStart + int64(next),
			})
		}
		if j < 0 {
			break
		}
		i = next
	}
	return out
}
