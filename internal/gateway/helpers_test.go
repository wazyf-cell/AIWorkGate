package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"aiworkgate/internal/model"
)

// clientKey is the token accepted by the fake key verifier by default.
const clientKey = "agk_client"

type keyIdentity struct {
	id   string
	name string
}

// fakeKeys is an in-memory KeyVerifier.
type fakeKeys struct {
	mu       sync.Mutex
	tokens   map[string]keyIdentity
	verifies int
	touches  map[string]int
}

func newFakeKeys() *fakeKeys {
	return &fakeKeys{
		tokens:  map[string]keyIdentity{clientKey: {id: "key-1", name: "default"}},
		touches: make(map[string]int),
	}
}

func (k *fakeKeys) Verify(plain string) (id, name string, ok bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.verifies++
	ident, ok := k.tokens[plain]
	return ident.id, ident.name, ok
}

func (k *fakeKeys) Touch(id string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.touches[id]++
}

func (k *fakeKeys) touchCount(id string) int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.touches[id]
}

func (k *fakeKeys) verifyCount() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.verifies
}

// fakeLogSink records API request log rows.
type fakeLogSink struct {
	mu   sync.Mutex
	rows []model.APIRequestLog
}

func (s *fakeLogSink) Write(row model.APIRequestLog) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, row)
}

func (s *fakeLogSink) snapshot() []model.APIRequestLog {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.APIRequestLog(nil), s.rows...)
}

// wait blocks until at least n rows were written, then returns all of them.
func (s *fakeLogSink) wait(t *testing.T, n int) []model.APIRequestLog {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		rows := s.snapshot()
		if len(rows) >= n {
			return rows
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d API log rows after 2s, want %d", len(rows), n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

type pluginEndpoint struct {
	baseURL string
	token   string
}

// fakeRegistry implements PluginRegistry.
type fakeRegistry struct {
	mu          sync.Mutex
	statuses    []model.PluginStatus
	endpoints   map[string]pluginEndpoint
	ensureErrs  map[string]error
	concurrency map[string]int
	ensures     map[string]int
	touches     map[string]int
	began       map[string]int
	ended       map[string]int
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{
		endpoints:   make(map[string]pluginEndpoint),
		ensureErrs:  make(map[string]error),
		concurrency: make(map[string]int),
		ensures:     make(map[string]int),
		touches:     make(map[string]int),
		began:       make(map[string]int),
		ended:       make(map[string]int),
	}
}

// BeginCall mirrors plugins.Manager: it keeps the plugin out of the idle
// reaper while a proxied request is in flight.
func (f *fakeRegistry) BeginCall(id string) func() {
	f.mu.Lock()
	f.began[id]++
	f.mu.Unlock()
	return func() {
		f.mu.Lock()
		f.ended[id]++
		f.mu.Unlock()
	}
}

func (f *fakeRegistry) callCounts(id string) (began, ended int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.began[id], f.ended[id]
}

func (f *fakeRegistry) add(st model.PluginStatus, baseURL, token string) *fakeRegistry {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses = append(f.statuses, st)
	f.endpoints[st.Manifest.ID] = pluginEndpoint{baseURL: baseURL, token: token}
	return f
}

func (f *fakeRegistry) setEnsureErr(id string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureErrs[id] = err
}

func (f *fakeRegistry) setConcurrency(id string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.concurrency[id] = n
}

func (f *fakeRegistry) List() []model.PluginStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]model.PluginStatus(nil), f.statuses...)
}

func (f *fakeRegistry) Endpoint(id string) (baseURL, token string, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ep, ok := f.endpoints[id]
	if !ok || ep.baseURL == "" {
		return "", "", false
	}
	return ep.baseURL, ep.token, true
}

func (f *fakeRegistry) EnsureRunning(ctx context.Context, id string) error {
	f.mu.Lock()
	f.ensures[id]++
	err := f.ensureErrs[id]
	f.mu.Unlock()
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func (f *fakeRegistry) Concurrency(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.concurrency[id]
}

func (f *fakeRegistry) Touch(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touches[id]++
}

func (f *fakeRegistry) counts(id string) (ensures, touches int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ensures[id], f.touches[id]
}

func pluginStatus(id string, enabled bool, models ...model.ModelInfo) model.PluginStatus {
	return model.PluginStatus{
		Manifest: model.Manifest{ID: id, Models: models},
		Enabled:  enabled,
		Running:  true,
		Healthy:  true,
	}
}

// fakeA implements AUpstream. Models is served from memory so tests can drive
// success/failure without a server; BaseURL points at an httptest server for
// routing tests.
type fakeA struct {
	mu         sync.Mutex
	base       string
	key        string
	models     []model.ModelInfo
	modelsErr  error
	modelCalls int
}

func (f *fakeA) BaseURL() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.base
}

