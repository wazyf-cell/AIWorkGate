package main

// SSE framing for the chat relay. Events are delimited by a blank line and a
// data field may span several "data:" lines; the receiver joins those with a
// newline (WHATWG HTML, server-sent events). The relay must therefore reason in
// events, not lines, or a multi-line payload is torn into unrelated events.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxSSELineBytes bounds one wire line; the largest Cline frame seen is far below.
const maxSSELineBytes = 8 << 20

// sseDoneFrame is the OpenAI end-of-stream marker.
const sseDoneFrame = "[DONE]"

// sseEvent is one wire event: the non-data fields (event:, id:, retry: and
// comment lines) verbatim, plus the data lines in order.
type sseEvent struct {
	fields []string
	data   []string
}

// done reports the OpenAI end marker.
func (e *sseEvent) done() bool {
	return strings.TrimSpace(strings.Join(e.data, "\n")) == sseDoneFrame
}

// writeSSEEvent re-frames one event. Each data line goes out as its own "data:"
// line, which is how SSE carries a value containing newlines.
func writeSSEEvent(w io.Writer, e *sseEvent) {
	for _, field := range e.fields {
		fmt.Fprintf(w, "%s\n", field)
	}
	for _, data := range e.data {
		fmt.Fprintf(w, "data: %s\n", data)
	}
	fmt.Fprint(w, "\n")
}

// writeStreamError reports a mid-stream failure in band: the response headers are
// already sent, so an HTTP status is no longer available.
func writeStreamError(w io.Writer, flusher http.Flusher, message string) {
	payload, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": message, "type": "upstream_error"},
	})
	fmt.Fprintf(w, "data: %s\n\n", payload)
	flusher.Flush()
}

// sseReader parses events off an upstream stream.
type sseReader struct {
	sc *bufio.Scanner
}

func newSSEReader(r io.Reader) *sseReader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxSSELineBytes)
	return &sseReader{sc: sc}
}

// next returns the next event, or io.EOF once the stream ends cleanly. A trailing
// partial event is returned rather than dropped, so a truncated tail is visible
// to the relay. A read failure is returned as-is.
func (r *sseReader) next() (*sseEvent, error) {
	event := &sseEvent{}
	for r.sc.Scan() {
		line := strings.TrimRight(r.sc.Text(), "\r")
		if line == "" {
			if len(event.fields) == 0 && len(event.data) == 0 {
				continue // padding or the blank line that ended the previous event
			}
			return event, nil
		}
		if !strings.HasPrefix(line, "data:") {
			event.fields = append(event.fields, line)
			continue
		}
		// A single space after the colon is part of the framing, not the value.
		data := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
		event.data = append(event.data, data)
	}
	if err := r.sc.Err(); err != nil {
		return nil, err
	}
	if len(event.fields) > 0 || len(event.data) > 0 {
		return event, nil
	}
	return nil, io.EOF
}

// relayStream forwards the upstream SSE event by event, flushing every frame.
//
// Headers are written only once a first event exists: an empty upstream answer
// must be a 502, not a 200 with nothing in it. A read failure becomes an in-band
// error frame with no [DONE], so the host cannot mistake a truncated stream for a
// clean finish.
func (s *server) relayStream(w http.ResponseWriter, src io.Reader) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming_unsupported", "writer cannot flush")
		return
	}
	events := newSSEReader(src)
	first, err := events.next()
	if err != nil {
		s.failStream(w, err)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	writeSSEEvent(w, first)
	flusher.Flush()

	sawDone := first.done()
	for {
		event, err := events.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			s.log("warn", "upstream_stream_aborted", map[string]any{"error": err.Error()})
			writeStreamError(w, flusher, "upstream stream error: "+err.Error())
			return
		}
		writeSSEEvent(w, event)
		flusher.Flush()
		if event.done() {
			sawDone = true
		}
	}
	// Cline ends with [DONE] itself; only a stream that stopped short of it needs
	// a synthetic terminator. Appending one unconditionally duplicates the frame.
	if !sawDone {
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}
}

// failStream answers a stream that yielded no usable event, before any header is
// sent, with a real HTTP error.
func (s *server) failStream(w http.ResponseWriter, err error) {
	if err == io.EOF {
		writeError(w, http.StatusBadGateway, "upstream_error", "upstream returned an empty stream")
		return
	}
	s.log("warn", "upstream_stream_aborted", map[string]any{"error": err.Error()})
	writeError(w, http.StatusBadGateway, "upstream_error", "cannot read upstream stream: "+err.Error())
}
