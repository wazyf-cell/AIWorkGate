package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aiworkgate/internal/model"
	"aiworkgate/internal/store"
)

// pluginBadGatewayBody is the measured opencode reply the P0 is about.
const pluginBadGatewayBody = `{"error":{"code":"upstream_error","message":"upstream request failed: Post \"https://opencode.ai/inference/v1/chat/completions\": Bad Gateway"}}`

const pluginBadGatewayMessage = `upstream request failed: Post "https://opencode.ai/inference/v1/chat/completions": Bad Gateway`

// fakeEgressHintSource is the read side of the store the hint consults.
type fakeEgressHintSource struct {
	egress     []model.AccountEgress
	egressErr  error
	failure    *model.ProxyLog
	failureErr error
	delay      time.Duration
}

func (f *fakeEgressHintSource) ListAccountEgress(string) ([]model.AccountEgress, error) {
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if f.egressErr != nil {
		return nil, f.egressErr
	}
	return f.egress, nil
}

func (f *fakeEgressHintSource) LatestProxyFailure(string, string, int64) (*model.ProxyLog, error) {
	if f.failureErr != nil {
		return nil, f.failureErr
	}
	return f.failure, nil
}

func useEgressHintSource(t *testing.T, src EgressHintSource) {
	t.Helper()
	InstallEgressHintSource(src)
	t.Cleanup(func() { InstallEgressHintSource(nil) })
}

func proxyFailedEgress() []model.AccountEgress {
	return []model.AccountEgress{{
		PluginID: "opencode", AccountID: "default", Mode: "proxy", ProxyID: "px_1074ee85",
	}}
}

func writeBadGateway(t *testing.T, g *Gateway, rec *reqRecord) *httptest.ResponseRecorder {
	t.Helper()
	res := &upstreamAttemptResult{
		status: http.StatusBadGateway,
		header: http.Header{"Content-Type": []string{"application/json"}},
		body:   []byte(pluginBadGatewayBody),
	}
	w := httptest.NewRecorder()
	g.writeAttempt(w, rec, res)
	return w
}

type decodedError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// (a) proxy egress + a failed tunnel row in the window: one clause is appended,
// while status, code and shape stay untouched.
func TestWriteAttemptAppendsProxyFailureClause(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.Local)
	useEgressHintSource(t, &fakeEgressHintSource{
		egress: proxyFailedEgress(),
		failure: &model.ProxyLog{
			At: now.Add(-90 * time.Second).Unix(), ProxyID: "px_1074ee85",
			ProxyLabel: "香港 01", Result: model.ProxyResultFailed,
			Error: `Post "https://opencode.ai/inference/v1/chat/completions": Bad Gateway`,
		},
	})
	g := &Gateway{now: func() time.Time { return now }}
	rec := &reqRecord{source: "plugin:opencode", account: "default"}

	w := writeBadGateway(t, g, rec)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (the fix must not change it)", w.Code)
	}
	var got decodedError
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not the original JSON shape: %v: %s", err, w.Body.String())
	}
	if got.Error.Code != "upstream_error" {
		t.Fatalf("code = %q, want upstream_error", got.Error.Code)
	}
	want := pluginBadGatewayMessage +
		`；该账号经代理「香港 01」出网，最近一次隧道失败：Post "https://opencode.ai/inference/v1/chat/completions": Bad Gateway（1:30 前）`
	if got.Error.Message != want {
		t.Fatalf("message =\n %q\nwant\n %q", got.Error.Message, want)
	}
}

// (b) no failed row: the buffered body must stay byte-identical.
func TestWriteAttemptKeepsBodyWithoutProxyFailure(t *testing.T) {
	useEgressHintSource(t, &fakeEgressHintSource{egress: proxyFailedEgress()})
	g := &Gateway{now: time.Now}
	rec := &reqRecord{source: "plugin:opencode", account: "default"}

	w := writeBadGateway(t, g, rec)

	if got := w.Body.String(); got != pluginBadGatewayBody {
		t.Fatalf("body changed without a failed row:\n got %s\nwant %s", got, pluginBadGatewayBody)
	}
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
}

