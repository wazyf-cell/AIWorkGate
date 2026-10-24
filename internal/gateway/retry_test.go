package gateway

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"aiworkgate/internal/model"
	"aiworkgate/internal/scheduler"
)

// ---------------------------------------------------------------- pure policy

func TestParseRetryAfterSecondsAndHTTPDate(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		raw   string
		want  time.Duration
		valid bool
	}{
		{name: "seconds", raw: "7", want: 7 * time.Second, valid: true},
		{name: "seconds with spaces", raw: " 7 ", want: 7 * time.Second, valid: true},
		{name: "zero seconds", raw: "0", want: 0, valid: true},
		{name: "capped", raw: "9999", want: maxRetryWait, valid: true},
		{name: "negative", raw: "-1", want: 0, valid: false},
		{name: "garbage", raw: "soon", want: 0, valid: false},
		{name: "empty", raw: "", want: 0, valid: false},
		{
			name:  "http date in the future",
			raw:   now.Add(45 * time.Second).Format(http.TimeFormat),
			want:  45 * time.Second,
			valid: true,
		},
		{
			name:  "http date in the past",
			raw:   now.Add(-90 * time.Second).Format(http.TimeFormat),
			want:  0,
			valid: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseRetryAfter(tc.raw, now)
			if ok != tc.valid || got != tc.want {
				t.Fatalf("parseRetryAfter(%q) = (%v, %v), want (%v, %v)", tc.raw, got, ok, tc.want, tc.valid)
			}
		})
	}
}

func TestPlanRetryStatusTable(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	header := func(retryAfter string) http.Header {
		h := http.Header{}
		if retryAfter != "" {
			h.Set("Retry-After", retryAfter)
		}
		return h
	}
	same := func(d time.Duration) retryPlan { return retryPlan{action: retrySameAccount, delay: d} }
	sw := retryPlan{action: retrySwitchAccount}
	stop := retryPlan{action: retryStop}

	cases := []struct {
		name    string
		status  int
		header  http.Header
		body    string
		attempt int
		want    retryPlan
	}{
		{name: "429 default 1s", status: 429, header: header(""), attempt: 0, want: same(time.Second)},
		{name: "429 default 2s", status: 429, header: header(""), attempt: 1, want: same(2 * time.Second)},
		{name: "429 default 3s", status: 429, header: header(""), attempt: 2, want: same(3 * time.Second)},
		{name: "429 exhausted", status: 429, header: header(""), attempt: 3, want: sw},
		{name: "429 retry-after seconds", status: 429, header: header("7"), attempt: 0, want: same(7 * time.Second)},
		{
			name: "429 retry-after date", status: 429, attempt: 0,
			header: header(now.Add(45 * time.Second).Format(http.TimeFormat)),
			want:   same(45 * time.Second),
		},
		{name: "429 invalid retry-after falls back", status: 429, header: header("soon"), attempt: 1, want: same(2 * time.Second)},
		{name: "503 first", status: 503, header: header(""), attempt: 0, want: same(10 * time.Second)},
		{name: "503 second", status: 503, header: header(""), attempt: 1, want: same(20 * time.Second)},
		{name: "503 third", status: 503, header: header(""), attempt: 2, want: same(40 * time.Second)},
		{name: "503 exhausted", status: 503, header: header(""), attempt: 3, want: sw},
		{name: "529 mirrors 503", status: 529, header: header(""), attempt: 1, want: same(20 * time.Second)},
		{name: "502 retries once", status: 502, header: header(""), attempt: 0, want: same(500 * time.Millisecond)},
		{name: "502 exhausted", status: 502, header: header(""), attempt: 1, want: sw},
		{name: "401 switches", status: 401, header: header(""), attempt: 0, want: sw},
		{name: "403 switches", status: 403, header: header(""), attempt: 0, want: sw},
		{name: "400 stops", status: 400, header: header(""), attempt: 0, want: stop},
		{
			name: "400 thinking signature retries the same account", status: 400, attempt: 0,
			body: `{"error":{"message":"invalid thinking.signature"}}`,
			want: same(200 * time.Millisecond),
		},
		{
			name: "400 thinking signature retries only once", status: 400, attempt: 1,
			body: `{"error":{"message":"invalid thinking signature"}}`,
			want: stop,
		},
		{
			name: "400 credit exhaustion switches accounts", status: 400, attempt: 0,
			body: `{"error":{"message":"余额不足，请充值"}}`,
			want: sw,
		},
		{
			name: "400 insufficient credit switches accounts", status: 400, attempt: 2,
			body: `{"error":{"message":"Insufficient Credit"}}`,
			want: sw,
		},
		{
			name: "400 quota exceeded switches accounts", status: 400, attempt: 0,
			body: `{"error":{"message":"quota exceeded"}}`,
			want: sw,
		},
		{
			name: "400 code 11128 retries the same account", status: 400, attempt: 0,
			body: `{"error":{"code":11128,"message":"Illegal API invocation"}}`,
			want: same(200 * time.Millisecond),
		},
		{
			name: "400 code 11128 retries only once", status: 400, attempt: 1,
			body: `{"error":{"code": 11128}}`,
			want: stop,
		},
		{name: "400 near-miss code stops", status: 400, attempt: 0, body: `{"code":111280}`, want: stop},
		{name: "404 switches", status: 404, header: header(""), attempt: 0, want: sw},
		{name: "422 stops", status: 422, header: header(""), attempt: 0, want: stop},
		{name: "500 switches", status: 500, header: header(""), attempt: 0, want: sw},
		{name: "504 switches", status: 504, header: header(""), attempt: 0, want: sw},
		{name: "transport failure switches", status: 0, header: header(""), attempt: 0, want: sw},
		{name: "408 switches", status: 408, header: header(""), attempt: 0, want: sw},
		{name: "409 switches", status: 409, header: header(""), attempt: 0, want: sw},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := planRetry(tc.status, tc.header, tc.body, tc.attempt, now); got != tc.want {
				t.Fatalf("planRetry(%d, %q, attempt %d) = %+v, want %+v", tc.status, tc.body, tc.attempt, got, tc.want)
			}
		})
	}
}

