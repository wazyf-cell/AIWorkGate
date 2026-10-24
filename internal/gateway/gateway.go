// Package gateway implements the AIWorkGate local API gateway (M3): client key
// authentication, the merged model registry, routing between local plugins and
// the AI Work 助手 (A) gateway, byte-for-byte SSE passthrough, per-plugin
// concurrency gates and API request logging.
//
// The package is deliberately decoupled from the concrete host runtime: it
// depends only on the small interfaces below (plus internal/model), so it can
// be built and tested without importing internal/plugins or internal/hosts.
package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"aiworkgate/internal/model"
	"aiworkgate/internal/scheduler"
)

// PluginRegistry is the host-side view of installed plugins. It mirrors the
// runtime that owns plugin processes, ports and tokens.
type PluginRegistry interface {
	List() []model.PluginStatus
	Endpoint(pluginID string) (baseURL, token string, ok bool)
	EnsureRunning(ctx context.Context, pluginID string) error
	Concurrency(pluginID string) int
	Touch(pluginID string)
}

// AUpstream is the AI Work 助手 built-in OpenAI-compatible gateway.
type AUpstream interface {
	BaseURL() string
	APIKey() string
	Models(ctx context.Context) ([]model.ModelInfo, error)
}

// SchedLogger is the slice of the host logger used for scheduler events.
type SchedLogger interface {
	Info(module, msg string, fields map[string]any)
	Warn(module, msg string, fields map[string]any)
}

// KeyVerifier checks client API keys (agk_...) against stored hashes.
type KeyVerifier interface {
	Verify(plain string) (id, name string, ok bool)
	Touch(id string)
}

// APILogSink receives one row per proxied request.
type APILogSink interface {
	Write(model.APIRequestLog)
}

// Options wires the gateway. Only Plugins is strictly required for routing;
// A, Keys and Log may be nil (which yields 502, 401 and no logging).
type Options struct {
	Plugins PluginRegistry
	A       AUpstream
	Keys    KeyVerifier
	Log     APILogSink

	// Schedule owns account-level routing (design §6). Snapshot supplies the
	// immutable per-request view it plans against. Both may be nil, in which
	// case plugin models are forwarded without account pinning (v0.2 behaviour).
	Schedule *scheduler.Scheduler
	Snapshot func(context.Context) (*scheduler.Snapshot, error)
	// SchedLog records each routing decision as a "scheduler" event.
	SchedLog SchedLogger

	// Overrides supplies the human metadata override layer for one plugin,
	// keyed by model id. Nil means "no override layer" (tests, standalone).
	Overrides func(pluginID string) (map[string]ModelOverride, error)

	// CustomModels supplies the user-defined OpenAI-compatible upstreams
	// (Settings → 自定义模型). CustomModelKey returns the DPAPI-stored upstream
	// key of one entry; the key is never part of CustomModel. Both nil disable
	// the feature (tests, standalone).
	CustomModels   func() ([]CustomModel, error)
	CustomModelKey func(id string) (string, bool)

	// KeyRecord supplies the scope/quota view of a verified key (plan
	// 2026-10-21 §1). Nil means every key is unrestricted (tests).
	KeyRecord func(keyID string) (KeyRecord, bool, error)
	// KeyUsageToday returns a key's request count for one local-calendar day.
	// It backs the quota gate; errors fail open so a database hiccup never
	// blocks traffic.
	KeyUsageToday func(keyID, day string) (int, error)
	// KeyUsage accumulates one finished request for a key. Fire-and-forget:
	// accounting never changes the response the client already received.
	KeyUsage func(keyID, day string, requests, promptTokens, completionTokens, cachedTokens int, credits *float64)

	// DebugBody enables request/response preview capture (redacted by the
	// sink). Request previews are capped at 4KB, responses at BodyCap.
	DebugBody bool
	// BodyCap caps the response preview bytes kept for logging. Default 8KB.
	BodyCap int
	// QueueTimeout is how long a chat request waits for a plugin slot before
	// receiving 429 concurrency_limit. Default 30s.
	QueueTimeout time.Duration
	// AModelCacheTTL is how long A's model list is reused. Default 10m.
	AModelCacheTTL time.Duration
	// CorsAllowOrigins lists browser origins allowed to read gateway
	// responses. Empty keeps the gateway closed to cross-origin JS.
	CorsAllowOrigins []string

	// DefaultPlugin serves bare model names through the generic plugin path.
	// Empty means no default; see LegacyA.
	DefaultPlugin string
	// LegacyA keeps the A upstream as the bare-name fallback for compatibility.
	LegacyA bool
}

const (
	maxRequestBodyBytes = 32 << 20
	requestPreviewBytes = 4096
	defaultBodyCap      = 8192
	defaultQueueTimeout = 30 * time.Second
	defaultCacheTTL     = 10 * time.Minute
	defaultConcurrency  = 2
	modelsFetchTimeout  = 5 * time.Second
	upstreamDialTimeout = 10 * time.Second
	idleConnTimeout     = 90 * time.Second

	sourceHeader = "X-AIWorkGate-Source"
	staleHeader  = "X-AIWorkGate-A-Stale"

	// aDownMessage is the user-facing message required by the contract.
	aDownMessage = "AI Work 助手未运行"
)

// Upstream liveness bounds. They are variables so tests can shorten them.
//   - responseHeaderTimeout covers an upstream that accepts the connection and
//     never sends headers (the client would otherwise pin a concurrency slot
//     and scheduler lease forever).
//   - idleReadTimeout covers a reply that stalled after the headers, e.g. a
//     silent SSE stream: every body Read rearms the timer and a fired timer
//     cancels the request context.
var (
	upstreamResponseHeaderTimeout = 90 * time.Second
	upstreamIdleReadTimeout       = 5 * time.Minute
)