// (c) a broken store lookup must degrade to the original message, never add a
// new failure mode.
func TestWriteAttemptKeepsBodyWhenStoreErrors(t *testing.T) {
	useEgressHintSource(t, &fakeEgressHintSource{egressErr: errors.New("database is locked")})
	g := &Gateway{now: time.Now}
	rec := &reqRecord{source: "plugin:opencode", account: "default"}

	w := writeBadGateway(t, g, rec)

	if got := w.Body.String(); got != pluginBadGatewayBody {
		t.Fatalf("body changed after a store error:\n got %s\nwant %s", got, pluginBadGatewayBody)
	}
}

// A slow store is the same story: answer with the original message.
func TestWriteAttemptKeepsBodyWhenStoreIsSlow(t *testing.T) {
	old := egressHintTimeout
	egressHintTimeout = 20 * time.Millisecond
	t.Cleanup(func() { egressHintTimeout = old })

	useEgressHintSource(t, &fakeEgressHintSource{
		egress:  proxyFailedEgress(),
		delay:   200 * time.Millisecond,
		failure: &model.ProxyLog{At: time.Now().Unix(), ProxyLabel: "香港 01", Error: "refused"},
	})
	g := &Gateway{now: time.Now}
	rec := &reqRecord{source: "plugin:opencode", account: "default"}

	w := writeBadGateway(t, g, rec)

	if got := w.Body.String(); got != pluginBadGatewayBody {
		t.Fatalf("body changed after a slow store:\n got %s\nwant %s", got, pluginBadGatewayBody)
	}
}

// Direct egress gets no proxy clause even when an old failed row exists.
func TestWriteAttemptSkipsClauseForDirectEgress(t *testing.T) {
	useEgressHintSource(t, &fakeEgressHintSource{
		egress:  []model.AccountEgress{{PluginID: "opencode", AccountID: "default", Mode: "direct"}},
		failure: &model.ProxyLog{At: time.Now().Unix(), ProxyLabel: "香港 01", Error: "refused"},
	})
	g := &Gateway{now: time.Now}
	rec := &reqRecord{source: "plugin:opencode", account: "default"}

	w := writeBadGateway(t, g, rec)

	if got := w.Body.String(); got != pluginBadGatewayBody {
		t.Fatalf("body changed for direct egress:\n got %s\nwant %s", got, pluginBadGatewayBody)
	}
}

// Non-5xx replies are never touched, and a nil gateway/store stays safe.
func TestWriteAttemptSkipsClauseOutside5xxAndWhenUnwired(t *testing.T) {
	useEgressHintSource(t, &fakeEgressHintSource{
		egress:  proxyFailedEgress(),
		failure: &model.ProxyLog{At: time.Now().Unix(), ProxyLabel: "香港 01", Error: "refused"},
	})
	g := &Gateway{now: time.Now}
	rec := &reqRecord{source: "plugin:opencode", account: "default"}
	res := &upstreamAttemptResult{status: http.StatusBadRequest, header: http.Header{}, body: []byte(`{"error":{"message":"bad"}}`)}
	w := httptest.NewRecorder()
	g.writeAttempt(w, rec, res)
	if got := w.Body.String(); got != `{"error":{"message":"bad"}}` {
		t.Fatalf("400 body changed: %s", got)
	}

	// An unwired gateway (no source) keeps the measured 502 as-is.
	InstallEgressHintSource(nil)
	InstallKeyUsageReserver(nil)
	w = writeBadGateway(t, &Gateway{now: time.Now}, rec)
	if got := w.Body.String(); got != pluginBadGatewayBody {
		t.Fatalf("unwired gateway body changed: %s", got)
	}
}

