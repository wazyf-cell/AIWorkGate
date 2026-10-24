// Package webui serves the embedded local administration single-page app and
// the JSON admin API that backs it.
//
// The handler is intentionally transport-only: all host state is reached
// through the function fields of Options so this package stays free of
// storage, scheduler and plugin-runtime dependencies. The one exception is
// plugins.MaxZipBytes, which the install endpoint reads so the upload cap and
// the extractor's cap are the same number rather than two values that drift. A
// nil function field is reported as HTTP 501 (type "not_implemented"), which
// lets the SPA ship before the backing services are wired.
//
// Every /api request passes the CSRF guard below (server.guard): a same-origin
// Origin/Referer, a Sec-Fetch-Site allow-list and the host-session cookie that
// the launch-token endpoint (/api/session) issues. The cookie is only handed
// out in exchange for the one-shot token the tray puts in the browser URL, so
// it is a credential rather than something any local GET could obtain. The
// gateway's /v1 namespace is routed to internal/gateway by internal/app before
// this handler sees it, so the guard cannot reach client traffic.
package webui

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"aiworkgate/internal/model"
	"aiworkgate/internal/store"
)

// The all: prefix is required: Vite emits shared chunks such as
// _plugin-vue_export-helper-*.js, and a plain `dist` pattern silently skips
// names beginning with "_" or ".".
//
//go:embed all:dist
var distFS embed.FS

// maxBodyBytes limits every JSON request body accepted by the admin API.
const maxBodyBytes = 1 << 20

// apiPrefix marks the admin API routes the CSRF guard covers. Everything else
// on this mux is a SPA asset or a client-side route.
const apiPrefix = "/api/"

// hostSessionCookie names the in-memory admin session token. It is issued only
// by POST /api/session, in exchange for the one-shot launch token the host put
// in the browser URL, and must accompany every /api request, including GETs and
// the EventSource live tail (browsers send same-origin cookies on both). The
// value is the launch token itself: 32 random bytes generated once per host
// start. The host keeps a copy in the DPAPI secret store so a second launch can
// reopen the UI; it is never written to logs or SQLite. A page left open across
// a restart still has to be reopened from the tray; the guard answers 403
// admin_session_required instead of silently re-issuing.
const hostSessionCookie = "dsh_admin_session"

// launchTokenParam is the query parameter the tray appends to the URL it opens
// (http://127.0.0.1:7865/?t=<token>). The SPA exchanges it for the cookie and
// then strips it from the address bar.
const launchTokenParam = "t"

// sessionEndpoint is the only /api route reachable without a session cookie. It
// still requires the launch token, compared in constant time.
const sessionEndpoint = "/api/session"

// sessionTokenBytes is the entropy of the host-session token.
const sessionTokenBytes = 32

// Errors a host adapter can wrap to tell the admin API which status an
// account-level plugin call deserves. Keeping them here avoids importing the
// plugin runtime into this transport-only package.
var (
	// ErrPluginNotFound reports an unknown or uninstalled plugin id.
	ErrPluginNotFound = errors.New("plugin not found")
	// ErrPluginNotRunning reports a plugin that is disabled or stopped.
	ErrPluginNotRunning = errors.New("plugin is not running")
	// ErrInvalidRequest reports a request that conflicts with stored state
	// (unknown group, duplicate name, dangling reference, ...).
	ErrInvalidRequest = errors.New("invalid request")
	// ErrAccountNotFound reports an account missing from the plugin's list.
	ErrAccountNotFound = errors.New("account not found")
)