// TestRetry400Classifiers pins the keyword tables ported from A retry.rs:22-59
// and mod.rs:624-630, including the deliberately narrow 11128 matcher.
func TestRetry400Classifiers(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		thinking bool
		credit   bool
		channel  bool
	}{
		{name: "thinking.signature", body: `{"error":{"message":"invalid thinking.signature"}}`, thinking: true},
		{name: "thinking signature prose", body: `bad thinking block: signature mismatch`, thinking: true},
		{name: "signature only", body: `signature mismatch`},
		{name: "insufficient credit", body: `{"error":{"message":"Insufficient Credit"}}`, credit: true},
		{name: "insufficient balance", body: `insufficient balance`, credit: true},
		{name: "quota exceeded", body: `quota exceeded for this account`, credit: true},
		{name: "chinese balance", body: `{"msg":"余额不足"}`, credit: true},
		{name: "chinese quota", body: `{"msg":"额度不足"}`, credit: true},
		{name: "chinese points", body: `{"msg":"积分不足"}`, credit: true},
		{name: "random error", body: `{"error":{"message":"upstream says no"}}`},
		{name: "code 11128", body: `{"error":{"code":11128,"msg":"x"}}`, channel: true},
		{name: "code with spaces", body: `{"error":{"code": 11128 }}`, channel: true},
		{name: "illegal invocation", body: `Illegal API invocation detected`, channel: true},
		{name: "unapproved channel", body: `unapproved channel`, channel: true},
		{name: "code 111280 near miss", body: `{"code":111280}`},
		{name: "code 111289 near miss", body: `{"code":111289}`},
		{name: "some_code near miss", body: `{"some_code":11128}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isThinkingSignature400(tc.body); got != tc.thinking {
				t.Fatalf("isThinkingSignature400(%q) = %v", tc.body, got)
			}
			if got := isCreditExhausted400(tc.body); got != tc.credit {
				t.Fatalf("isCreditExhausted400(%q) = %v", tc.body, got)
			}
			if got := isIllegalChannel400(tc.body); got != tc.channel {
				t.Fatalf("isIllegalChannel400(%q) = %v", tc.body, got)
			}
		})
	}
}

// ---------------------------------------------------------------- test doubles
type sleepRecorder struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (s *sleepRecorder) sleep(ctx context.Context, d time.Duration) bool {
	s.mu.Lock()
	s.waits = append(s.waits, d)
	s.mu.Unlock()
	return ctx.Err() == nil
}

func (s *sleepRecorder) durations() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.waits...)
}

type fakeSchedLogger struct {
	mu   sync.Mutex
	rows []map[string]any
}

func (f *fakeSchedLogger) Info(_, _ string, fields map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row := make(map[string]any, len(fields))
	for k, v := range fields {
		row[k] = v
	}
	f.rows = append(f.rows, row)
}

func (f *fakeSchedLogger) Warn(_, _ string, _ map[string]any) {}

func (f *fakeSchedLogger) snapshot() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]any, 0, len(f.rows))
	for _, row := range f.rows {
		cp := make(map[string]any, len(row))
		for k, v := range row {
			cp[k] = v
		}
		out = append(out, cp)
	}
	return out
}

// refreshingRegistry adds the optional credential-refresh entry to the fake
// plugin registry.
type refreshingRegistry struct {
	*fakeRegistry
	mu        sync.Mutex
	refreshes int
	result    model.RefreshResult
	err       error
}

func (r *refreshingRegistry) Refresh(context.Context, string, string) (model.RefreshResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refreshes++
	return r.result, r.err
}

func (r *refreshingRegistry) refreshCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refreshes
}

func retryGateway(t *testing.T, reg PluginRegistry, snap *scheduler.Snapshot, logger SchedLogger) (*Gateway, *sleepRecorder) {
	t.Helper()
	sleeps := &sleepRecorder{}
	g := newTestGateway(t, Options{
		Plugins:  reg,
		Log:      &fakeLogSink{},
		Schedule: scheduler.New(scheduler.Options{}),
		Snapshot: staticSnapshot(snap),
		SchedLog: logger,
	})
	g.sleep = sleeps.sleep
	return g, sleeps
}

func assertWaits(t *testing.T, got []time.Duration, want ...time.Duration) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("backoff waits = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("backoff waits = %v, want %v", got, want)
		}
	}
}

func alwaysStatus(status int, headers map[string]string) func(http.ResponseWriter, *http.Request, []byte) {
	return func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":{"message":"upstream says no"}}`)
	}
}