// Gateway is the http.Handler returned by New.
type Gateway struct {
	plugins        PluginRegistry
	a              AUpstream
	keys           KeyVerifier
	logs           APILogSink
	sched          *scheduler.Scheduler
	snapshot       func(context.Context) (*scheduler.Snapshot, error)
	schedLog       SchedLogger
	overrides      func(pluginID string) (map[string]ModelOverride, error)
	customModels   func() ([]CustomModel, error)
	customModelKey func(id string) (string, bool)
	keyRecord      func(keyID string) (KeyRecord, bool, error)
	keyUsageToday  func(keyID, day string) (int, error)
	keyUsage       func(keyID, day string, requests, promptTokens, completionTokens, cachedTokens int, credits *float64)
	debugBody      atomic.Bool
	bodyCap        int
	queueTimeout   time.Duration
	cacheTTL       time.Duration
	corsOrigins    atomic.Value // []string, set by SetCorsOrigins
	defaultPlugin  string
	legacyA        bool
	now            func() time.Time
	// sleep waits for one retry backoff and reports whether the request may
	// continue. Tests inject a recorder so 10/20/40s waits stay instant.
	sleep func(context.Context, time.Duration) bool

	client    *http.Client
	transport *http.Transport
	lim       *limiter

	aMu      sync.Mutex
	aModels  []model.ModelInfo
	aFetched time.Time
	aHave    bool

	pMu    sync.Mutex
	pCache map[string]pluginModels
}

type pluginModels struct {
	models []model.ModelInfo
	known  bool
	until  time.Time
}

// SetDebug toggles request/response preview capture at runtime.
func (g *Gateway) SetDebug(on bool) { g.debugBody.Store(on) }

// SetCorsOrigins replaces the browser origins allowed to read /v1 responses.
// It is safe to call while the gateway serves traffic.
func (g *Gateway) SetCorsOrigins(origins []string) {
	if g == nil {
		return
	}
	g.corsOrigins.Store(append([]string(nil), origins...))
}

func (g *Gateway) corsList() []string {
	if g == nil {
		return nil
	}
	if list, ok := g.corsOrigins.Load().([]string); ok {
		return list
	}
	return nil
}

// New builds the gateway handler.
func New(opts Options) http.Handler {
	return newGateway(opts)
}

func newGateway(opts Options) *Gateway {
	bodyCap := opts.BodyCap
	if bodyCap <= 0 {
		bodyCap = defaultBodyCap
	}
	queueTimeout := opts.QueueTimeout
	if queueTimeout <= 0 {
		queueTimeout = defaultQueueTimeout
	}
	cacheTTL := opts.AModelCacheTTL
	if cacheTTL <= 0 {
		cacheTTL = defaultCacheTTL
	}

	dialer := &net.Dialer{Timeout: upstreamDialTimeout, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:                 nil, // A and plugins are local; never use env proxies
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       idleConnTimeout,
		TLSHandshakeTimeout:   upstreamDialTimeout,
		ExpectContinueTimeout: time.Second,
		// A plugin that accepts TCP but never answers must not pin a slot.
		ResponseHeaderTimeout: upstreamResponseHeaderTimeout,
		// Keep bodies verbatim: no transparent gzip, no decompression.
		DisableCompression: true,
	}

	g := &Gateway{
		plugins:        opts.Plugins,
		a:              opts.A,
		keys:           opts.Keys,
		logs:           opts.Log,
		sched:          opts.Schedule,
		snapshot:       opts.Snapshot,
		schedLog:       opts.SchedLog,
		overrides:      opts.Overrides,
		customModels:   opts.CustomModels,
		customModelKey: opts.CustomModelKey,
		keyRecord:      opts.KeyRecord,
		keyUsageToday:  opts.KeyUsageToday,
		keyUsage:       opts.KeyUsage,
		bodyCap:        bodyCap,
		queueTimeout:   queueTimeout,
		cacheTTL:       cacheTTL,
		defaultPlugin:  strings.ToLower(strings.TrimSpace(opts.DefaultPlugin)),
		legacyA:        opts.LegacyA,
		now:            time.Now,
		sleep:          sleepContext,
		client:         &http.Client{Transport: transport, CheckRedirect: doNotFollowRedirects},
		transport:      transport,
		lim:            newLimiter(),
		pCache:         make(map[string]pluginModels),
	}
	g.debugBody.Store(opts.DebugBody)
	g.SetCorsOrigins(opts.CorsAllowOrigins)
	return g
}