// Options wires the admin API to the host. Every field is optional except
// SessionToken, which the host must supply.
type Options struct {
	// SessionToken is the one-shot launch token the host generates once per
	// start (webui.NewSessionToken) and puts in the browser URL it opens. It is
	// the value behind the admin session cookie, so the host owns it: this
	// package never invents a token of its own, which would make the URL the
	// tray opens unusable.
	SessionToken string

	Status func() model.HostStatus

	GetSettings  func() (json.RawMessage, error)
	SaveSettings func(json.RawMessage) error

	AKeyConfigured func() bool
	SetAKey        func(string) error
	SetLANPassword func(string) error

	Plugins func() []model.PluginStatus
	// PluginMeta proxies GET /meta to one plugin. It returns the plugin's own
	// payload verbatim; the host owns the timeout and wraps an unknown or
	// stopped plugin in ErrPluginNotFound / ErrPluginNotRunning.
	PluginMeta func(id string) (any, error)
	// InstallPlugin installs an uploaded plugin archive. The zip is a local
	// temporary file this package created from the request body and removes
	// afterwards; enabled carries the explicit "start it right away" choice and
	// defaults to false, so a fresh install lands stopped.
	InstallPlugin func(zipPath string, enabled bool) error
	PluginAction  func(id, action string) error

	Accounts   func(pluginID string) any
	Checkins   func(pluginID, from, to string) any
	CheckinAll func() any

	// Model overrides (resource page editor). Nil callbacks answer 501.
	ModelOverrides      func(pluginID string) (map[string]store.ModelOverride, error)
	PutModelOverride    func(pluginID, model string, o store.ModelOverride) error
	DeleteModelOverride func(pluginID, model string) error

	// SetAccountEgress stores (or removes, with mode "default") one account's
	// egress assignment. It reports whether a running plugin was reloaded and,
	// when the reload failed, a warning to surface to the user.
	SetAccountEgress func(pluginID, accountID, mode, proxyID string) (reloaded bool, warning string, err error)
	// ResetIdentity proxies POST /accounts/{id}/identity/reset to a plugin.
	ResetIdentity func(pluginID, accountID, mode string) error

	// Login sessions are always owned by the plugin; the host only relays
	// instructions, form fields and status. Credential values passed to
	// AccountLoginSubmit are forwarded verbatim and never stored.
	AccountLoginStart  func(pluginID, method string) (model.LoginStart, error)
	AccountLoginPoll   func(pluginID, sessionID string) (model.LoginStatus, error)
	AccountLoginSubmit func(pluginID, sessionID string, values map[string]string) (model.LoginStatus, error)
	AccountLoginCancel func(pluginID, sessionID string) error
	AccountPatch       func(pluginID, accountID string, patch map[string]any) error
	AccountDelete      func(pluginID, accountID string) error
	AccountRefresh     func(pluginID, accountID string) (model.RefreshResult, error)

	// S1 account center. AccountCheckin runs one immediate check-in and
	// records it in checkin_results; AccountRename only touches the host
	// overlay. AccountBulk executes items sequentially and reports each item
	// without aborting the batch. AccountSave applies a whole draft in one
	// transaction. AccountIdentityDetails masks identifiers unless reveal is
	// set. AccountLoginState summarizes each plugin's login policy.
	AccountCheckin         func(pluginID, accountID string) (model.CheckinResult, error)
	AccountRename          func(pluginID, accountID, alias string) error
	AccountBulk            func(action string, items []model.AccountRef, params model.BulkParams) []model.BulkResult
	AccountSave            func(draft model.AccountDraft) (model.SaveApplied, error)
	AccountIdentityDetails func(pluginID, accountID string, reveal bool) (model.IdentityDetails, error)
	AccountLoginState      func() []model.LoginState

	// Groups backs /api/groups. Only listing and creation live here: renames,
	// deletes and membership changes are part of the atomic account draft
	// (POST /api/accounts/save), so there is exactly one write path.
	Groups      func() ([]model.AccountGroup, error)
	CreateGroup func(name, color string) (string, error)

	// RulePacks returns every stored pack plus the builtin pack names.
	RulePacks      func() ([]model.RulePack, []string, error)
	SaveRulePack   func(pack model.RulePack) error
	DeleteRulePack func(name string) error
	ResetRulePack  func(name string) error
	// ReloadRules rewrites every rules file and reloads running plugins. It
	// reports the reloaded plugin ids and, per plugin, the reload errors.
	ReloadRules func() (reloaded []string, failed map[string]string, err error)

	Proxies     func() []model.Proxy
	SaveProxy   func(model.Proxy) error
	DeleteProxy func(id string) error
	CheckProxy  func(id string) any

	RequestLogs func(query map[string]string) any
	RuntimeLogs func(source string, n int) []string

	// S2 usage views. Each callback receives the raw query parameters so
	// repeatable and comma-separated filters stay available to the host.
	UsageSummary  func(query map[string][]string) (any, error)
	UsageCredits  func(query map[string][]string) (any, error)
	UsageExpiry   func(query map[string][]string) (any, error)
	UsageSessions func(query map[string][]string) (any, error)
	UsageExport   func(w io.Writer, query map[string][]string) (filename string, err error)

	// Observability over raw request logs (plan 2026-10-21 §5): per-day
	// latency/TTFB percentiles and per-key token/credit totals. Both take an
	// already validated 1..90 day window; a nil callback answers 501.
	UsageLatency  func(days int) ([]store.DayLatency, error)
	UsageKeyUsage func(days int) ([]store.KeyUsageRow, error)
	// Per-model consumption ranking and per-model latency percentiles
	// (plan 2026-10-21 §5, A: CreditsTab.tsx:533-563, usage.rs:143-145).
	UsageModels       func(days int) ([]store.ModelUsageRow, error)
	UsageModelLatency func(days int) ([]store.ModelLatencyRow, error)

	// S2 unified logs. LogTimeline returns one merged page; LogBody returns
	// the stored (redacted) request/response body; LogSeed and LogPoll back the
	// SSE live tail with an opaque cursor. ProxyLogs serves the proxy traffic
	// tab, and LogRaw returns the one source line behind a timeline event.
	LogTimeline func(query map[string][]string) (any, error)
	LogBody     func(reqID, which string) (LogBody, error)
	LogSeed     func(query map[string][]string) (cursor string, err error)
	LogPoll     func(query map[string][]string, cursor string, limit int) (LogPollResult, error)
	ProxyLogs   func(query map[string][]string) (any, error)
	LogRaw      func(query map[string][]string) (LogRaw, error)

	// DebugActionIDs reports whether the admin action log may record concrete
	// resource ids instead of the "{id}" template. It follows the host's debug
	// switch, so the extra detail is a deliberate, visible choice.
	DebugActionIDs func() bool

	Keys      func() []model.ClientKey
	CreateKey func(name string) (string, model.ClientKey, error)
	DeleteKey func(id string) error

	// Client key scopes and quotas (plan 2026-10-21 §1). CreateKeySpec stores
	// the full validated definition; UpdateKey edits one key. A nil
	// CreateKeySpec falls back to CreateKey and ignores the scope fields, so a
	// host without the scope store still serves the legacy form.
	CreateKeySpec func(spec KeySpec) (string, model.ClientKey, error)
	UpdateKey     func(id string, spec KeySpec) (model.ClientKey, error)

	// User-defined custom models (Settings → 自定义模型). The host owns the
	// upstream key (DPAPI) and only ever returns key_set; Test probes the
	// upstream once on explicit user action. Nil callbacks answer 501.
	CustomModels      func() ([]store.CustomModel, error)
	CreateCustomModel func(input CustomModelInput) (store.CustomModel, error)
	UpdateCustomModel func(id string, patch CustomModelPatch) (store.CustomModel, error)
	DeleteCustomModel func(id string) (bool, error)
	TestCustomModel   func(id string) (latencyMS int64, err error)

	// Custom upstream connections (Settings → 自定义模型 → 接入). Nil
	// callbacks answer 501.
	CustomConnections            func() ([]store.CustomConnection, error)
	CreateCustomConnection       func(input CustomConnectionInput) (conn store.CustomConnection, created bool, err error)
	UpdateCustomConnection       func(id string, patch CustomConnectionPatch) (store.CustomConnection, error)
	DeleteCustomConnection       func(id string) (bool, error)
	TestCustomConnection         func(id string) (latencyMS int64, modelCount int, err error)
	FetchCustomConnectionModels  func(id string) ([]RemoteModel, error)
	ImportCustomConnectionModels func(id string, models []ImportModelInput) (ImportModelsResult, error)

	// Connectivity probes (Settings → 连通性自检). ProbeConnectivity runs one
	// probe, persists it (success or failure) and returns the redacted result;
	// ProbeHistory reads the persisted rows newest first. A nil callback
	// answers 501.
	ProbeConnectivity func(req ProbeRequest) (ProbeResult, error)
	ProbeHistory      func(limit int, kind, targetID string) ([]store.ConnectivityProbe, error)

	// S3 scheduling center (§5.4). Every callback receives already validated
	// input and returns a JSON payload (normally the refreshed overview).
	SchedulerOverview     func() (any, error)
	SaveSchedulerGlobal   func(update SchedulerGlobalUpdate) (any, error)
	SaveSchedulerPool     func(pluginID string, update SchedulerPoolUpdate) (any, error)
	ApplySchedulerPreset  func(name string) (any, error)
	SchedulerModels       func() (any, error)
	SaveSchedulerModel    func(logical string, update SchedulerModelUpdate) (any, error)
	DeleteSchedulerModel  func(logical string) error
	SchedulerExplain      func(model, conversationID, clientKey string) (any, error)
	SchedulerSticky       func() (any, error)
	DeleteSchedulerSticky func(scope, key string) (removed int64, err error)

	// S3 resource scheduling per plugin (§5.5).
	PoolDetail          func(pluginID string) (any, error)
	SavePoolSettings    func(pluginID string, update PoolSettingsUpdate) (any, error)
	SavePoolMembers     func(pluginID string, items []string) (any, error)
	SyncPoolCatalog     func(pluginID string) (any, error)
	PoolCatalog         func(pluginID string) (any, error)
	SetPoolModelEnabled func(pluginID, modelID string, enabled bool) (any, error)

	// Per-plugin settings (§ GAP 1). GetPluginSettings returns the editable
	// settings view for an installed plugin; SavePluginSettings validates and
	// persists a whole update. A host reports an unknown plugin by wrapping
	// ErrPluginNotFound and an invalid value by wrapping ErrInvalidRequest.
	GetPluginSettings  func(id string) (any, error)
	SavePluginSettings func(id string, update PluginSettingsUpdate) (any, error)

	// Out-of-app notifications (F-19). The host owns the URL secret and the
	// delivery worker; these callbacks only read and write the policy. A nil
	// callback answers 501.
	GetNotify  func() (NotifyStatus, error)
	SaveNotify func(update NotifyUpdate) (NotifyStatus, error)
	TestNotify func(ctx context.Context) error

	// RecordAction persists one user action (a state-changing admin API call)
	// into the audit log. Nil disables the action log, which keeps the package
	// usable on its own in tests.
	RecordAction func(entry model.AuditEntry) error
}