// ---------------------------------------------------------------- 429 / 503

func TestScheduled429BackoffWithoutRetryAfter(t *testing.T) {
	srv, calls := newUpstreamServer(t, alwaysStatus(http.StatusTooManyRequests, nil))
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	g, sleeps := retryGateway(t, reg, singlePoolSnapshot("p1", 2, "acc1"), nil)

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"p1/m1"}`))
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if calls.count() != 4 {
		t.Fatalf("upstream calls = %d, want 4 (first try + 1s/2s/3s retries)", calls.count())
	}
	for i, req := range calls.all() {
		if got := req.Header.Get(accountHeader); got != "acc1" {
			t.Fatalf("attempt %d account = %q, want acc1", i, got)
		}
	}
	assertWaits(t, sleeps.durations(), time.Second, 2*time.Second, 3*time.Second)
}

func TestScheduled429HonoursRetryAfterSeconds(t *testing.T) {
	srv, calls := newUpstreamServer(t, alwaysStatus(http.StatusTooManyRequests, map[string]string{"Retry-After": "7"}))
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	logger := &fakeSchedLogger{}
	g, sleeps := retryGateway(t, reg, singlePoolSnapshot("p1", 2, "acc1"), logger)

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"p1/m1"}`))
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if calls.count() != 4 {
		t.Fatalf("upstream calls = %d, want 4", calls.count())
	}
	assertWaits(t, sleeps.durations(), 7*time.Second, 7*time.Second, 7*time.Second)

	// The scheduler log must carry the raw header so an operator can explain
	// why the request waited.
	logged := false
	for _, row := range logger.snapshot() {
		if row["retry_after"] == "7" {
			logged = true
			break
		}
	}
	if !logged {
		t.Fatalf("retry_after missing from scheduler log: %v", logger.snapshot())
	}
}