// doNotFollowRedirects keeps the gateway transparent: a 3xx from A or a plugin
// is passed through to the client instead of being replayed (which would also
// hand the upstream credential to the redirect target).
func doNotFollowRedirects(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// sleepContext waits for a retry backoff but aborts as soon as the client goes
// away. It returns false when the request context ended during the wait.
func sleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// CloseIdleConnections releases pooled upstream connections (called on host
// shutdown).
func (g *Gateway) CloseIdleConnections() {
	if g != nil && g.transport != nil {
		g.transport.CloseIdleConnections()
	}
}

// reqRecord accumulates what the request log needs while a request runs.
type reqRecord struct {
	reqID          string
	start          time.Time
	source         string
	model          string
	logicalModel   string
	account        string
	conversationID string
	keyID          string
	keyName        string
	stream         bool
	status         int
	ttfb           time.Duration
	firstTokenAt   time.Time
	prompt         int64
	completion     int64
	cached         int64
	credits        *float64
	downgraded     string
	errText        string
	bodyReq        string
	bodyResp       string
	// billable marks a /v1 inference request that consumes the client key's
	// daily quota; quotaRejected marks the request refused by that gate, which
	// is not counted again (otherwise a blocked key keeps growing its counter).
	billable      bool
	quotaRejected bool
	// scopeRejected marks a request refused by the key's model scope / pool
	// binding. It never reached a model, so it must not consume the quota.
	scopeRejected bool
}

// sseKeepAliveInterval is how long a streaming reply may stay silent before the
// gateway writes an SSE comment. A variable so tests can shorten it.
var sseKeepAliveInterval = 15 * time.Second

// sseKeepAliveWriter writes ": keep-alive\n\n" into an idle text/event-stream
// response so intermediaries do not recycle a long, quiet stream (F-34). It only
// writes when the previous write ended at a frame boundary (last byte '\n'), so
// a comment can never split an event, and it flushes right after writing.
// Ported from A (TraeWorkAssistant, MIT, 朱天伟) wb_route.rs:619-635.
type sseKeepAliveWriter struct {
	http.ResponseWriter
	interval time.Duration

	mu        sync.Mutex
	started   bool
	streaming bool
	boundary  bool
	last      time.Time

	stopOnce sync.Once
	done     chan struct{}
	wg       sync.WaitGroup
}

func newSSEKeepAliveWriter(w http.ResponseWriter) *sseKeepAliveWriter {
	return &sseKeepAliveWriter{
		ResponseWriter: w,
		interval:       sseKeepAliveInterval,
		done:           make(chan struct{}),
	}
}

func (k *sseKeepAliveWriter) WriteHeader(code int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.started {
		return
	}
	k.started = true
	k.armLocked()
	k.ResponseWriter.WriteHeader(code)
}

func (k *sseKeepAliveWriter) Write(p []byte) (int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.started {
		k.started = true
		k.armLocked()
	}
	n, err := k.ResponseWriter.Write(p)
	if n > 0 {
		k.last = time.Now()
		k.boundary = p[n-1] == '\n'
	}
	return n, err
}

// Flush exposes http.Flusher so the copy loops keep flushing unchanged.
func (k *sseKeepAliveWriter) Flush() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if f, ok := k.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// armLocked starts the idle watchdog once, and only for SSE replies: a JSON
// body has no comment syntax and never needs one.
func (k *sseKeepAliveWriter) armLocked() {
	if k.streaming || k.interval <= 0 {
		return
	}
	if !strings.Contains(strings.ToLower(k.Header().Get("Content-Type")), "text/event-stream") {
		return
	}
	k.streaming = true
	k.boundary = true
	k.last = time.Now()
	k.wg.Add(1)
	go k.run()
}

func (k *sseKeepAliveWriter) run() {
	defer k.wg.Done()
	tick := k.interval / 5
	if tick < 10*time.Millisecond {
		tick = 10 * time.Millisecond
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-k.done:
			return
		case <-ticker.C:
			k.keepAlive()
		}
	}
}

// keepAlive writes one comment frame when the stream has been quiet for the
// whole interval and the last write ended a line.
func (k *sseKeepAliveWriter) keepAlive() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.streaming || !k.boundary || time.Since(k.last) < k.interval {
		return
	}
	if _, err := io.WriteString(k.ResponseWriter, ": keep-alive\n\n"); err != nil {
		return
	}
	k.last = time.Now()
	if f, ok := k.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// stop ends the watchdog before the handler returns, so nothing is written
// after the response is finalized.
func (k *sseKeepAliveWriter) stop() {
	k.stopOnce.Do(func() { close(k.done) })
	k.wg.Wait()
}

// ServeHTTP implements http.Handler.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Every streaming reply goes through the keep-alive wrapper, both the
	// byte-for-byte passthrough and the converted (Messages/Responses) writers.
	keep := newSSEKeepAliveWriter(w)
	defer keep.stop()
	w = keep

	rec := &reqRecord{reqID: newReqID(), start: g.now()}
	defer func() {
		latency := g.now().Sub(rec.start)
		g.emit(rec, latency)
		g.recordKeyUsage(rec)
	}()

	path := r.URL.Path
	if path != "/v1" && !strings.HasPrefix(path, "/v1/") {
		g.fail(w, rec, http.StatusNotFound, "not_found_error", "not_found",
			"unknown path "+path)
		return
	}

	if allowed, origin := g.corsFor(r.Header.Get("Origin")); origin != "" && allowed {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", origin)
		h.Add("Vary", "Origin")
		h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		h.Set("Access-Control-Allow-Headers", strings.Join(corsAllowHeaders, ", "))
		h.Set("Access-Control-Expose-Headers", strings.Join(corsExposeHeaders, ", "))
		h.Set("Access-Control-Max-Age", "600")
		if r.Method == http.MethodOptions {
			rec.status = http.StatusNoContent
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}

	if r.Method == http.MethodOptions {
		// Preflight from an origin we do not allow: answer without granting it.
		rec.status = http.StatusMethodNotAllowed
		w.Header().Set("Allow", "GET, POST")
		g.fail(w, rec, http.StatusMethodNotAllowed, "invalid_request_error", "cors_not_allowed",
			"跨源访问未开启：请在「设置」里把该来源加入 CORS 允许列表")
		return
	}

	keyID, keyName, ok := g.authenticate(r)
	if !ok {
		rec.errText = "missing or invalid API key"
		g.fail(w, rec, http.StatusUnauthorized, "authentication_error", "invalid_api_key",
			"missing or invalid API key")
		return
	}
	rec.keyID, rec.keyName = keyID, keyName
	// The scope snapshot travels with the request context (plan 2026-10-21 §1)
	// so the routing helpers never touch the store themselves.
	if kr, found := g.keyRecordFor(keyID); found {
		if !kr.Enabled {
			rec.errText = "client key disabled"
			g.fail(w, rec, http.StatusUnauthorized, "authentication_error", "invalid_api_key",
				"该客户端 Key 已停用，请在宿主的「设置 → 客户端 Key」中重新启用")
			return
		}
		r = r.WithContext(withKeyRecord(r.Context(), kr))
	}
	rec.billable = isInferencePath(path)
	if !g.keyQuotaAllows(w, r, rec) {
		return
	}
	// The source header is only knowable after routing, so the forwarders and
	// fail() stamp it; the CORS block above cannot.
	g.route(w, r, rec)
}

var (
	corsAllowHeaders = []string{
		"Authorization", "Content-Type", "Accept", "X-API-Key", "api-key",
		"X-Client-Key", "X-Conversation-Id", "X-Aiworkgate-Account", "X-Aiworkgate-Request-Id",
	}
	corsExposeHeaders = []string{
		sourceHeader, staleHeader, "X-Aiworkgate-Account", "X-Aiworkgate-Credits",
		"X-Aiworkgate-Account-Status", "X-Aiworkgate-Cooldown", "X-Aiworkgate-Downgraded",
	}
)