func (f *fakeA) APIKey() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.key
}

func (f *fakeA) Models(ctx context.Context) ([]model.ModelInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.modelCalls++
	if f.modelsErr != nil {
		return nil, f.modelsErr
	}
	return append([]model.ModelInfo(nil), f.models...), nil
}

func (f *fakeA) setModelsErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.modelsErr = err
}

func (f *fakeA) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.modelCalls
}

type capturedRequest struct {
	Method        string
	Path          string
	RawQuery      string
	Authorization string
	ContentType   string
	Header        http.Header
	Body          []byte
}

type upstreamRecorder struct {
	mu   sync.Mutex
	reqs []capturedRequest
}

func (r *upstreamRecorder) add(c capturedRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, c)
}

func (r *upstreamRecorder) all() []capturedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]capturedRequest(nil), r.reqs...)
}

func (r *upstreamRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reqs)
}

func (r *upstreamRecorder) last(t *testing.T) capturedRequest {
	t.Helper()
	reqs := r.all()
	if len(reqs) == 0 {
		t.Fatal("upstream received no request")
	}
	return reqs[len(reqs)-1]
}

// newUpstreamServer starts an httptest server that records each request (body
// included) and then calls respond. It is used for both A and plugin endpoints.
func newUpstreamServer(t *testing.T, respond func(http.ResponseWriter, *http.Request, []byte)) (*httptest.Server, *upstreamRecorder) {
	t.Helper()
	rec := &upstreamRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.add(capturedRequest{
			Method:        r.Method,
			Path:          r.URL.Path,
			RawQuery:      r.URL.RawQuery,
			Authorization: r.Header.Get("Authorization"),
			ContentType:   r.Header.Get("Content-Type"),
			Header:        r.Header.Clone(),
			Body:          body,
		})
		respond(w, r, body)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func jsonResponder(status int, body string) func(http.ResponseWriter, *http.Request, []byte) {
	return func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func newTestGateway(t *testing.T, opts Options) *Gateway {
	t.Helper()
	if opts.Keys == nil {
		opts.Keys = newFakeKeys()
	}
	// config.Default() ships LegacyAFallback=true, so a gateway built from the
	// shipped configuration still answers bare model names through A. Mirror
	// that here: the pre-plugin suites assert the legacy contract, and a test
	// about the default-plugin slot builds its gateway with newGateway directly
	// so it can control DefaultPlugin/LegacyA itself.
	opts.LegacyA = true
	g := newGateway(opts)
	// Retry backoffs are wall-clock waits in production; tests only care about
	// the decision sequence, so the default is instant. Individual tests
	// replace g.sleep with a recorder when the delays themselves matter.
	g.sleep = func(context.Context, time.Duration) bool { return true }
	t.Cleanup(g.CloseIdleConnections)
	return g
}

func newChatRequest(t *testing.T, path, key, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	return req
}

func newGetRequest(t *testing.T, path, key string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	return req
}

func serve(g http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	return rec
}

type errorResponse struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) errorResponse {
	t.Helper()
	var payload errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("error body is not JSON: %v (body %q)", err, rec.Body.String())
	}
	if payload.Error.Type == "" || payload.Error.Code == "" || payload.Error.Message == "" {
		t.Fatalf("incomplete error body: %+v (raw %q)", payload, rec.Body.String())
	}
	return payload
}

// deadServerURL returns a URL that used to be an httptest server and is now
// closed, so connections are refused immediately.
func deadServerURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	return url
}

// fakeClock returns a clock advancing by step on every call.
func fakeClock(base time.Time, step time.Duration) func() time.Time {
	var mu sync.Mutex
	n := 0
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		n++
		return base.Add(time.Duration(n-1) * step)
	}
}

// fillerReader produces an endless run of 'x' bytes for oversize-body tests.
type fillerReader struct{}

func (fillerReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}