func TestScheduled429HonoursRetryAfterHTTPDate(t *testing.T) {
	fixed := time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)
	header := fixed.Add(45 * time.Second).Format(http.TimeFormat)
	srv, calls := newUpstreamServer(t, alwaysStatus(http.StatusTooManyRequests, map[string]string{"Retry-After": header}))
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	g, sleeps := retryGateway(t, reg, singlePoolSnapshot("p1", 2, "acc1"), nil)
	g.now = func() time.Time { return fixed }

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"p1/m1"}`))
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if calls.count() != 4 {
		t.Fatalf("upstream calls = %d, want 4", calls.count())
	}
	assertWaits(t, sleeps.durations(), 45*time.Second, 45*time.Second, 45*time.Second)
}

func TestScheduled503BackoffSequence(t *testing.T) {
	srv, calls := newUpstreamServer(t, alwaysStatus(http.StatusServiceUnavailable, nil))
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	g, sleeps := retryGateway(t, reg, singlePoolSnapshot("p1", 2, "acc1"), nil)

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"p1/m1"}`))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if calls.count() != 4 {
		t.Fatalf("upstream calls = %d, want 4 (first try + 10s/20s/40s retries)", calls.count())
	}
	assertWaits(t, sleeps.durations(), 10*time.Second, 20*time.Second, 40*time.Second)
}

// ---------------------------------------------------------------- terminal 4xx