// corsFor decides whether an Origin may read gateway responses. Only origins
// explicitly listed in config (or "*" when the operator asked for it) are
// granted; a loopback gateway is still reachable by any local page, so CORS is
// opt-in rather than on by default.
func (g *Gateway) corsFor(origin string) (allowed bool, echo string) {
	if strings.TrimSpace(origin) == "" {
		return false, ""
	}
	for _, candidate := range g.corsList() {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if candidate == "*" {
			return true, origin
		}
		if strings.EqualFold(strings.TrimRight(candidate, "/"), strings.TrimRight(origin, "/")) {
			return true, origin
		}
	}
	return false, ""
}

func (g *Gateway) authenticate(r *http.Request) (id, name string, ok bool) {
	token := clientToken(r)
	if token == "" || g.keys == nil {
		return "", "", false
	}
	id, name, ok = g.keys.Verify(token)
	if !ok {
		return "", "", false
	}
	g.keys.Touch(id)
	return id, name, true
}

// clientToken accepts the OpenAI style bearer token plus the header spellings
// the Anthropic and Azure SDKs send by default, so those clients can point at
// the gateway without a translation proxy in front of it.
func clientToken(r *http.Request) string {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix = "bearer "
	if len(h) >= len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	if v := strings.TrimSpace(r.Header.Get("x-api-key")); v != "" {
		return v
	}
	if v := strings.TrimSpace(r.Header.Get("api-key")); v != "" {
		return v
	}
	return ""
}

func (g *Gateway) route(w http.ResponseWriter, r *http.Request, rec *reqRecord) {
	path := r.URL.Path
	// Custom models are checked before the scheduler/plugin/A routing: a
	// matching enabled entry sends the request straight to its own upstream
	// (A dispatch.rs:405-425 ⓪). The hook only claims requests it fully handles.
	if g.customModels != nil && r.Method == http.MethodPost {
		if g.handleCustomModel(w, r, rec) {
			return
		}
	}
	switch {
	case r.Method == http.MethodGet && path == "/v1/models":
		if g.customModels != nil {
			g.handleModelsWithCustom(w, r, rec)
			return
		}
		g.handleModelsForKey(w, r, rec)
	case r.Method == http.MethodPost && path == "/v1/chat/completions":
		g.handleChat(w, r, rec)
	case r.Method == http.MethodPost && path == "/v1/completions":
		g.handleEnvelope(w, r, rec, envelopeCompletions)
	case r.Method == http.MethodPost && path == "/v1/responses":
		g.handleEnvelope(w, r, rec, envelopeResponses)
	case r.Method == http.MethodPost && path == "/v1/messages":
		// handleMessages owns its body parsing (anthropic.go), so the key's
		// scope/binding rules are applied here on the buffered body.
		if !g.prepareMessagesForKey(w, r, rec) {
			return
		}
		g.handleMessages(w, r, rec)
	case r.Method == http.MethodPost && isPassThroughPath(path):
		g.handlePassThrough(w, r, rec)
	default:
		g.fail(w, rec, http.StatusNotFound, "not_found_error", "not_found",
			"unknown endpoint "+path)
	}
}

// isPassThroughPath lists the non-chat endpoints A accepts unchanged.
// /v1/messages is not here: it is translated for plugin models and forwards
// bare model names from handleMessages.
func isPassThroughPath(path string) bool {
	switch path {
	case "/v1/embeddings", "/v1/images", "/v1/images/":
		return true
	}
	return strings.HasPrefix(path, "/v1/images/")
}

// fail writes a unified error body and records it for logging.
func (g *Gateway) fail(w http.ResponseWriter, rec *reqRecord, status int, typ, code, msg string) {
	rec.status = status
	if rec.errText == "" {
		rec.errText = msg
	}
	if rec.source != "" {
		w.Header().Set(sourceHeader, rec.source)
	}
	writeError(w, status, typ, code, msg)
}

type errorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

func writeError(w http.ResponseWriter, status int, typ, code, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorEnvelope{Error: errorBody{
		Message: msg,
		Type:    typ,
		Code:    code,
	}})
}

// readRequestBody buffers the request body (cap 32MB). On failure it writes
// the error response and returns a non-zero HTTP status.
func readRequestBody(w http.ResponseWriter, r *http.Request) ([]byte, int) {
	if r.Body == nil {
		return nil, 0
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "read_error",
			"failed to read request body")
		return nil, http.StatusBadRequest
	}
	if len(body) > maxRequestBodyBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "body_too_large",
			"request body exceeds 32MB")
		return nil, http.StatusRequestEntityTooLarge
	}
	return body, 0
}

type requestMeta struct {
	model  string
	stream bool
	valid  bool
}

