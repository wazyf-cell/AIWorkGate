package webui

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"aiworkgate/internal/model"
)

// actionSink collects the entries recordAction hands to the host.
type actionSink struct {
	mu      sync.Mutex
	entries []model.AuditEntry
	err     error
}

func (a *actionSink) record(entry model.AuditEntry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return a.err
	}
	a.entries = append(a.entries, entry)
	return nil
}

func (a *actionSink) all() []model.AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]model.AuditEntry(nil), a.entries...)
}

// newActionHandler builds a handler whose only meaningful option is the action
// recorder. Every other callback is nil, which the routes answer with 501 -- the
// action log must still record the attempt.
func newActionHandler(t *testing.T, sink *actionSink) http.Handler {
	t.Helper()
	opts := Options{RecordAction: sink.record}
	return New(opts)
}

// A write call must produce exactly one action entry naming the method and the
// route.
func TestActionLogRecordsWriteCall(t *testing.T) {
	sink := &actionSink{}
	handler := newActionHandler(t, sink)

	doRequest(t, handler, http.MethodPut, apiPrefix+"/settings", `{}`)

	got := sink.all()
	if len(got) != 1 {
		t.Fatalf("entries = %d, want 1", len(got))
	}
	if got[0].Category != actionCategory {
		t.Fatalf("category = %q, want %q", got[0].Category, actionCategory)
	}
	if got[0].Action != "PUT /api/settings" {
		t.Fatalf("action = %q, want %q", got[0].Action, "PUT /api/settings")
	}
}

// Polling must never be recorded. Without this, a dashboard that refreshes
// /api/status every few seconds would bury every real action.
func TestActionLogSkipsPollingReads(t *testing.T) {
	sink := &actionSink{}
	handler := newActionHandler(t, sink)

	for i := 0; i < 10; i++ {
		doRequest(t, handler, http.MethodGet, apiPrefix+"/status", "")
	}
	doRequest(t, handler, http.MethodGet, apiPrefix+"/logs/requests", "")
	doRequest(t, handler, http.MethodGet, apiPrefix+"/usage/summary", "")

	if got := sink.all(); len(got) != 0 {
		t.Fatalf("recorded %d polling entries, want 0 (%+v)", len(got), got)
	}
}

// A client key id in the URL must be templated away. A key id is exactly the
// kind of secret that would leak through a "harmless" log line.
func TestActionLogTemplatesSensitiveIDs(t *testing.T) {
	sink := &actionSink{}
	handler := newActionHandler(t, sink)

	const secret = "agk_secret123"
	doRequest(t, handler, http.MethodPost, apiPrefix+"/keys/"+secret, `{}`)

	got := sink.all()
	if len(got) != 1 {
		t.Fatalf("entries = %d, want 1", len(got))
	}
	if got[0].Action != "POST /api/keys/{id}" {
		t.Fatalf("action = %q, want %q", got[0].Action, "POST /api/keys/{id}")
	}
	for _, entry := range got {
		blob := entry.TS + entry.Category + entry.Action + entry.Target + entry.Detail + entry.Result
		if strings.Contains(blob, secret) {
			t.Fatalf("the client key id reached the audit entry: %q", blob)
		}
	}
}

// A nil recorder must disable the feature without panicking.
func TestActionLogNilRecorderIsInert(t *testing.T) {
	handler := New(Options{})
	doRequest(t, handler, http.MethodPut, apiPrefix+"/settings", `{}`)
}

// A failing recorder must not change the response the client receives.
func TestActionLogFailureDoesNotChangeResponse(t *testing.T) {
	okSink := &actionSink{}
	okHandler := newActionHandler(t, okSink)
	want := doRequest(t, okHandler, http.MethodPut, apiPrefix+"/settings", `{}`)

	badSink := &actionSink{err: errors.New("database is locked")}
	badHandler := newActionHandler(t, badSink)
	got := doRequest(t, badHandler, http.MethodPut, apiPrefix+"/settings", `{}`)

	if got.Code != want.Code {
		t.Fatalf("status = %d, want %d (a failed audit write must not change the response)", got.Code, want.Code)
	}
}