type server struct {
	opts       Options
	files      fs.FS
	fileServer http.Handler

	// session is the host-session token compared with the hostSessionCookie
	// value on every admin API request. It is the host-provided launch token
	// (Options.SessionToken), never a value generated here.
	session string

	// SSE cadence; New sets the production values and tests shorten them.
	streamPoll      time.Duration
	streamHeartbeat time.Duration
}

// New returns the admin API plus SPA handler. Routes under /api are JSON and
// pass the CSRF guard; every other path serves an embedded asset or falls back
// to index.html so client-side routes survive a reload. The gateway's /v1
// namespace is never served here (internal/app routes it to internal/gateway
// before this handler sees the request), so the guard cannot affect client
// traffic.
func New(opts Options) http.Handler {
	s := newServer(opts)

	mux := http.NewServeMux()
	mux.HandleFunc(sessionEndpoint, s.handleSession)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/settings", s.handleSettings)
	mux.HandleFunc("/api/notify", s.handleNotify)
	mux.HandleFunc("/api/notify/test", s.handleNotifyTest)
	mux.HandleFunc("/api/secrets/a-key", s.handleAKey)
	mux.HandleFunc("/api/secrets/lan-password", s.handleLANPassword)
	mux.HandleFunc("/api/plugins", s.handlePlugins)
	mux.HandleFunc("/api/plugins/install", s.handlePluginInstall)
	mux.HandleFunc("/api/plugins/{id}/{action}", s.handlePluginAction)
	// The literal segment wins over {action} for every method, so POST here is
	// answered 405 by the handler instead of "unknown plugin action: meta".
	mux.HandleFunc("/api/plugins/{id}/meta", s.handlePluginMeta)
	mux.HandleFunc("/api/plugins/{id}/settings", s.handlePluginSettings)
	mux.HandleFunc("/api/accounts", s.handleAccounts)
	mux.HandleFunc("/api/accounts/egress", s.handleAccountEgress)
	mux.HandleFunc("/api/accounts/identity/reset", s.handleIdentityReset)
	mux.HandleFunc("/api/accounts/login", s.handleAccountLoginStart)
	mux.HandleFunc("/api/accounts/login/{sid}", s.handleAccountLoginSession)
	mux.HandleFunc("/api/accounts/login/{sid}/submit", s.handleAccountLoginSubmit)
	mux.HandleFunc("/api/accounts/patch", s.handleAccountPatch)
	mux.HandleFunc("/api/accounts/delete", s.handleAccountDelete)
	mux.HandleFunc("/api/accounts/refresh", s.handleAccountRefresh)
	mux.HandleFunc("/api/accounts/checkin", s.handleAccountCheckin)
	mux.HandleFunc("/api/accounts/rename", s.handleAccountRename)
	mux.HandleFunc("/api/accounts/bulk", s.handleAccountBulk)
	mux.HandleFunc("/api/accounts/save", s.handleAccountSave)
	mux.HandleFunc("/api/accounts/identity/details", s.handleAccountIdentityDetails)
	mux.HandleFunc("/api/accounts/login-state", s.handleAccountLoginState)
	mux.HandleFunc("/api/groups", s.handleGroups)
	mux.HandleFunc("/api/checkin", s.handleCheckins)
	mux.HandleFunc("/api/checkin/all", s.handleCheckinAll)
	mux.HandleFunc("/api/rules", s.handleRules)
	mux.HandleFunc("/api/rules/reload", s.handleRulesReload)
	mux.HandleFunc("/api/rules/{name}", s.handleRulePack)
	mux.HandleFunc("/api/rules/{name}/reset", s.handleRulePackReset)
	mux.HandleFunc("/api/proxies", s.handleProxies)
	mux.HandleFunc("/api/proxies/{id}", s.handleProxy)
	mux.HandleFunc("/api/proxies/{id}/check", s.handleProxyCheck)
	mux.HandleFunc("/api/logs/requests", s.handleRequestLogs)
	// GET wins over the legacy pattern so the extended filters (status_class,
	// stream) reject bad values with 400 invalid_query.
	mux.HandleFunc("GET /api/logs/requests", s.handleRequestLogsFiltered)
	mux.HandleFunc("/api/logs/requests/export", s.handleRequestLogsExport)
	mux.HandleFunc("/api/logs/runtime", s.handleRuntimeLogs)
	mux.HandleFunc("/api/logs/timeline", s.handleLogTimeline)
	mux.HandleFunc("/api/logs/proxy", s.handleProxyLogs)
	mux.HandleFunc("/api/logs/raw", s.handleLogRaw)
	mux.HandleFunc("/api/logs/body", s.handleLogBody)
	mux.HandleFunc("/api/logs/stream", s.handleLogStream)
	mux.HandleFunc("/api/usage/summary", s.handleUsageSummary)
	mux.HandleFunc("/api/usage/credits", s.handleUsageCredits)
	mux.HandleFunc("/api/usage/expiry", s.handleUsageExpiry)
	mux.HandleFunc("/api/usage/sessions", s.handleUsageSessions)
	mux.HandleFunc("/api/usage/export", s.handleUsageExport)
	mux.HandleFunc("/api/usage/latency", s.handleUsageLatency)
	mux.HandleFunc("/api/usage/latency/models", s.handleUsageModelLatency)
	mux.HandleFunc("/api/usage/keys", s.handleUsageKeys)
	mux.HandleFunc("/api/usage/models", s.handleUsageModels)
	mux.HandleFunc("/api/keys", s.handleKeysAPI)
	mux.HandleFunc("/api/keys/{id}", s.handleKeyAPI)
	mux.HandleFunc("/api/custom-models", s.handleCustomModels)
	mux.HandleFunc("/api/custom-models/{id}", s.handleCustomModel)
	mux.HandleFunc("/api/custom-models/{id}/test", s.handleCustomModelTest)
	mux.HandleFunc("/api/custom-connections", s.handleCustomConnections)
	mux.HandleFunc("/api/custom-connections/{id}", s.handleCustomConnection)
	mux.HandleFunc("/api/custom-connections/{id}/test", s.handleCustomConnectionTest)
	mux.HandleFunc("/api/custom-connections/{id}/remote-models", s.handleCustomConnectionRemoteModels)
	mux.HandleFunc("/api/custom-connections/{id}/models/import", s.handleCustomConnectionImport)
	mux.HandleFunc("/api/diagnostics/probe", s.handleDiagnosticsProbe)
	mux.HandleFunc("/api/diagnostics/probes", s.handleDiagnosticsProbes)
	mux.HandleFunc("/api/scheduler", s.handleScheduler)
	mux.HandleFunc("/api/scheduler/global", s.handleSchedulerGlobal)
	mux.HandleFunc("/api/scheduler/pools/{plugin}", s.handleSchedulerPool)
	mux.HandleFunc("/api/scheduler/presets/{name}", s.handleSchedulerPreset)
	mux.HandleFunc("/api/scheduler/models", s.handleSchedulerModels)
	mux.HandleFunc("/api/scheduler/models/{logical}", s.handleSchedulerModel)
	mux.HandleFunc("/api/scheduler/explain", s.handleSchedulerExplain)
	mux.HandleFunc("/api/scheduler/sticky", s.handleSchedulerSticky)
	mux.HandleFunc("/api/pools/{plugin}", s.handlePool)
	mux.HandleFunc("/api/pools/{plugin}/members", s.handlePoolMembers)
	mux.HandleFunc("/api/pools/{plugin}/sync", s.handlePoolSync)
	mux.HandleFunc("/api/pools/{plugin}/catalog", s.handlePoolCatalog)
	mux.HandleFunc("/api/pools/{plugin}/catalog/{model}", s.handlePoolModelEnabled)
	mux.HandleFunc("/api/pools/{plugin}/models/overrides", s.handleModelOverrides)
	mux.HandleFunc("/api/pools/{plugin}/models/overrides/{model}", s.handleModelOverride)
	mux.HandleFunc("/api/", s.handleAPINotFound)
	mux.HandleFunc("/", s.serveSPA)
	// recordAction sits inside the guard: a request that fails the CSRF checks
	// must not be recorded as a user action.
	return &launchHandler{Handler: s.recordAction(s.guard(mux)), token: s.session}
}