// parseRequestMeta extracts the top-level model and stream fields. It is
// tolerant: an unparsable body simply yields valid=false.
func parseRequestMeta(body []byte) requestMeta {
	if len(body) == 0 {
		return requestMeta{}
	}
	var probe struct {
		Model  json.RawMessage `json:"model"`
		Stream json.RawMessage `json:"stream"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return requestMeta{}
	}
	meta := requestMeta{valid: true}
	if len(probe.Model) > 0 {
		var s string
		if json.Unmarshal(probe.Model, &s) == nil {
			meta.model = s
		}
	}
	if len(probe.Stream) > 0 {
		var b bool
		if json.Unmarshal(probe.Stream, &b) == nil {
			meta.stream = b
		}
	}
	return meta
}

// stripDisabledThinking removes Anthropic's no-op `thinking:{"type":"disabled"}`
// from a chat body. The field is valid on /v1/messages but upstream chat APIs
// (e.g. ModelArts) reject it with a 400, so forwarding it breaks clients that
// send the explicit "disabled" form.
func stripDisabledThinking(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil {
		return body
	}
	raw, ok := top["thinking"]
	if !ok {
		return body
	}
	var shape struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &shape) != nil || !strings.EqualFold(strings.TrimSpace(shape.Type), "disabled") {
		return body
	}
	delete(top, "thinking")
	out, err := json.Marshal(top)
	if err != nil {
		return body
	}
	return out
}

// rewriteModel replaces the top-level model field, preserving every other
// field of the JSON body.
func rewriteModel(body []byte, modelID string) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(modelID)
	if err != nil {
		return nil, err
	}
	top["model"] = raw
	return json.Marshal(top)
}

type pluginTarget struct {
	status model.PluginStatus
	inner  string
}

// matchPlugin maps "<id>/<model>" to a plugin. Disabled plugins also match so
// that callers can return a precise 404/501 instead of forwarding the prefixed
// name to A.
func (g *Gateway) matchPlugin(name string) (pluginTarget, bool) {
	if name == "" || g.plugins == nil {
		return pluginTarget{}, false
	}
	for _, st := range g.plugins.List() {
		id := st.Manifest.ID
		if id == "" {
			continue
		}
		prefix := id + "/"
		if strings.HasPrefix(name, prefix) {
			return pluginTarget{status: st, inner: name[len(prefix):]}, true
		}
	}
	return pluginTarget{}, false
}

// pluginWithoutAccounts reports whether a plugin serves its own upstream and
// therefore needs no account routing.
//
// The scheduler treats every "<id>/<model>" hit as a schedulable candidate, so
// a plugin that forwards to a single built-in upstream (and has nothing to load
// balance across) publishes no accounts. Without this check the scheduler finds
// zero candidates and answers 503 ErrNoCandidate before the plugin is ever
// called, which makes such a plugin unusable no matter how correct it is.
//
// The condition is deliberately narrow: the plugin must declare the upstream
// capability, be present in the snapshot, and have an empty account list. A
// plugin that does publish accounts keeps the account-routing path, so quota,
// stickiness and failover are unchanged for every existing plugin.
func (g *Gateway) pluginWithoutAccounts(ctx context.Context, id string) bool {
	if g == nil || g.snapshot == nil || id == "" {
		return false
	}
	snap, err := g.snapshot(ctx)
	if err != nil || snap == nil {
		return false
	}
	pool, ok := snap.Pools[id]
	if !ok || pool == nil {
		return false
	}
	if !pool.UpstreamEnabled {
		return false
	}
	return len(pool.Accounts) == 0
}

// errNoDefaultRoute marks a bare model name that has nowhere to go: no default
// plugin is configured and the legacy A fallback is switched off. Callers turn
// it into 404 model_not_found.
var errNoDefaultRoute = errors.New("no default plugin configured")

// bareModelTarget decides what a bare (unprefixed) model name routes to.
// It returns the plugin status to use, or ok=false when the request must fail.
// This is the single place that knows about the default-plugin slot, so the
// old "every bare name goes to A" rule is gone.
//
// The three outcomes, in order:
//   - a default plugin is configured: it serves the bare name, and the plugin
//     target's inner model is the bare name itself (the prefix was not there).
//     A configured default that is not installed is an error: the caller
//     answers 503 plugin_unavailable instead of silently falling back to A.
//   - no default plugin and LegacyA: ok=false, err=nil, so the caller keeps the
//     pre-plugin A behaviour.
//   - no default plugin and no LegacyA: errNoDefaultRoute, so the bare name is a
//     404 without any code change.
func (g *Gateway) bareModelTarget(name string) (pluginTarget, bool, error) {
	if g == nil {
		return pluginTarget{}, false, errNoDefaultRoute
	}
	if pluginID := strings.TrimSpace(g.defaultPlugin); pluginID != "" {
		// An empty name would otherwise resolve to "<id>/" and be bounced back
		// here by the scheduler as errNotScheduled forever.
		if strings.TrimSpace(name) == "" {
			return pluginTarget{}, false, errNoDefaultRoute
		}
		if st, found := g.pluginStatus(pluginID); found {
			return pluginTarget{status: st, inner: name}, true, nil
		}
		return pluginTarget{}, false, fmt.Errorf("默认插件 %s 未安装或不可用", pluginID)
	}
	if g.legacyA {
		return pluginTarget{}, false, nil
	}
	return pluginTarget{}, false, errNoDefaultRoute
}

// handleChat implements POST /v1/chat/completions.
func (g *Gateway) handleChat(w http.ResponseWriter, r *http.Request, rec *reqRecord) {
	body, failStatus := readRequestBody(w, r)
	if failStatus != 0 {
		rec.status = failStatus
		return
	}
	meta := parseRequestMeta(body)
	if !meta.valid {
		rec.errText = "invalid JSON body"
		g.fail(w, rec, http.StatusBadRequest, "invalid_request_error", "invalid_json",
			"request body must be a JSON object")
		return
	}
	g.prepareRecord(rec, r, body, meta)
	body = stripDisabledThinking(body)

	// A key's model scope and pool binding decide the routed name before any
	// plugin/A routing happens (plan 2026-10-21 §1).
	routed, allowed := g.keyRouteModel(w, r, rec, meta.model)
	if !allowed {
		return
	}
	rec.model = routed

	if g.sched != nil && g.snapshot != nil {
		if tgt, ok := g.matchPlugin(routed); ok {
			rec.source = "plugin:" + tgt.status.Manifest.ID
			// A plugin that forwards to its own upstream publishes no accounts,
			// so the scheduler has nothing to pick and would answer 503 before
			// the plugin ever runs. Such a plugin is the whole routing decision
			// itself: forward directly, exactly like the no-scheduler path.
			if g.pluginWithoutAccounts(r.Context(), tgt.status.Manifest.ID) {
				g.forwardPlugin(w, r, rec, tgt, body, "/v1/chat/completions")
				return
			}
			g.forwardScheduled(w, r, rec, body, "/v1/chat/completions")
			return
		}
		if g.hasBinding(r.Context(), routed) {
			rec.source = scheduledSourceName
			g.forwardScheduled(w, r, rec, body, "/v1/chat/completions")
			return
		}
		// A bare name no binding owns: the default plugin may serve it. The
		// scheduled path resolves the "<id>/<model>" spelling itself, so the
		// request never bypasses account routing.
		tgt, ok, derr := g.bareModelTarget(routed)
		switch {
		case derr != nil && errors.Is(derr, errNoDefaultRoute):
			g.writeUnknownModel(w, rec, routed, "")
			return
		case derr != nil:
			g.fail(w, rec, http.StatusServiceUnavailable, "plugin_unavailable", "plugin_unavailable", derr.Error())
			return
		case ok:
			rec.source = "plugin:" + tgt.status.Manifest.ID
			// Same rule as a prefixed name: a plugin with no accounts cannot be
			// scheduled, so the default plugin forwards on its own.
			if g.pluginWithoutAccounts(r.Context(), tgt.status.Manifest.ID) {
				g.forwardPlugin(w, r, rec, tgt, body, "/v1/chat/completions")
				return
			}
			g.forwardScheduled(w, r, rec, body, "/v1/chat/completions")
			return
		default:
			g.forwardA(w, r, rec, body)
			return
		}
	}

	if tgt, ok := g.matchPlugin(routed); ok {
		rec.source = "plugin:" + tgt.status.Manifest.ID
		g.forwardPlugin(w, r, rec, tgt, body, "/v1/chat/completions")
		return
	}
	tgt, ok, derr := g.bareModelTarget(routed)
	switch {
	case derr != nil && errors.Is(derr, errNoDefaultRoute):
		g.writeUnknownModel(w, rec, routed, "")
		return
	case derr != nil:
		g.fail(w, rec, http.StatusServiceUnavailable, "plugin_unavailable", "plugin_unavailable", derr.Error())
		return
	case ok:
		rec.source = "plugin:" + tgt.status.Manifest.ID
		g.forwardPlugin(w, r, rec, tgt, body, "/v1/chat/completions")
		return
	default:
		g.forwardA(w, r, rec, body)
		return
	}
}

// handlePassThrough implements /v1/embeddings and /v1/images/*. These are
// billable inference endpoints, so the key's model scope/binding applies before
// anything is forwarded (a key scoped to one plugin model must not reach
// arbitrary A models). The daily-quota admission already ran in ServeHTTP;
// a scope rejection here releases that reservation via rec.scopeRejected.
func (g *Gateway) handlePassThrough(w http.ResponseWriter, r *http.Request, rec *reqRecord) {
	body, failStatus := readRequestBody(w, r)
	if failStatus != 0 {
		rec.status = failStatus
		return
	}
	meta := parseRequestMeta(body)
	g.prepareRecord(rec, r, body, meta)

	routed, allowed := g.keyRouteModel(w, r, rec, meta.model)
	if !allowed {
		return
	}
	rec.model = routed

	tgt, isPlugin := g.matchPlugin(rec.model)
	if !isPlugin {
		def, ok, derr := g.bareModelTarget(rec.model)
		switch {
		case derr != nil && errors.Is(derr, errNoDefaultRoute):
			g.writeUnknownModel(w, rec, rec.model, "")
			return
		case derr != nil:
			g.fail(w, rec, http.StatusServiceUnavailable, "plugin_unavailable", "plugin_unavailable", derr.Error())
			return
		case ok:
			tgt, isPlugin = def, true
		}
	}
	if isPlugin {
		rec.source = "plugin:" + tgt.status.Manifest.ID
		msg := fmt.Sprintf("插件模型暂不支持 %s，请改用 /v1/chat/completions", r.URL.Path)
		g.fail(w, rec, http.StatusNotImplemented, "invalid_request_error",
			"unsupported_endpoint", msg)
		return
	}
	g.forwardA(w, r, rec, body)
}

// chatSender is the narrow seam between protocol conversion and transport: it
// takes an already-converted chat/completions body and writes the reply into
// dst, which is either the client's ResponseWriter or one of the protocol
// adapters (anthropicWriter, envelopeWriter). The plugin/scheduler pipeline and
// a custom upstream each provide an implementation, so /v1/messages,
// /v1/responses and /v1/completions have exactly one converter per protocol.
type chatSender func(dst http.ResponseWriter, chatBody []byte)

// handleEnvelope serves /v1/completions and /v1/responses for plugin models by
// translating to chat/completions. Every other model keeps the plain A
// passthrough, so A's own semantics (including the fields this translation
// rejects) are unchanged.
func (g *Gateway) handleEnvelope(w http.ResponseWriter, r *http.Request, rec *reqRecord, kind envelopeKind) {
	body, failStatus := readRequestBody(w, r)
	if failStatus != 0 {
		rec.status = failStatus
		return
	}
	g.prepareRecord(rec, r, body, parseRequestMeta(body))

	routed, allowed := g.keyRouteModel(w, r, rec, rec.model)
	if !allowed {
		return
	}
	rec.model = routed

	scheduled := g.sched != nil && g.snapshot != nil
	tgt, isPlugin := g.matchPlugin(rec.model)
	// A bare name may be served by the configured default plugin. Folding the
	// resolution into the same tgt/isPlugin pair keeps every later step here
	// (the empty-inner 404, the translation and the scheduled forward) unchanged:
	// this function gains no default-plugin-specific branch of its own.
	if !isPlugin {
		def, ok, derr := g.bareModelTarget(rec.model)
		switch {
		case derr != nil && errors.Is(derr, errNoDefaultRoute):
			g.writeUnknownModel(w, rec, rec.model, "")
			return
		case derr != nil:
			g.fail(w, rec, http.StatusServiceUnavailable, "plugin_unavailable", "plugin_unavailable", derr.Error())
			return
		case ok:
			tgt, isPlugin = def, true
		}
	}
	bound := scheduled && !isPlugin && g.hasBinding(r.Context(), rec.model)
	if !isPlugin && !bound {
		g.forwardA(w, r, rec, body)
		return
	}
	// "<plugin>/" names no model. The scheduled path answers such a request with
	// errNotScheduled and would then hand A a chat body on the completions URL,
	// so the unknown model is rejected here.
	if isPlugin && tgt.inner == "" {
		g.writeUnknownModel(w, rec, rec.model, tgt.status.Manifest.ID)
		return
	}

	ew := newEnvelopeWriter(w, kind, rec.model, rec.stream)
	defer ew.finish()
	var send chatSender
	if scheduled {
		send = func(dst http.ResponseWriter, chatBody []byte) {
			if isPlugin {
				rec.source = "plugin:" + tgt.status.Manifest.ID
			} else {
				rec.source = scheduledSourceName
			}
			g.forwardScheduled(dst, r, rec, chatBody, "/v1/chat/completions")
		}
	} else {
		send = func(dst http.ResponseWriter, chatBody []byte) {
			rec.source = "plugin:" + tgt.status.Manifest.ID
			g.forwardPlugin(dst, r, rec, tgt, chatBody, "/v1/chat/completions")
		}
	}
	g.relayEnvelope(ew, rec, body, kind, send)
}

// prepareRecord fills the request-log fields derived from a buffered body.
func (g *Gateway) prepareRecord(rec *reqRecord, r *http.Request, body []byte, meta requestMeta) {
	rec.model = meta.model
	rec.stream = meta.stream
	rec.conversationID = conversationID(r, body)
	if g.debugBody.Load() {
		rec.bodyReq = string(body)
	}
}

// forwardPlugin ensures the plugin runs, checks the model against its
// registry, waits for a concurrency slot and proxies the chat request.
func (g *Gateway) forwardPlugin(w http.ResponseWriter, r *http.Request, rec *reqRecord, tgt pluginTarget, body []byte, path string) {
	id := tgt.status.Manifest.ID
	rec.source = "plugin:" + id

	// A long stream (or a slow cold start) must not be reaped by the plugin
	// manager's idle timer; the release runs after the body has been copied.
	if tracker, ok := g.plugins.(interface{ BeginCall(string) func() }); ok {
		release := tracker.BeginCall(id)
		defer release()
	}

	if tgt.inner == "" {
		g.writeUnknownModel(w, rec, rec.model, id)
		return
	}
	if err := g.plugins.EnsureRunning(r.Context(), id); err != nil {
		w.Header().Set("Retry-After", "5")
		g.fail(w, rec, http.StatusServiceUnavailable, "plugin_unavailable", "plugin_unavailable",
			fmt.Sprintf("插件 %s 无法启动: %v", id, err))
		return
	}
	baseURL, token, ok := g.plugins.Endpoint(id)
	if !ok || strings.TrimSpace(baseURL) == "" {
		w.Header().Set("Retry-After", "5")
		g.fail(w, rec, http.StatusServiceUnavailable, "plugin_unavailable", "plugin_unavailable",
			fmt.Sprintf("插件 %s 未运行", id))
		return
	}

	if mods, known := g.pluginModelList(r.Context(), tgt.status); known {
		found := false
		for _, m := range mods {
			if m.ID == tgt.inner {
				found = true
				break
			}
		}
		if !found {
			g.writeUnknownModel(w, rec, rec.model, id)
			return
		}
	}

	release, err := g.lim.acquire(r.Context(), id, g.plugins.Concurrency(id), g.queueTimeout)
	if err != nil {
		if errors.Is(err, errQueueTimeout) {
			g.fail(w, rec, http.StatusTooManyRequests, "concurrency_limit", "concurrency_limit",
				fmt.Sprintf("插件 %s 并发已满，排队超时", id))
			return
		}
		// The client went away while queuing; nothing sensible to write.
		rec.status = 499
		rec.errText = "client canceled while queuing"
		return
	}
	defer release()

	g.plugins.Touch(id)

	rewritten, err := rewriteModel(body, tgt.inner)
	if err != nil {
		g.fail(w, rec, http.StatusBadRequest, "invalid_request_error", "invalid_json",
			"request body must be a JSON object")
		return
	}

	target := strings.TrimRight(baseURL, "/") + path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	headers := forwardHeaders(r.Header)
	if token != "" {
		headers.Set("Authorization", "Bearer "+token)
	}
	g.forward(w, r, rec, target, headers, rewritten, func() {
		g.fail(w, rec, http.StatusBadGateway, "upstream_unavailable", "upstream_unavailable",
			fmt.Sprintf("插件 %s 不可达", id))
	})
}

// forwardA proxies to A with its key and the original path/query.
func (g *Gateway) forwardA(w http.ResponseWriter, r *http.Request, rec *reqRecord, body []byte) {
	rec.source = "a"
	if g.a == nil || strings.TrimSpace(g.a.BaseURL()) == "" {
		g.failAUnavailable(w, rec)
		return
	}
	headers := forwardHeaders(r.Header)
	if key := strings.TrimSpace(g.a.APIKey()); key != "" {
		headers.Set("Authorization", "Bearer "+key)
	}
	target := strings.TrimRight(strings.TrimSpace(g.a.BaseURL()), "/") + r.URL.RequestURI()
	g.forward(w, r, rec, target, headers, body, func() {
		g.failAUnavailable(w, rec)
	})
}

func (g *Gateway) failAUnavailable(w http.ResponseWriter, rec *reqRecord) {
	g.fail(w, rec, http.StatusBadGateway, "upstream_unavailable", "upstream_unavailable",
		aDownMessage)
}

// errUpstreamIdleTimeout marks a response body abandoned because the upstream
// stopped sending bytes for upstreamIdleReadTimeout.
var errUpstreamIdleTimeout = errors.New("upstream idle timeout")

// idleReadBody wraps an upstream response body with an idle-read deadline.
// Each Read rearms the timer; when it fires the request context is cancelled so
// the blocked Read returns instead of hanging forever (silent SSE stream).
type idleReadBody struct {
	body    io.ReadCloser
	timeout time.Duration
	cancel  context.CancelFunc
	timer   *time.Timer
	stalled atomic.Bool
}

func newIdleReadBody(body io.ReadCloser, timeout time.Duration, cancel context.CancelFunc) io.ReadCloser {
	if body == nil || timeout <= 0 {
		return body
	}
	b := &idleReadBody{body: body, timeout: timeout, cancel: cancel}
	b.timer = time.AfterFunc(timeout, func() {
		b.stalled.Store(true)
		if b.cancel != nil {
			b.cancel()
		}
	})
	return b
}

func (b *idleReadBody) Read(p []byte) (int, error) {
	if b.timer != nil {
		b.timer.Reset(b.timeout)
	}
	n, err := b.body.Read(p)
	if err != nil && b.stalled.Load() && !errors.Is(err, io.EOF) {
		return n, fmt.Errorf("%w after %s: %v", errUpstreamIdleTimeout, b.timeout, err)
	}
	return n, err
}

func (b *idleReadBody) Close() error {
	if b.timer != nil {
		b.timer.Stop()
	}
	if b.cancel != nil {
		b.cancel()
	}
	return b.body.Close()
}

// forward performs the upstream call and streams the response. onUnavailable
// runs when no bytes were written because the upstream could not be reached.
func (g *Gateway) forward(w http.ResponseWriter, r *http.Request, rec *reqRecord, target string, headers http.Header, body []byte, onUnavailable func()) {
	// A per-request cancel lets the idle-read watchdog abort a silent upstream
	// without cancelling the client's own context.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, r.Method, target, bytes.NewReader(body))
	if err != nil {
		onUnavailable()
		return
	}
	req.Header = headers
	req.ContentLength = int64(len(body))
	if len(body) == 0 {
		req.Body = nil
		req.GetBody = nil
		req.ContentLength = 0
	}

	resp, err := g.client.Do(req)
	if err != nil {
		if r.Context().Err() != nil {
			rec.status = 499
			rec.errText = "client canceled"
			return
		}
		onUnavailable()
		return
	}
	resp.Body = newIdleReadBody(resp.Body, upstreamIdleReadTimeout, cancel)
	defer resp.Body.Close()

	rec.status = resp.StatusCode
	dst := w.Header()
	copyResponseHeaders(dst, resp.Header)
	dst.Set(sourceHeader, rec.source)

	isSSE := strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") ||
		rec.stream
	w.WriteHeader(resp.StatusCode)
	headerAt := time.Now()

	flusher, _ := w.(http.Flusher)
	if isSSE && flusher != nil {
		flusher.Flush()
	}

	tap := newBodyTap(g.bodyCap, isSSE)
	ttfbSet := false
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if !ttfbSet {
				rec.ttfb = time.Since(rec.start)
				ttfbSet = true
			}
			tap.feed(buf[:n])
			if _, werr := w.Write(buf[:n]); werr != nil {
				rec.errText = "client write failed"
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				rec.errText = "upstream read failed: " + readErr.Error()
			}
			break
		}
	}
	if !ttfbSet {
		rec.ttfb = headerAt.Sub(rec.start)
	}
	tap.finish()
	rec.prompt, rec.completion, rec.cached = tap.prompt, tap.completion, tap.cached

	if g.debugBody.Load() {
		rec.bodyResp = string(tap.preview)
	}
	if rec.status >= 400 && rec.errText == "" {
		rec.errText = truncateUTF8(strings.TrimSpace(string(tap.preview)), 512)
	}
}

func copyResponseHeaders(dst, src http.Header) {
	for k, vs := range src {
		if isHopByHop(k) || strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
	// Content-Length is recomputed by net/http from our writes; the upstream
	// value is dropped because the response could be cut off mid-stream.
}

// forwardHeaders copies client headers to an upstream but drops every
// credential spelling the host itself accepts. A gateway client key must not
// reach a plugin that reads X-API-Key as its own upstream credential.
// Headers that only a native Anthropic upstream understands are handled on the
// translation path instead: chatSenderForMessages (anthropic.go) removes
// anthropic-version/-beta once a /v1/messages body has become a chat body, while
// forwardA keeps forwarding the client's headers untouched.
func forwardHeaders(src http.Header) http.Header {
	out := cloneHeader(src)
	for _, name := range []string{"Authorization", "X-API-Key", "Api-Key", "X-Client-Key"} {
		out.Del(name)
	}
	return out
}

func cloneHeader(src http.Header) http.Header {
	dst := make(http.Header, len(src))
	for k, vs := range src {
		if isHopByHop(k) || strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
	return dst
}

func isHopByHop(k string) bool {
	switch strings.ToLower(k) {
	case "connection", "proxy-connection", "keep-alive", "proxy-authenticate",
		"proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	}
	return false
}

// newReqID returns 12 random hex characters.
func newReqID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		n := uint64(time.Now().UnixNano())
		for i := len(b) - 1; i >= 0; i-- {
			b[i] = byte(n)
			n >>= 8
		}
	}
	return hex.EncodeToString(b[:])
}

// truncateUTF8 cuts s to at most n bytes without splitting a rune.
func truncateUTF8(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	b := s[:n]
	for len(b) > 0 && !utf8.ValidString(b) {
		b = b[:len(b)-1]
	}
	return b
}

// emit writes the request log row. It never fails the request.
func (g *Gateway) emit(rec *reqRecord, latency time.Duration) {
	if g.logs == nil || rec == nil {
		return
	}
	row := model.APIRequestLog{
		TS:               g.now().Format(time.RFC3339),
		ReqID:            rec.reqID,
		Source:           rec.source,
		Model:            rec.model,
		LogicalModel:     rec.logicalModel,
		Account:          rec.account,
		ConversationID:   rec.conversationID,
		KeyID:            rec.keyID,
		KeyName:          rec.keyName,
		Stream:           rec.stream,
		Status:           rec.status,
		LatencyMS:        latency.Milliseconds(),
		TTFBMS:           rec.ttfb.Milliseconds(),
		PromptTokens:     rec.prompt,
		CompletionTokens: rec.completion,
		CachedTokens:     rec.cached,
		Credits:          rec.credits,
		Error:            truncateUTF8(rec.errText, 512),
	}
	if !rec.firstTokenAt.IsZero() {
		row.FirstTokenAt = rec.firstTokenAt.Format(time.RFC3339)
	}
	if g.debugBody.Load() {
		row.BodyReq = truncateUTF8(rec.bodyReq, requestPreviewBytes)
		row.BodyResp = truncateUTF8(rec.bodyResp, g.bodyCap)
	}
	g.logs.Write(row)
}
