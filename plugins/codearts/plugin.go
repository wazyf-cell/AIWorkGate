package main

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"
)

// Version is the plugin version reported by /meta, /health and manifest.json.
const Version = "0.3.1"

// APIVersion is the plugin contract level implemented here (forced-account
// scheduling requires api_level 2).
const APIVersion = 2

var capabilities = []string{
	"upstream", "accounts", "checkin", "quota", "login_oauth",
	"refresh", "egress", "identity_request", "login_paste_code",
	"login_ide_plugin",
}

const (
	oauthClientID      = "codearts-agent"
	oauthRedirectPath  = "/oauth/callback"
	loginPluginName    = "snap_AIIDE"
	loginPluginVersion = "5.2.0"
	// The JetBrains/doer channel identifies itself differently from the
	// AIIDE portal channel: its ticket was minted for snap_jetbrains and the
	// credential endpoint rejects a poll carrying the other pair.
	idePluginName          = "snap_jetbrains"
	idePluginVersion       = "26.3.3"
	ideCallbackPath        = "/authentication"
	huaweiAuthBase         = "https://auth.huaweicloud.com/authui/login.html"
	oauthTheme             = "2"
	oauthLocale            = "zh-cn"
	credentialRefreshLead  = time.Hour
	defaultLoginTTL        = 15 * time.Minute
	defaultQuotaCacheTTL   = 60 * time.Second
	defaultModelsCacheTTL  = 6 * time.Hour
	defaultModelFetchLimit = 10 * time.Second
	defaultQueueRetry      = 10 * time.Second
	defaultQueueMaxTries   = 180
	defaultFirstTokenWait  = 300 * time.Second
	defaultIdleWait        = 600 * time.Second
)

// Endpoints groups every upstream URL. Tests override single entries; the
// zero value is filled with the production hosts from the reference
// implementation.
type Endpoints struct {
	STS           string
	Chat          string
	Queue         string
	Statistics    string
	Delivery      string
	Claim         string
	Confirm       string
	Ticket        string
	GatewayConfig string
	BuiltinModels string
	Authorize     string
	PortalLogin   string
}

func defaultEndpoints() Endpoints {
	snap := "https://snap-access.cn-north-4.myhuaweicloud.com"
	return Endpoints{
		STS:           "https://sts.cn-north-4.myhuaweicloud.com/v1/oauth2/tokens",
		Chat:          snap + "/api/v2/chat/completions",
		Queue:         snap + "/api/v1/queue/status",
		Statistics:    snap + "/snap-manager/v1/statistics/plugin",
		Delivery:      snap + "/v1/ops/delivery",
		Claim:         snap + "/v1/ops/claim",
		Confirm:       snap + "/v1/ops/confirm",
		Ticket:        snap + "/snap-manager/v1/login/ticket",
		GatewayConfig: "https://opengw.developer.huaweicloud.com/api/v1/gateway/config",
		BuiltinModels: snap + "/v1/model/builtin",
		Authorize:     "https://codearts.huaweicloud.com/portal/authorize",
		PortalLogin:   "https://codearts.huaweicloud.com/portal/login",
	}
}

// Options configures a plugin instance.
type Options struct {
	DataDir    string
	Token      string
	RulesFile  string
	EgressFile string
	LogWriter  io.Writer
	Now        func() time.Time

	Endpoints Endpoints
	// OpenBrowser is injected in tests so a login never launches a real
	// browser. The default uses `cmd /c start` on Windows.
	OpenBrowser func(url string) error

	// DisableBackground stops the maintenance loop (model and token refresh).
	// Tests set this so no network call happens outside their fake servers.
	DisableBackground bool

	LoginTTL       time.Duration
	QuotaCacheTTL  time.Duration
	ModelsCacheTTL time.Duration
	ModelFetchWait time.Duration

	QueueRetryDelay   time.Duration
	QueueMaxAttempts  int
	FirstTokenTimeout time.Duration
	IdleTimeout       time.Duration
}

type transportEntry struct {
	proxyURL string
	tr       *http.Transport
}

type quotaCacheEntry struct {
	at    int64
	quota Quota
}

// Plugin is the CodeArts sidecar.
type Plugin struct {
	ID      string
	Version string
	Token   string
	DataDir string

	mu       sync.RWMutex
	accounts []*Account

	stateMu sync.Mutex
	state   *State

	saveMu sync.Mutex

	rulesMu sync.RWMutex
	rules   RuleSet

	egressMu sync.RWMutex
	egress   EgressFile

	rulesPath  string
	egressPath string

	trMu       sync.Mutex
	transports map[string]*transportEntry

	sessionsMu sync.Mutex
	sessions   map[string]*loginSession

	modelsMu      sync.RWMutex
	models        []ModelInfo
	benefitModels map[string]bool
	modelsFetched int64
	modelsGen     uint64

	// modelsFetchMu serializes catalog fetches so concurrent forced syncs
	// (GET /v1/models?refresh=1) coalesce instead of stampeding upstream.
	modelsFetchMu sync.Mutex

	quotaMu    sync.Mutex
	quotaCache map[string]quotaCacheEntry

	refreshMu sync.Mutex
	refresh   map[string]*sync.Mutex

	opts      Options
	endpoints Endpoints
	startedAt time.Time
	now       func() time.Time

	logMu sync.Mutex
	logw  io.Writer

	rootCtx    context.Context
	rootCancel context.CancelFunc
	stopCh     chan struct{}
	stopOnce   sync.Once
	wg         sync.WaitGroup
}