// launchHandler is the http.Handler New returns. It exposes the launch token so
// an embedder that did not pass Options.SessionToken (a test, chiefly) can still
// perform the /api/session exchange instead of being locked out of the API it
// wants to exercise.
type launchHandler struct {
	http.Handler
	token string
}

// LaunchToken reports the token this handler's /api/session accepts.
func (h *launchHandler) LaunchToken() string { return h.token }

func newServer(opts Options) *server {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic("webui: embedded dist is missing: " + err.Error())
	}
	token := opts.SessionToken
	if token == "" {
		// The host always supplies the token, because it needs the same value for
		// the URL it opens. This branch only serves embedders without a launch
		// URL: serving an admin surface whose token nobody knows would be worse
		// than minting one they can read back through LaunchToken.
		token = NewSessionToken()
	}
	return &server{
		opts:            opts,
		files:           sub,
		fileServer:      http.FileServer(http.FS(sub)),
		session:         token,
		streamPoll:      time.Second,
		streamHeartbeat: 15 * time.Second,
	}
}

// ---------------------------------------------------------------- guard

// guard rejects browser-driven request forgery on the admin API. It layers
// three independent checks: a same-origin Origin/Referer, a Sec-Fetch-Site
// allow-list and the host-session cookie (SameSite=Strict, so a cross-site
// page can never make the browser send it). Every method needs the cookie,
// reads included: a GET must not hand one out, because that made the cookie a
// CSRF token any local process could fetch instead of a credential only the
// launch URL knows. Non-API paths are untouched: the SPA shell and its assets
// must stay readable without a session.
func (s *server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, apiPrefix) {
			next.ServeHTTP(w, r)
			return
		}
		if !sameOriginRequest(r) {
			// The offending header value is attacker controlled, so it is
			// deliberately never echoed back. This applies to the session
			// endpoint too: knowing the launch token is not a licence to hand a
			// browser's cookie to a foreign origin.
			writeError(w, http.StatusForbidden, "cross-origin admin request rejected", "cross_origin")
			return
		}
		// The session endpoint is the bootstrap: the one /api route a client
		// without a cookie may reach. It still needs the launch token, which it
		// compares itself.
		if r.URL.Path == sessionEndpoint {
			next.ServeHTTP(w, r)
			return
		}
		if !s.validSession(r) {
			// No bootstrap on reads any more: a GET used to hand the cookie to
			// any caller, which made it a CSRF token rather than a credential.
			// Every method now needs the cookie the launch URL earned.
			writeError(w, http.StatusForbidden,
				"admin session missing or stale; reopen the AIWorkGate page from the tray", "admin_session_required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// NewSessionToken returns one random 32-byte launch token, hex encoded. The
// host calls it once per start, passes the value in Options.SessionToken and
// puts the same value in the browser URL it opens.
func NewSessionToken() string {
	buf := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		panic("webui: cannot generate the admin session token: " + err.Error())
	}
	return hex.EncodeToString(buf)
}

func (s *server) validSession(r *http.Request) bool {
	cookie, err := r.Cookie(hostSessionCookie)
	if err != nil || cookie.Value == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(s.session)) == 1
}

