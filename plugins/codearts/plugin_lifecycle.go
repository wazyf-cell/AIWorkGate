package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// New creates the plugin, loads persisted encrypted credentials and applies
// the initial rules/egress files. Network work is never done synchronously
// here: the model catalog is loaded from disk (or the hardcoded fallback) and
// refreshed in the background.
func New(o Options) (*Plugin, error) {
	if strings.TrimSpace(o.DataDir) == "" {
		return nil, errors.New("plugin data dir is required")
	}
	if err := os.MkdirAll(o.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	accounts, err := loadAccounts(o.DataDir)
	if err != nil {
		return nil, err
	}
	st, err := loadState(o.DataDir)
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
	endpoints := o.Endpoints
	def := defaultEndpoints()
	fill := func(dst *string, v string) {
		if strings.TrimSpace(*dst) == "" {
			*dst = v
		}
	}
	fill(&endpoints.STS, def.STS)
	fill(&endpoints.Chat, def.Chat)
	fill(&endpoints.Queue, def.Queue)
	fill(&endpoints.Statistics, def.Statistics)
	fill(&endpoints.Delivery, def.Delivery)
	fill(&endpoints.Claim, def.Claim)
	fill(&endpoints.Confirm, def.Confirm)
	fill(&endpoints.Ticket, def.Ticket)
	fill(&endpoints.GatewayConfig, def.GatewayConfig)
	fill(&endpoints.BuiltinModels, def.BuiltinModels)
	fill(&endpoints.Authorize, def.Authorize)
	fill(&endpoints.PortalLogin, def.PortalLogin)
	if o.LoginTTL <= 0 {
		o.LoginTTL = defaultLoginTTL
	}
	if o.QuotaCacheTTL <= 0 {
		o.QuotaCacheTTL = defaultQuotaCacheTTL
	}
	if o.ModelsCacheTTL <= 0 {
		o.ModelsCacheTTL = defaultModelsCacheTTL
	}
	if o.ModelFetchWait <= 0 {
		o.ModelFetchWait = defaultModelFetchLimit
	}
	if o.QueueRetryDelay <= 0 {
		o.QueueRetryDelay = defaultQueueRetry
	}
	if o.QueueMaxAttempts <= 0 {
		o.QueueMaxAttempts = defaultQueueMaxTries
	}
	if o.FirstTokenTimeout <= 0 {
		o.FirstTokenTimeout = defaultFirstTokenWait
	}
	if o.IdleTimeout <= 0 {
		o.IdleTimeout = defaultIdleWait
	}

	rootCtx, rootCancel := context.WithCancel(context.Background())
	p := &Plugin{
		ID:         "codearts",
		Version:    Version,
		Token:      o.Token,
		DataDir:    o.DataDir,
		accounts:   accounts,
		state:      st,
		rulesPath:  o.RulesFile,
		egressPath: o.EgressFile,
		transports: map[string]*transportEntry{},
		sessions:   map[string]*loginSession{},
		quotaCache: map[string]quotaCacheEntry{},
		refresh:    map[string]*sync.Mutex{},
		opts:       o,
		endpoints:  endpoints,
		startedAt:  nowFn(),
		now:        nowFn,
		logw:       logw,
		rootCtx:    rootCtx,
		rootCancel: rootCancel,
		stopCh:     make(chan struct{}),
	}
	p.egress = EgressFile{Version: 1, Default: EgressEntry{Mode: egressDirect}, Accounts: map[string]EgressEntry{}}
	cache := loadModelCache(o.DataDir)
	if len(cache.Models) > 0 {
		p.models = cache.Models
		p.benefitModels = toBenefitSet(cache.BenefitModels)
		p.modelsFetched = cache.FetchedAt
	} else {
		p.models = defaultModels()
		p.benefitModels = toBenefitSet(defaultBenefitModels())
	}
	if p.benefitModels == nil {
		p.benefitModels = map[string]bool{}
	}
	res, err := p.reload()
	if err != nil {
		return nil, fmt.Errorf("initial reload: %w", err)
	}
	for _, e := range res.Errors {
		p.logLine("warn", "reload warning at startup", map[string]any{"error": e})
	}
	if !o.DisableBackground {
		p.startBackground()
	}
	return p, nil
}

// Close stops background work and closes any pending login listeners.
func (p *Plugin) Close() {
	p.stopOnce.Do(func() {
		p.rootCancel()
		close(p.stopCh)
		p.cancelAllSessions()
	})
	p.wg.Wait()
	p.resetTransports()
}

func (p *Plugin) startBackground() {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		// First maintenance shortly after start, then every five minutes.
		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-p.stopCh:
				return
			case <-timer.C:
				p.maintenance()
				timer.Reset(5 * time.Minute)
			}
		}
	}()
}

func (p *Plugin) maintenance() {
	ctx, cancel := context.WithTimeout(p.rootCtx, 2*time.Minute)
	defer cancel()
	if p.modelsStale(p.now().Unix()) {
		if err := p.refreshModels(ctx); err != nil {
			p.logLine("warn", "model refresh failed", map[string]any{"error": err.Error()})
		}
	}
	now := p.now()
	for _, snap := range p.accountSnapshots() {
		if !snap.Enabled || !snap.Credential.refreshable() {
			continue
		}
		exp := snap.Credential.expiry()
		if exp.IsZero() || exp.Sub(now) > credentialRefreshLead {
			continue
		}
		if _, err := p.ensureCredential(ctx, snap.ID, false); err != nil {
			p.logLine("warn", "credential refresh failed", map[string]any{"account": snap.ID, "error": err.Error()})
		}
	}
}