func TestScheduled400DoesNotSwitchAccounts(t *testing.T) {
	srv, calls := newUpstreamServer(t, alwaysStatus(http.StatusBadRequest, nil))
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	g, sleeps := retryGateway(t, reg, singlePoolSnapshot("p1", 2, "acc1", "acc2"), nil)

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"p1/m1"}`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if calls.count() != 1 {
		t.Fatalf("upstream calls = %d, want 1 (400 is terminal)", calls.count())
	}
	if got := calls.last(t).Header.Get(accountHeader); got != "acc1" {
		t.Fatalf("account = %q, want acc1", got)
	}
	assertWaits(t, sleeps.durations())
}

// failOnceThenOK answers badStatus+badBody on the first call per account and a
// success afterwards, so a same-account retry can be observed.
func failOnceThenOK(badStatus int, badBody string) func(http.ResponseWriter, *http.Request, []byte) {
	var mu sync.Mutex
	failed := map[string]bool{}
	return func(w http.ResponseWriter, r *http.Request, _ []byte) {
		account := r.Header.Get(accountHeader)
		mu.Lock()
		first := !failed[account]
		failed[account] = true
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if first {
			w.WriteHeader(badStatus)
			_, _ = io.WriteString(w, badBody)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}
}

func TestScheduled400ThinkingSignatureRetriesSameAccount(t *testing.T) {
	srv, calls := newUpstreamServer(t,
		failOnceThenOK(http.StatusBadRequest, `{"error":{"message":"invalid thinking.signature"}}`))
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	g, sleeps := retryGateway(t, reg, singlePoolSnapshot("p1", 2, "acc1", "acc2"), nil)

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"p1/m1"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	reqs := calls.all()
	if len(reqs) != 2 {
		t.Fatalf("upstream calls = %d, want 2 (same-account retry)", len(reqs))
	}
	for i, req := range reqs {
		if got := req.Header.Get(accountHeader); got != "acc1" {
			t.Fatalf("attempt %d account = %q, want acc1 (no account switch)", i, got)
		}
	}
	assertWaits(t, sleeps.durations(), 200*time.Millisecond)
}

func TestScheduled400IllegalChannelRetriesSameAccount(t *testing.T) {
	srv, calls := newUpstreamServer(t,
		failOnceThenOK(http.StatusBadRequest, `{"error":{"code":11128,"message":"Illegal API invocation"}}`))
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	g, sleeps := retryGateway(t, reg, singlePoolSnapshot("p1", 2, "acc1", "acc2"), nil)

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"p1/m1"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	reqs := calls.all()
	if len(reqs) != 2 {
		t.Fatalf("upstream calls = %d, want 2 (11128 retries the same account once)", len(reqs))
	}
	for i, req := range reqs {
		if got := req.Header.Get(accountHeader); got != "acc1" {
			t.Fatalf("attempt %d account = %q, want acc1", i, got)
		}
	}
	assertWaits(t, sleeps.durations(), 200*time.Millisecond)
}

func TestScheduled400CreditExhaustionSwitchesAccount(t *testing.T) {
	srv, calls := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get(accountHeader) == "acc1" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"余额不足，请充值后重试"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	snap := singlePoolSnapshot("p1", 2, "acc1", "acc2")
	g, sleeps := retryGateway(t, reg, snap, nil)

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"p1/m1"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	reqs := calls.all()
	if len(reqs) != 2 {
		t.Fatalf("upstream calls = %d, want 2 (credit 400 switches accounts)", len(reqs))
	}
	if got := reqs[1].Header.Get(accountHeader); got != "acc2" {
		t.Fatalf("second account = %q, want acc2", got)
	}
	assertWaits(t, sleeps.durations())

	// The exhausted account is parked like a quota failure, so the next request
	// does not bounce off it again.
	next, err := g.sched.Pick(context.Background(), snap, scheduler.Request{Model: "p1/m1"})
	if err != nil {
		t.Fatalf("pick after credit failure: %v", err)
	}
	if next.AccountID != "acc2" {
		t.Fatalf("next account = %q, want acc2 (acc1 out of credits)", next.AccountID)
	}
}

func TestScheduled400CreditExhaustionWithoutAlternativeSurfacesOriginal(t *testing.T) {
	const body = `{"error":{"message":"Insufficient Credit"}}`
	srv, calls := newUpstreamServer(t, jsonResponder(http.StatusBadRequest, body))
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	g, sleeps := retryGateway(t, reg, singlePoolSnapshot("p1", 2, "acc1"), nil)

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"p1/m1"}`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want the original 400 (body %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "Insufficient Credit") {
		t.Fatalf("body = %q, want the upstream 400 body", rr.Body.String())
	}
	if calls.count() != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls.count())
	}
	assertWaits(t, sleeps.durations())
}

// ---------------------------------------------------------------- 401 / 403
func TestScheduled401SwitchesWithoutWaiting(t *testing.T) {
	srv, calls := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get(accountHeader) == "acc1" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"expired"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	g, sleeps := retryGateway(t, reg, singlePoolSnapshot("p1", 2, "acc1", "acc2"), nil)

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"p1/m1"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	reqs := calls.all()
	if len(reqs) != 2 {
		t.Fatalf("upstream calls = %d, want 2", len(reqs))
	}
	if got := reqs[0].Header.Get(accountHeader); got != "acc1" {
		t.Fatalf("first account = %q, want acc1", got)
	}
	if got := reqs[1].Header.Get(accountHeader); got != "acc2" {
		t.Fatalf("second account = %q, want acc2", got)
	}
	assertWaits(t, sleeps.durations())
}

func TestScheduled401APILevelOneStops(t *testing.T) {
	srv, calls := newUpstreamServer(t, alwaysStatus(http.StatusUnauthorized, nil))
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	g, sleeps := retryGateway(t, reg, singlePoolSnapshot("p1", 1, "acc1"), nil)

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"p1/m1"}`))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if calls.count() != 1 {
		t.Fatalf("upstream calls = %d, want 1 (api_level 1 has no account to switch)", calls.count())
	}
	assertWaits(t, sleeps.durations())
}

func TestScheduled401RefreshSelfHealsOnce(t *testing.T) {
	var mu sync.Mutex
	acc1Calls := 0
	srv, calls := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get(accountHeader) == "acc1" {
			mu.Lock()
			acc1Calls++
			first := acc1Calls == 1
			mu.Unlock()
			if first {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":{"message":"expired"}}`)
				return
			}
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	base := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	reg := &refreshingRegistry{fakeRegistry: base, result: model.RefreshResult{Result: "ok"}}
	g, _ := retryGateway(t, reg, singlePoolSnapshot("p1", 2, "acc1", "acc2"), nil)

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"p1/m1"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if reg.refreshCount() != 1 {
		t.Fatalf("refresh calls = %d, want 1", reg.refreshCount())
	}
	reqs := calls.all()
	if len(reqs) != 2 {
		t.Fatalf("upstream calls = %d, want 2 (refresh retries the same account)", len(reqs))
	}
	for i, req := range reqs {
		if got := req.Header.Get(accountHeader); got != "acc1" {
			t.Fatalf("attempt %d account = %q, want acc1", i, got)
		}
	}
}

