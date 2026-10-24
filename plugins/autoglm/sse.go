package main

// OpenAI-compatible SSE output writers.
//
// Ported from chat.go (sseWriter / listWriter). The relay event converter lives
// in relay_converter.go + relay_emit.go; the aggregation lives in aggregate.go.

import (
	"fmt"
	"io"
	"net/http"
	"sync"
)

// chunkOut is the sink the converter writes to: either a live SSE writer or an
// in-memory buffer for the non-streaming path.
type chunkOut interface {
	writeData(payload string)
	writeDone()
}

// sseWriter streams OpenAI chunks to the host.
type sseWriter struct {
	w  http.ResponseWriter
	fl http.Flusher
	mu sync.Mutex
}

func newSSEWriter(w http.ResponseWriter) *sseWriter {
	fl, _ := w.(http.Flusher)
	return &sseWriter{w: w, fl: fl}
}

// writeSSEHeaders emits the SSE preamble and flushes it immediately.
func writeSSEHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "close")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
}

func (s *sseWriter) writeData(payload string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(s.w, "data: %s\n\n", payload)
	if s.fl != nil {
		s.fl.Flush()
	}
}

func (s *sseWriter) writeDone() {
	s.mu.Lock()
	defer s.mu.Unlock()
	io.WriteString(s.w, "data: [DONE]\n\n")
	if s.fl != nil {
		s.fl.Flush()
	}
}

// listWriter buffers chunks so a stream can be collapsed into one completion.
type listWriter struct {
	mu    sync.Mutex
	lines []string
}

func (l *listWriter) writeData(payload string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, "data: "+payload+"\n")
}

func (l *listWriter) writeDone() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, "data: [DONE]\n")
}

func (l *listWriter) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.lines))
	copy(out, l.lines)
	return out
}