// Reads that are not polling are still not user actions: only state changes are.
func TestActionLogIgnoresNonPollingReads(t *testing.T) {
	sink := &actionSink{}
	handler := newActionHandler(t, sink)

	doRequest(t, handler, http.MethodGet, apiPrefix+"/accounts", "")

	if got := sink.all(); len(got) != 0 {
		t.Fatalf("recorded %d GET entries, want 0", len(got))
	}
}

// pathTemplate must keep nested resources readable while templating the id.
func TestActionPathTemplate(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/api/keys/agk_abc", "/api/keys/{id}"},
		{"/api/pools/officeace/models", "/api/pools/{id}/models"},
		{"/api/plugins/codearts/install", "/api/plugins/{id}/install"},
		{"/api/scheduler/pools/officeace", "/api/scheduler/pools/{id}"},
		{"/api/settings", "/api/settings"},
		{"/api/accounts/bulk", "/api/accounts/bulk"},
	}
	for _, tc := range cases {
		if got := actionPathTemplate(tc.in); got != tc.want {
			t.Errorf("actionPathTemplate(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A failed call must carry the reason the handler answered with, so a request
// that never reached its host callback (unknown plugin, unsupported route) is
// still diagnosable from the timeline alone.
func TestActionLogRecordsFailureReason(t *testing.T) {
	sink := &actionSink{}
	handler := New(Options{
		RecordAction: sink.record,
		CreateKeySpec: func(KeySpec) (string, model.ClientKey, error) {
			t.Fatal("the request must be rejected before the host is called")
			return "", model.ClientKey{}, nil
		},
	})

	got := doRequest(t, handler, http.MethodPost, apiPrefix+"keys", `{}`)
	if got.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", got.Code, got.Body.String())
	}

	entries := sink.all()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if !strings.Contains(entries[0].Detail, "name is required") {
		t.Fatalf("detail = %q, want it to carry the validation reason", entries[0].Detail)
	}
	if entries[0].Result != "4xx" {
		t.Fatalf("result = %q, want 4xx", entries[0].Result)
	}
}

// Debug mode shows which resource an action touched; without it the id stays
// templated. A client key id is never revealed, even under debug.
func TestActionLogDebugModeRevealsResourceIDs(t *testing.T) {
	cases := []struct {
		name    string
		debug   bool
		target  string
		wantAct string
	}{
		{"plain keeps template", false, apiPrefix + "proxies/px_1/check", "POST /api/proxies/{id}/check"},
		{"debug reveals id", true, apiPrefix + "proxies/px_1/check", "POST /api/proxies/px_1/check"},
		{"debug still templates key id", true, apiPrefix + "keys/agk_leakme", "POST /api/keys/{id}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &actionSink{}
			opts := Options{RecordAction: sink.record}
			if tc.debug {
				opts.DebugActionIDs = func() bool { return true }
			}
			doRequest(t, New(opts), http.MethodPost, tc.target, `{}`)

			entries := sink.all()
			if len(entries) != 1 {
				t.Fatalf("entries = %d, want 1", len(entries))
			}
			if entries[0].Action != tc.wantAct {
				t.Fatalf("action = %q, want %q", entries[0].Action, tc.wantAct)
			}
			blob := entries[0].Action + entries[0].Target + entries[0].Detail
			if strings.Contains(blob, "agk_leakme") {
				t.Fatalf("client key id reached the audit row: %q", blob)
			}
		})
	}
}

// A long error sentence must not bloat the audit row.
func TestActionReasonCapsAndCleansText(t *testing.T) {
	body := []byte(`{"error":{"message":"` + strings.Repeat("x", 500) + `\nsecond line"}}`)
	got := actionReason(body)
	if len([]rune(got)) > maxActionReasonRunes {
		t.Fatalf("reason length = %d, want at most %d", len([]rune(got)), maxActionReasonRunes)
	}
	if strings.Contains(got, "\n") {
		t.Fatalf("reason kept a newline: %q", got)
	}
	if actionReason(nil) != "" {
		t.Fatal("empty body must yield no reason")
	}
	if got := actionReason([]byte("plugin is not running")); got != "plugin is not running" {
		t.Fatalf("plain text reason = %q", got)
	}
}

var _ = httptest.NewRecorder