// The default source is the store the host already installed as the quota
// reserver (app.go: gateway.InstallKeyUsageReserver(a.store)); no new host
// wiring is required for the clause to work in production.
func TestEgressHintFallsBackToInstalledStore(t *testing.T) {
	st, err := store.Open(testTempDir(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	InstallKeyUsageReserver(st)
	t.Cleanup(func() { InstallKeyUsageReserver(nil) })
	InstallEgressHintSource(nil)

	if err := st.SetAccountEgress(model.AccountEgress{
		PluginID: "opencode", AccountID: "default", Mode: "proxy", ProxyID: "px_1074ee85",
	}); err != nil {
		t.Fatalf("SetAccountEgress: %v", err)
	}
	if _, err := st.InsertProxyLog(model.ProxyLog{
		At: time.Now().Add(-time.Minute).Unix(), PluginID: "opencode", AccountID: "default",
		ProxyID: "px_1074ee85", ProxyLabel: "香港 01", TargetHost: "opencode.ai",
		TargetPort: 443, Result: model.ProxyResultFailed, Status: 502,
		Error: "dial tcp 127.0.0.1:7897: connectex: refused",
	}); err != nil {
		t.Fatalf("InsertProxyLog: %v", err)
	}

	rec := &reqRecord{source: "plugin:opencode", account: "default"}
	w := writeBadGateway(t, &Gateway{now: time.Now}, rec)
	var got decodedError
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("body: %v", err)
	}
	if !strings.Contains(got.Error.Message, "该账号经代理「香港 01」出网") {
		t.Fatalf("message = %q, want the proxy clause from the installed store", got.Error.Message)
	}
}

// Full dispatch path: a scheduled plugin 502 ends in writeAttempt, so the
// clause reaches the client there too.
func TestScheduledBadGatewayCarriesProxyClause(t *testing.T) {
	srv, _ := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, pluginBadGatewayBody)
	})
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	g := scheduledGateway(t, reg, &fakeLogSink{}, singlePoolSnapshot("p1", 2, "acc1"))
	useEgressHintSource(t, &fakeEgressHintSource{
		egress:  []model.AccountEgress{{PluginID: "p1", AccountID: "acc1", Mode: "proxy"}},
		failure: &model.ProxyLog{At: time.Now().Unix(), ProxyLabel: "香港 01", Error: "refused"},
	})

	w := httptest.NewRecorder()
	g.ServeHTTP(w, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p1/m1","messages":[{"role":"user","content":"hi"}]}`))

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %s", w.Code, w.Body.String())
	}
	var got decodedError
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("body: %v: %s", err, w.Body.String())
	}
	if got.Error.Code != "upstream_error" {
		t.Fatalf("code = %q, want upstream_error", got.Error.Code)
	}
	if !strings.Contains(got.Error.Message, "最近一次隧道失败：refused") {
		t.Fatalf("message = %q, want the tunnel clause", got.Error.Message)
	}
}

// The clause itself: label falls back to the proxy id, credentials never leak,
// and the whole hint stays within 160 runes.
func TestBuildEgressHintFormatting(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.Local)
	row := &model.ProxyLog{
		At: now.Add(-5*time.Minute - 7*time.Second).Unix(), ProxyID: "px_1074ee85",
		Error: "http://alice:s3cret@127.0.0.1:7897 " + strings.Repeat("very long tunnel error ", 20),
	}
	got := buildEgressHint(row, now)
	if got == "" {
		t.Fatal("hint is empty")
	}
	if !strings.HasPrefix(got, "；该账号经代理「px_1074ee85」出网，最近一次隧道失败：") {
		t.Fatalf("hint = %q, want the proxy id as label", got)
	}
	if !strings.HasSuffix(got, "（5:07 前）") {
		t.Fatalf("hint = %q, want the m:ss age suffix", got)
	}
	if strings.Contains(got, "s3cret") || strings.Contains(got, "alice") {
		t.Fatalf("hint leaks credentials: %q", got)
	}
	if n := len([]rune(got)); n > 160 {
		t.Fatalf("hint has %d runes, want <= 160: %q", n, got)
	}
	if got := buildEgressHint(nil, now); got != "" {
		t.Fatalf("nil row hint = %q, want empty", got)
	}
}
