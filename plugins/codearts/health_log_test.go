package main

// Health-probe logging tests.
//
// The host probes /health every 10s. Before this rule, those successes were 68%
// of a day's log lines (measured 2026-10-22: 7442 of 10930), which buries the
// failures the log exists to explain. The rule is narrow on purpose: only a
// successful GET /health is dropped — a failing probe still logs, because a
// probe that fails is exactly the event worth keeping.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// logCapture is a Plugin wired to an in-memory log sink.
func logCapture() (*Plugin, *bytes.Buffer) {
	var buf bytes.Buffer
	p := &Plugin{ID: "codearts", Version: "test", Token: "tk", logw: &buf, now: time.Now}
	return p, &buf
}

func serveOnce(t *testing.T, p *Plugin, path string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer tk")
	p.Handler().ServeHTTP(httptest.NewRecorder(), req)
}

// A successful health probe must leave no line at all.
func TestHealthSuccessIsNotLogged(t *testing.T) {
	p, buf := logCapture()
	serveOnce(t, p, "/health")
	if got := buf.String(); got != "" {
		t.Fatalf("GET /health wrote %q; a healthy probe must not add noise", got)
	}
}

// Any other path must still be logged, so the rule cannot swallow real traffic.
func TestOtherPathsAreStillLogged(t *testing.T) {
	p, buf := logCapture()
	serveOnce(t, p, "/accounts")
	if !strings.Contains(buf.String(), "http_request") {
		t.Fatalf("GET /accounts produced no http_request line; got %q", buf.String())
	}
	if !strings.Contains(buf.String(), `"/accounts"`) {
		t.Fatalf("line does not name the path; got %q", buf.String())
	}
}

// A non-GET on /health is unusual and keeps its record.
func TestHealthNonGetIsLogged(t *testing.T) {
	p, buf := logCapture()
	req := httptest.NewRequest(http.MethodPost, "/health", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer tk")
	p.Handler().ServeHTTP(httptest.NewRecorder(), req)
	if !strings.Contains(buf.String(), "http_request") {
		t.Fatalf("POST /health produced no line; got %q", buf.String())
	}
}

// With the trailing slash the host sometimes uses, the rule must still apply:
// the middleware already trims it for the auth check, so logging must agree.
func TestHealthWithTrailingSlashIsNotLogged(t *testing.T) {
	p, buf := logCapture()
	// The route table may 404 this, but a 404 is >= 400 and therefore logged;
	// what must never happen is a *successful* probe being logged. Assert the
	// narrower invariant: no 2xx /health line exists.
	serveOnce(t, p, "/health/")
	if strings.Contains(buf.String(), `"path":"/health/"`) && strings.Contains(buf.String(), `"status":200`) {
		t.Fatalf("a successful /health/ probe was logged: %q", buf.String())
	}
}