func TestScheduled401RefreshSpentThenSwitches(t *testing.T) {
	srv, calls := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get(accountHeader) == "acc1" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"expired"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	base := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	reg := &refreshingRegistry{fakeRegistry: base, result: model.RefreshResult{Result: "ok"}}
	g, sleeps := retryGateway(t, reg, singlePoolSnapshot("p1", 2, "acc1", "acc2"), nil)

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"p1/m1"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if reg.refreshCount() != 1 {
		t.Fatalf("refresh calls = %d, want exactly one per account", reg.refreshCount())
	}
	reqs := calls.all()
	if len(reqs) != 3 {
		t.Fatalf("upstream calls = %d, want 3 (acc1, refreshed acc1, acc2)", len(reqs))
	}
	want := []string{"acc1", "acc1", "acc2"}
	for i, w := range want {
		if got := reqs[i].Header.Get(accountHeader); got != w {
			t.Fatalf("attempt %d account = %q, want %s", i, got, w)
		}
	}
	assertWaits(t, sleeps.durations())
}

// ---------------------------------------------------------------- hedging

func TestHedgeDisabledSendsSingleAttempt(t *testing.T) {
	srv, calls := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set(accountHeader, r.Header.Get(accountHeader))
		_, _ = io.WriteString(w, "data: ok\n\n")
	})
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	snap := singlePoolSnapshot("p1", 2, "acc1", "acc2")
	snap.Pools["p1"].HedgeMS = 0
	g := scheduledGateway(t, reg, &fakeLogSink{}, snap)

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"p1/m1","stream":true}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if calls.count() != 1 {
		t.Fatalf("upstream calls = %d, want 1 when hedge_ms=0", calls.count())
	}
}

func TestHedgeEnabledSendsSecondAttempt(t *testing.T) {
	srv, calls := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.Header.Get(accountHeader) == "acc1" {
			time.Sleep(1100 * time.Millisecond)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set(accountHeader, r.Header.Get(accountHeader))
		_, _ = io.WriteString(w, "data: "+r.Header.Get(accountHeader)+"\n\n")
	})
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	snap := singlePoolSnapshot("p1", 2, "acc1", "acc2")
	snap.Pools["p1"].HedgeMS = 1000
	g := scheduledGateway(t, reg, &fakeLogSink{}, snap)

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"p1/m1","stream":true}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if calls.count() != 2 {
		t.Fatalf("upstream calls = %d, want 2 when the first byte is late", calls.count())
	}
	if got := rr.Header().Get(accountHeader); got != "acc2" {
		t.Fatalf("winning account = %q, want acc2", got)
	}
}

func TestHedgeNeedsAnotherHealthyAccount(t *testing.T) {
	srv, calls := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		time.Sleep(1100 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set(accountHeader, r.Header.Get(accountHeader))
		_, _ = io.WriteString(w, "data: ok\n\n")
	})
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	snap := singlePoolSnapshot("p1", 2, "acc1")
	snap.Pools["p1"].HedgeMS = 1000
	g := scheduledGateway(t, reg, &fakeLogSink{}, snap)

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"p1/m1","stream":true}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if calls.count() != 1 {
		t.Fatalf("upstream calls = %d, want 1 without a second healthy account", calls.count())
	}
}
