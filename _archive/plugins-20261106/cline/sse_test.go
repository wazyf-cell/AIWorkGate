package main

// The relay must forward SSE as events, not as lines: an event is delimited by a
// blank line and its data may span several "data:" lines. A truncated upstream
// stream must never look like a clean finish.

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

// errAfterReader emits payload first, then fails the read.
type errAfterReader struct {
	payload string
	err     error
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if r.payload == "" {
		return 0, r.err
	}
	n := copy(p, r.payload)
	r.payload = r.payload[n:]
	return n, nil
}

func TestRelayStreamKeepsEventFraming(t *testing.T) {
	upstream := "event: message\ndata: line1\ndata: line2\n\ndata: [DONE]\n\n"
	rec := httptest.NewRecorder()
	(&server{}).relayStream(rec, strings.NewReader(upstream))
	// A multi-line data field belongs to ONE event: splitting it into two
	// events changes the payload the downstream reads.
	if got := rec.Body.String(); !strings.Contains(got, "event: message\ndata: line1\ndata: line2\n\n") {
		t.Fatalf("multi-line event was split:\n%q", got)
	}
}

func TestRelayStreamReadErrorIsNotACleanFinish(t *testing.T) {
	rec := httptest.NewRecorder()
	src := io.MultiReader(
		strings.NewReader("data: {\"choices\":[]}\n\n"),
		&errAfterReader{err: errors.New("boom")},
	)
	(&server{}).relayStream(rec, src)
	body := rec.Body.String()
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("a truncated stream ended with [DONE]:\n%q", body)
	}
	if !strings.Contains(body, "boom") || !strings.Contains(body, "upstream_error") {
		t.Fatalf("read error not surfaced to the caller:\n%q", body)
	}
}

func TestRelayStreamEmptyUpstreamIsAnError(t *testing.T) {
	rec := httptest.NewRecorder()
	(&server{}).relayStream(rec, strings.NewReader(""))
	if rec.Code != 502 {
		t.Fatalf("empty upstream stream = %d %q, want 502", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "empty stream") {
		t.Fatalf("empty stream error = %q", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "data:") {
		t.Fatalf("empty stream answered with SSE:\n%q", rec.Body.String())
	}
}

func TestRelayStreamSendsOneDone(t *testing.T) {
	upstream := "data: {\"choices\":[]}\n\ndata: [DONE]\n\n"
	rec := httptest.NewRecorder()
	(&server{}).relayStream(rec, strings.NewReader(upstream))
	if got := strings.Count(rec.Body.String(), "data: [DONE]"); got != 1 {
		t.Fatalf("[DONE] count = %d, want exactly the upstream one:\n%q", got, rec.Body.String())
	}
}