// sameOriginRequest reports whether the request may reach the admin API.
// Origin and Referer are only checked when present (a browser always sends
// Origin on the methods the UI uses). A present value must match the request
// authority exactly, which for the normal deployment is a loopback form such
// as 127.0.0.1:7865, localhost:7865 or [::1]:7865; localhost and 127.0.0.1 are
// deliberately not treated as interchangeable. A LAN deployment reaches this
// handler only after internal/app has verified the LAN password, and it still
// needs an exact match.
func sameOriginRequest(r *http.Request) bool {
	if site := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))); site != "" {
		// "same-site" covers another page on a sibling host/port (for example
		// http://localhost:7865 driving http://127.0.0.1:7865), which is
		// rejected the same way as a cross-site request.
		if site == "cross-site" || site == "same-site" {
			return false
		}
	}
	scheme := requestScheme(r)
	if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" && !sameOrigin(origin, r.Host, scheme) {
		return false
	}
	if referer := strings.TrimSpace(r.Header.Get("Referer")); referer != "" {
		parsed, err := url.Parse(referer)
		if err != nil || parsed.Host == "" {
			return false
		}
		if !sameOrigin(parsed.Scheme+"://"+parsed.Host, r.Host, scheme) {
			return false
		}
	}
	return true
}

// requestScheme is the scheme the admin UI is served over: https only when the
// server negotiated TLS, which the loopback host never does.
func requestScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// sameOrigin compares an absolute origin with the request authority, treating a
// missing port as the scheme default on both sides.
func sameOrigin(origin, authority, scheme string) bool {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" || authority == "" {
		return false
	}
	if parsed.Scheme != scheme {
		return false
	}
	return canonicalAuthority(parsed.Host, parsed.Scheme) == canonicalAuthority(authority, scheme)
}

