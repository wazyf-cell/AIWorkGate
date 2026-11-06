package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Version is reported by /meta, /health and manifest.json.
const Version = "0.3.1"

// APIVersion is the plugin contract level (forced-account scheduling needs 2).
const APIVersion = 2

var capabilities = []string{
	"upstream", "accounts", "checkin", "quota", "login_local_state", "refresh", "egress",
}

// Plugin status codes exposed through /meta.
const (
	pluginStatusOK            = "ok"
	pluginStatusNotFound      = "officeace_not_found"
	pluginStatusLoginRequired = "login_required"
	pluginStatusDBUnavailable = "db_unavailable"
	pluginStatusExpired       = "credential_expired"
	pluginStatusError         = "error"
)

// Options configures one plugin instance.
type Options struct {
	DataDir     string
	Token       string
	RulesFile   string
	EgressFile  string
	LocalAPIURL string
	IAMURL      string
	LogWriter   io.Writer

	// Test/override hooks; main leaves these empty.
	ForcedOfficeAceHome *string
	UserClawHome        string
	IsolateDiscovery    bool
	Now                 func() time.Time
}

type transportEntry struct {
	proxyURL string
	tr       *http.Transport
}

// Plugin is the OfficeAce sidecar: read-only session reader, local API client
// and direct Huawei MaaS transport.
type Plugin struct {
	ID      string
	Version string
	Token   string
	DataDir string

	mu    sync.RWMutex
	state *State

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

	credMu      sync.RWMutex
	refreshed   map[string]*Credential
	refreshFail map[string]refreshFailure

	sessMu   sync.Mutex
	sessSig  string
	sessAt   time.Time
	sessCred *Credential

	modelsMu        sync.RWMutex
	modelsFetchMu   sync.Mutex
	models          []Model
	modelsSource    string
	modelsFetchedAt time.Time

	cfgMu sync.RWMutex
	cfg   Config

	discMu sync.RWMutex
	disc   Discovery

	statusMu   sync.RWMutex
	statusCode string
	statusMsg  string

	svcMu      sync.RWMutex
	serviceUp  bool
	serviceAt  time.Time
	local      *localClient
	iamURL     string
	forcedHome *string
	isolate    bool
	userClaw   string

	startedAt time.Time
	now       func() time.Time

	logMu sync.Mutex
	logw  io.Writer
}

// New creates the plugin, loads persisted state and applies initial rules/egress.
func New(o Options) (*Plugin, error) {
	if strings.TrimSpace(o.DataDir) == "" {
		return nil, errors.New("plugin data dir is required")
	}
	if err := os.MkdirAll(o.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	st, err := loadState(o.DataDir)
	if err != nil {
		return nil, err
	}
	cfg, err := loadConfig(o.DataDir)
	if err != nil {
		return nil, err
	}
	logw := o.LogWriter
	if logw == nil {
		logw = os.Stdout
	}
	nowFn := o.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	iamURL := strings.TrimSpace(o.IAMURL)
	if iamURL == "" {
		iamURL = defaultIAMSTSUrl
	}
	p := &Plugin{
		ID:          "officeace",
		Version:     Version,
		Token:       o.Token,
		DataDir:     o.DataDir,
		state:       st,
		rulesPath:   o.RulesFile,
		egressPath:  o.EgressFile,
		transports:  map[string]*transportEntry{},
		sessions:    map[string]*loginSession{},
		refreshed:   map[string]*Credential{},
		refreshFail: map[string]refreshFailure{},
		cfg:         cfg,
		local:       newLocalClient(o.LocalAPIURL),
		iamURL:      iamURL,
		forcedHome:  o.ForcedOfficeAceHome,
		isolate:     o.IsolateDiscovery,
		userClaw:    o.UserClawHome,
		startedAt:   nowFn(),
		now:         nowFn,
		logw:        logw,
		statusCode:  pluginStatusOK,
	}
	p.disc = p.computeDiscovery()
	p.egress = EgressFile{Version: 1, Default: EgressEntry{Mode: egressDirect}, Accounts: map[string]EgressEntry{}}
	if mc, err := loadModelCache(p.DataDir); err == nil && len(mc.Models) > 0 {
		p.models = mc.Models
		p.modelsSource = mc.Source
		p.modelsFetchedAt = time.Unix(mc.FetchedAt, 0)
	}
	if _, err := p.reload(); err != nil {
		return nil, fmt.Errorf("initial reload: %w", err)
	}
	p.logLine("info", "plugin configured", map[string]any{
		"id":        p.ID,
		"version":   p.Version,
		"data_dir":  o.DataDir,
		"officeace": p.disc,
		"iam_url":   redactURL(p.iamURL),
	})
	p.initializeSession()
	return p, nil
}

func (p *Plugin) computeDiscovery() Discovery {
	var disc Discovery
	if p.isolate {
		home := ""
		if p.forcedHome != nil {
			home = *p.forcedHome
		}
		p.cfgMu.RLock()
		if p.cfg.OfficeAceHome != "" {
			home = p.cfg.OfficeAceHome
		}
		p.cfgMu.RUnlock()
		disc = discoverIsolated(home, p.userClaw)
	} else {
		p.cfgMu.RLock()
		cfgHome := p.cfg.OfficeAceHome
		p.cfgMu.RUnlock()
		disc = discoverOfficeAce(cfgHome)
	}
	if p.userClaw != "" {
		disc.UserClawHome = p.userClaw
	}
	return disc
}

func (p *Plugin) discovery() Discovery {
	p.discMu.RLock()
	defer p.discMu.RUnlock()
	return p.disc
}

func (p *Plugin) refreshDiscovery() Discovery {
	d := p.computeDiscovery()
	p.discMu.Lock()
	p.disc = d
	p.discMu.Unlock()
	p.sessMu.Lock()
	p.sessSig = ""
	p.sessCred = nil
	p.sessMu.Unlock()
	return d
}

func (p *Plugin) setStatus(code, msg string) {
	p.statusMu.Lock()
	p.statusCode = code
	p.statusMsg = msg
	p.statusMu.Unlock()
}

func (p *Plugin) status() (string, string) {
	p.statusMu.RLock()
	defer p.statusMu.RUnlock()
	return p.statusCode, p.statusMsg
}

func (p *Plugin) setService(up bool) {
	p.svcMu.Lock()
	p.serviceUp = up
	p.serviceAt = p.now()
	p.svcMu.Unlock()
}

func (p *Plugin) service() (bool, time.Time) {
	p.svcMu.RLock()
	defer p.svcMu.RUnlock()
	return p.serviceUp, p.serviceAt
}