func canonicalAuthority(authority, scheme string) string {
	host, port, err := net.SplitHostPort(authority)
	if err != nil {
		host, port = strings.Trim(authority, "[]"), ""
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return host + ":" + port
}

func (s *server) serveSPA(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "method_not_allowed")
		return
	}

	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" {
		s.serveIndex(w, r)
		return
	}
	isAsset := name == "assets" || strings.HasPrefix(name, "assets/")
	if info, err := fs.Stat(s.files, name); err == nil && !info.IsDir() {
		// Vite fingerprints everything under assets/, so it can be cached hard.
		if isAsset {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		s.fileServer.ServeHTTP(w, r)
		return
	}
	// A stale index.html can reference asset hashes from a previous build. The
	// SPA fallback must never answer those: returning HTML with 200 makes the
	// browser fail with "Failed to fetch dynamically imported module" and hides
	// the real stale-cache problem, so answer a plain 404 instead.
	if isAsset {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "asset not found\n")
		return
	}
	s.serveIndex(w, r)
}

// serveIndex serves the SPA shell. It deliberately does NOT hand out the
// session cookie: the shell is readable by anyone who can reach the port, and
// issuing the cookie here was one of the two ways a bare local request used to
// obtain admin credentials. The cookie now comes only from POST /api/session,
// which requires the launch token.
func (s *server) serveIndex(w http.ResponseWriter, r *http.Request) {
	// The shell names fingerprint assets that a rebuild replaces, so it must
	// always be revalidated (no-store also covers client and proxy caches).
	w.Header().Set("Cache-Control", "no-store")
	// Anti-framing: clickjacking is already hard with SameSite=Strict, but the
	// shell has no reason to be embedded anywhere. Only frame-ancestors is set
	// so the inline theme bootstrap in index.html keeps working.
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.ServeFileFS(w, r, s.files, "index.html")
}

// issueSessionCookie hands out the admin session token. HttpOnly keeps it out
// of page scripts and SameSite=Strict keeps it out of any request a third-party
// page can trigger. Secure stays off: this is served over plain http on
// loopback. There is no expiry - the value lives only in this process and is
// regenerated on the next host start. Only handleSession may call this.
func (s *server) issueSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     hostSessionCookie,
		Value:    s.session,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

func (s *server) handleAPINotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "unknown API endpoint: "+r.Method+" "+r.URL.Path, "not_found")
}

func (s *server) unavailable(w http.ResponseWriter) {
	writeError(w, http.StatusNotImplemented, "not implemented", "not_implemented")
}

// ---------------------------------------------------------------- helpers

func writeJSON(w http.ResponseWriter, status int, value any) {
	header := w.Header()
	header.Set("Content-Type", "application/json; charset=utf-8")
	header.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if value == nil {
		_, _ = w.Write([]byte("null\n"))
		return
	}
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}

func writeRawJSON(w http.ResponseWriter, status int, raw []byte) {
	header := w.Header()
	header.Set("Content-Type", "application/json; charset=utf-8")
	header.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeError(w http.ResponseWriter, status int, message, kind string) {
	if kind == "" {
		switch status {
		case http.StatusBadRequest:
			kind = "bad_request"
		case http.StatusNotFound:
			kind = "not_found"
		case http.StatusMethodNotAllowed:
			kind = "method_not_allowed"
		case http.StatusNotImplemented:
			kind = "not_implemented"
		case http.StatusRequestEntityTooLarge:
			kind = "payload_too_large"
		default:
			kind = "error"
		}
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"message": message, "type": kind},
	})
}

func allowMethods(w http.ResponseWriter, r *http.Request, methods ...string) bool {
	for _, method := range methods {
		if r.Method == method {
			return true
		}
	}
	w.Header().Set("Allow", strings.Join(methods, ", "))
	writeError(w, http.StatusMethodNotAllowed, "method not allowed", "method_not_allowed")
	return false
}

// decodeJSON decodes a request body of at most 1 MiB, replying with the
// matching JSON error when the body is missing or malformed.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		switch {
		case errors.As(err, &tooLarge):
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large (limit 1 MiB)", "payload_too_large")
		case errors.Is(err, io.EOF):
			writeError(w, http.StatusBadRequest, "request body is required", "bad_request")
		default:
			writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error(), "bad_request")
		}
		return false
	}
	return true
}

func parseLimit(raw string, fallback, max int) int {
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return fallback
	}
	if value > max {
		return max
	}
	return value
}

func ensureJSONObject(raw json.RawMessage) (json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return nil, false
	}
	return json.RawMessage(trimmed), true
}
