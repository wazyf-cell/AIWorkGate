// Package scheduler implements the host-side account selection core of
// AIWorkGate (design §6): model resolution, pool ordering, in-pool strategies,
// concurrency caps, sticky sessions, cooldowns, the pool circuit breaker and
// the explain view.
//
// The package is deliberately free of plugin, store and HTTP dependencies: a
// caller feeds it a Snapshot and gets a Decision. All time and randomness enter
// through Options so tests stay deterministic.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"aiworkgate/internal/model"
)

// Inter-pool ordering modes (§6.2).
const (
	InterPoolSmart = "smart"
	InterPoolFixed = "fixed"
	// InterPoolCheap orders candidate pools by configured token price (P9/D-44).
	InterPoolCheap = "cheap"
)

// In-pool account strategies (§6.3).
const (
	StrategyCreditFirst    = "credit_first"
	StrategyRemainingFirst = "remaining_first"
	StrategyExpiringFirst  = "expiring_first"
	StrategyFree           = "free"
	StrategyPace           = "pace"
	StrategyRandom         = "random"
	StrategyWeighted3      = "weighted3"
	StrategyP2C            = "p2c"
	StrategyFixed          = "fixed"
)

// Errors returned by Pick/Explain.
var (
	// ErrUnknownModel reports a model that is neither a physical
	// "<plugin>/<model>" id nor a configured logical binding.
	ErrUnknownModel = errors.New("scheduler: unknown model")
	// ErrNoCandidate reports that every pool/account was filtered out.
	ErrNoCandidate = errors.New("scheduler: no candidate account")
	// ErrAccountRateLimited reports that every candidate account is paused by
	// its per-minute request cap (P5 每分钟请求数) and PickWait's budget
	// expired before a window slid the account back in.
	ErrAccountRateLimited = errors.New("scheduler: account rate limited")
)

// StrategyNames lists the accepted in-pool strategies in UI order.
var StrategyNames = []string{
	StrategyCreditFirst, StrategyRemainingFirst, StrategyExpiringFirst,
	StrategyFree, StrategyPace,
	StrategyRandom, StrategyWeighted3, StrategyP2C, StrategyFixed,
}

// InterPoolNames lists the accepted inter-pool modes.
var InterPoolNames = []string{InterPoolSmart, InterPoolCheap, InterPoolFixed}

// ValidStrategy reports whether name is an accepted in-pool strategy. The
// empty string is accepted by the admin API as "reset to the default".
func ValidStrategy(name string) bool {
	switch name {
	case StrategyCreditFirst, StrategyRemainingFirst, StrategyExpiringFirst,
		StrategyFree, StrategyPace,
		StrategyRandom, StrategyWeighted3, StrategyP2C, StrategyFixed:
		return true
	}
	return false
}

// ValidInterPool reports whether name is an accepted inter-pool mode.
func ValidInterPool(name string) bool {
	return name == InterPoolSmart || name == InterPoolCheap || name == InterPoolFixed
}

// AccountKey identifies one plugin-owned account.
type AccountKey struct {
	PluginID  string
	AccountID string
}

// String renders the key for logs and explain reasons.
func (k AccountKey) String() string { return k.PluginID + "/" + k.AccountID }

// Account is the scheduler's snapshot of one account.
type Account struct {
	ID                string
	Label             string
	Enabled           bool
	CredentialPresent bool
	// Status is the plugin-reported account status (GET /accounts "status":
	// ok / expired / invalid / rate_limited / unknown, docs/插件开发指南.md).
	// It is distinct from the host health overlay (status_kind) and is the only
	// signal an account gives when its quota block is empty.
	Status    string
	Remaining float64
	Total     float64
	Unlimited bool
	ExpiresAt int64
	// RunsOutAt is the F-14 "券/批次烧完" timestamp (0 = unknown). It is the
	// primary signal of the expiring_first strategy; when the bridge supplies
	// windows[] data (task-26) it is filled per account, and the legacy
	// expires_at remains the fallback for single-group data.
	RunsOutAt int64
	// ResetAt is the F-14 "周期重置" timestamp (0 = unknown). Filled by the
	// bridge from windows[] resetsAt; legacy data has no periodic reset, so
	// the resetting alert rule stays silent.
	ResetAt  int64
	Plan     string
	Priority int
	Weight   float64
	// RPM is the account's per-minute request cap from the host overlay (P5
	// 每分钟请求数; 0 = off). Hits are counted per upstream attempt (Acquire),
	// so retries and hedged seconds count too.
	RPM            int
	CooldownUntil  int64
	ModelCooldowns map[string]int64
	// Egress is the recent-evidence verdict of the account's assigned proxy,
	// filled by the snapshot builder. It is data, not a callback: Pick runs
	// under the scheduler lock and must not touch the store.
	Egress EgressEvidence
}

// EgressEvidence is the host's recent evidence about one account's assigned
// proxy. Failed is true only for a failure inside the evidence window; Label
// and Error are already user-safe (no credentials).
type EgressEvidence struct {
	Failed bool
	Label  string
	Error  string
	At     int64
}

// Key returns the account key inside pluginID's pool.
func (a Account) KeyFor(pluginID string) AccountKey {
	return AccountKey{PluginID: pluginID, AccountID: a.ID}
}

// Pool is the snapshot of one plugin's scheduling pool.
type Pool struct {
	PluginID             string
	Name                 string
	APIVersion           int
	UpstreamEnabled      bool
	Strategy             string
	PoolPriority         int
	AccountConcurrency   int
	PoolStickyTTL        int
	ConvStickyTTL        int
	HedgeMS              int
	DefaultReasoning     string
	BackgroundDowngrade  bool
	LongContextDowngrade bool

	Accounts []Account
	// Models maps a physical model id to its catalog metadata. Unknown models
	// score as multiplier 1.0 and are never considered free.
	Models map[string]model.ModelInfo
	// Members is the explicit pool membership. A nil map means "every account
	// with a credential participates".
	Members map[string]bool
}

// Source is one physical model candidate of a request.
type Source struct {
	PluginID string
	Model    string
}

// Binding maps a logical model name to its ordered physical sources.
type Binding struct {
	Logical string
	Display string
	Items   []Source
	// Rules override Items for the first request whose features match (P11).
	Rules []BindingRule
	// Weights ("plugin/model" → share) drives smooth weighted rotation (P11).
	Weights map[string]int
	// Intent, when set, lets a decision upstream pick another group for this
	// request before account selection (P11).
	Intent *IntentBinding
}

// IntentBinding is the scheduler-side form of a config routing intent.
type IntentBinding struct {
	Upstream   string
	Labels     []string
	Route      map[string]string
	Default    string
	ProbeChars int
}

// BindingRule is one feature-gated member set of a group.
type BindingRule struct {
	Name  string
	When  BindingWhen
	Items []Source
}

// BindingWhen is a conjunction of request features; zero values are ignored.
type BindingWhen struct {
	MinChars int
	MaxChars int
	// MinTokens/MaxTokens bound the estimated prompt tokens; zero ignores.
	MinTokens int
	MaxTokens int
	Image     bool
	Thinking  bool
	Agent     string
	StartHour *int
	EndHour   *int
}

func (w BindingWhen) matches(req Request, now time.Time) bool {
	if w.MinChars > 0 && req.PromptChars < w.MinChars {
		return false
	}
	if w.MaxChars > 0 && req.PromptChars > w.MaxChars {
		return false
	}
	if w.MinTokens > 0 && req.EstimatedTokens < w.MinTokens {
		return false
	}
	if w.MaxTokens > 0 && req.EstimatedTokens > w.MaxTokens {
		return false
	}
	if w.Image && !req.HasImage {
		return false
	}
	if w.Thinking && !req.Thinking {
		return false
	}
	if w.Agent != "" && !strings.Contains(strings.ToLower(req.Agent), strings.ToLower(w.Agent)) {
		return false
	}
	if w.StartHour != nil && w.EndHour != nil {
		hour := now.In(time.Local).Hour()
		start, end := *w.StartHour, *w.EndHour
		switch {
		case start == end:
			// whole day
		case start < end:
			if hour < start || hour >= end {
				return false
			}
		default:
			if hour < start && hour >= end {
				return false
			}
		}
	}
	return true
}

// weightedSourcesLocked rotates group members with smooth weighted round
// robin (the NGINX algorithm), returning the chosen source first and the rest
// in their configured order for failover. Caller holds s.mu.
func (s *Scheduler) weightedSourcesLocked(group string, sources []Source, weights map[string]int) []Source {
	if len(sources) < 2 || len(weights) == 0 {
		return sources
	}
	if s.groupWeightState == nil {
		s.groupWeightState = make(map[string]map[string]int64)
	}
	cursors := s.groupWeightState[group]
	if cursors == nil {
		cursors = make(map[string]int64, len(sources))
		s.groupWeightState[group] = cursors
	}
	current := make([]int64, len(sources))
	best := 0
	var total int64
	for i, src := range sources {
		key := src.PluginID + "/" + src.Model
		weight := int64(weights[key])
		if weight <= 0 {
			weight = 1
		}
		total += weight
		cursors[key] += weight
		current[i] = cursors[key]
		if current[i] > current[best] {
			best = i
		}
	}
	chosenKey := sources[best].PluginID + "/" + sources[best].Model
	cursors[chosenKey] -= total
	out := make([]Source, 0, len(sources))
	out = append(out, sources[best])
	for i, src := range sources {
		if i != best {
			out = append(out, src)
		}
	}
	return out
}

// snapshotPrice looks up one source's effective price by model id, then by
// "plugin/model" (both lower-cased).
func snapshotPrice(snap *Snapshot, src Source) (float64, bool) {
	if snap == nil || len(snap.Prices) == 0 {
		return 0, false
	}
	if v, ok := snap.Prices[strings.ToLower(strings.TrimSpace(src.Model))]; ok {
		return v, true
	}
	if v, ok := snap.Prices[strings.ToLower(src.PluginID+"/"+src.Model)]; ok {
		return v, true
	}
	return 0, false
}

func bindingSourcesForRequest(b Binding, req Request, now time.Time) ([]Source, string) {
	for _, rule := range b.Rules {
		if rule.When.matches(req, now) {
			return rule.Items, rule.Name
		}
	}
	return b.Items, ""
}

// Snapshot is the immutable scheduling view read once per request.
type Snapshot struct {
	Pools     map[string]*Pool
	Bindings  map[string]Binding
	InterPool string
	// Prices maps a lower-cased model id to its effective per-1M price
	// (input+output, multiplier applied). Nil/unknown means "no price".
	Prices map[string]float64
}

// Sticky scopes mirror the persisted sticky_bindings.scope column.
const (
	StickyScopeConversation = model.StickyScopeConversation
	StickyScopePool         = model.StickyScopePool
)

// StickyBinding is one persisted pin.
type StickyBinding struct {
	Scope     string
	Key       string
	PluginID  string
	AccountID string
	ExpiresAt int64
}

// StickyStore persists sticky bindings. *store.Store is wrapped by the app
// package; tests use an in-memory implementation. Nil disables persistence
// (the in-memory cache keeps working).
type StickyStore interface {
	ListSticky(scope string) ([]StickyBinding, error)
	SaveSticky(binding StickyBinding) error
	DeleteSticky(scope, key string) error
	DeleteExpiredSticky(now int64) (int64, error)
}

// CooldownRecord is one request-path cooldown that must survive a restart. An
// empty Model marks an account-wide cooldown (5xx/408/409); a non-empty Model
// marks a 429 cooldown of one account+model pair. Strikes is the 429 doubling
// counter, so the next request continues the backoff instead of restarting at
// the base duration.
//
// Why is the D-24/D-45 reason class (CooldownRateLimit ... CooldownOther) and
// By is the triggering account key ("plugin/account"); both are structured
// metadata for the manual clear endpoint and the UI badge. The durable store
// has no dedicated columns for them, so the app-side adapter folds Why into
// the reason text ("[rate_limit] status 429") and rebuilds By from the row key.
//
// Ported from A (TraeWorkAssistant, MIT, 朱天伟) models.rs:712-731 and
// store/docs.rs:1266-1290: A persists `until` plus `error_count` per account
// and restores both into the pool on startup (api_server/pool.rs:331-370).
type CooldownRecord struct {
	PluginID  string
	AccountID string
	Model     string
	Until     int64
	Reason    string
	// Why is one of the D-24 classes (cooldown_policy.go); "" keeps legacy rows
	// without attribution loadable.
	Why string
	// By is the account key that triggered the cooldown ("plugin/account").
	By      string
	Strikes int
}

// CooldownStore persists request-path cooldowns. *store.Store is wrapped by the
// app package; tests use an in-memory implementation. Nil disables persistence
// (cooldowns still protect the running process).
type CooldownStore interface {
	// LoadCooldowns returns the unexpired rows and prunes expired ones.
	LoadCooldowns(now int64) ([]CooldownRecord, error)
	SaveCooldown(record CooldownRecord) error
	DeleteCooldown(pluginID, accountID, modelID string) error
}

// Request is one scheduling question.
type Request struct {
	Model          string
	ConversationID string
	ClientKey      string
	Exclude        map[AccountKey]bool
	Hedge          bool
	// D-43 会话亲和（plans/2026-11-05 计划）：Turn 是会话内第几轮。它与既有
	// sticky 的关系：本实现的会话键（ConversationID）在亲和窗口内始终钉住同
	// 一账号，Turn 递增从不参与选择，仅作为会话轮次的显式标记随请求传递、
	// 供 trace/审计使用——「Turn 递增不换号」由此成立，而不是由 Turn 的数值
	// 触发任何切换逻辑。
	Turn int
	// Within is the D-43 request-scoped affinity window. When > 0 it overrides
	// the pool's ConvStickyTTL for conversation pins: the same session key
	// sticks to the picked account for Within (窗口从首次选定起算，命中不续
	// 期，与既有 sticky 的 keep-expiry 规则一致)。It also lets a caller opt
	// into conversation affinity on a pool whose ConvStickyTTL is 0. Pool-scope
	// (client key) stickiness is unaffected and keeps its pool TTL.
	Within time.Duration
	// Strict is the D-43 strict mode: an expired window or a temporarily
	// unavailable pinned account no longer breaks affinity; only permanent
	// loss (disabled / no credential / not a member / removed from the pool)
	// switches accounts. See sticky_why.go for the exact state set.
	Strict bool
	// P11 group-rule features, computed by the gateway from the request body.
	PromptChars     int
	EstimatedTokens int
	HasImage        bool
	Thinking        bool
	Agent           string
}

// Decision is the answer of Pick.
type Decision struct {
	PluginID     string   `json:"plugin_id"`
	AccountID    string   `json:"account_id"`
	Model        string   `json:"model"`
	LogicalModel string   `json:"logical_model,omitempty"`
	Sticky       bool     `json:"sticky"`
	// StickyWhy is the D-43 seven-state verdict of why the decision stuck to
	// (or left) the session's account; the states are documented in
	// sticky_why.go. Empty only for legacy zero-value Decisions.
	StickyWhy string   `json:"sticky_why,omitempty"`
	Reasons   []string `json:"reasons,omitempty"`
}

// Candidate is one explain row: an account considered (or a pool rejected
// before account selection).
type Candidate struct {
	PluginID  string  `json:"plugin_id"`
	Model     string  `json:"model,omitempty"`
	AccountID string  `json:"account_id,omitempty"`
	Score     float64 `json:"score"`
	Sticky    bool    `json:"sticky,omitempty"`
	// StickyWhy mirrors Decision.StickyWhy on the selected row (D-43).
	StickyWhy string  `json:"sticky_why,omitempty"`
	Selected  bool    `json:"selected"`
	Rejected  bool    `json:"rejected,omitempty"`
	Reason    string  `json:"reason,omitempty"`
}

// Explanation is the body of POST /api/scheduler/explain. Decision is nil
// when every candidate was rejected; Candidates still explains why.
type Explanation struct {
	Decision   *Decision   `json:"decision"`
	InterPool  string      `json:"inter_pool,omitempty"`
	Candidates []Candidate `json:"candidates"`
	Reasons    []string    `json:"reasons,omitempty"`
}

// Options wires time, randomness and persistence into the scheduler.
type Options struct {
	Sticky StickyStore
	// Cooldowns persists request-path account/model cooldowns (nil disables
	// persistence). Writes are buffered by a debounced worker and are flushed
	// best-effort by Close, so Report never blocks on SQLite.
	Cooldowns CooldownStore
	// Log receives the single restore summary line (counts only). Nil is fine.
	Log func(msg string, fields map[string]any)
	Now func() time.Time
	// Sleep replaces the internal wait between RPM-paused re-plans (P5
	// PickWait). Tests inject one that advances the injected clock instead of
	// really sleeping, so the two-minute budget stays instant under test.
	// Nil falls back to a context-aware time.Sleep.
	Sleep    func(time.Duration)
	Rand     func() float64
	EWMAlpha float64
}

// EgressVerdict is the host's recent evidence about one account's assigned
// proxy. Dead is true only for a failure inside the evidence window; Label and
// Error are already user-safe (no credentials).
type EgressVerdict struct {
	Dead  bool
	Label string
	Error string
	At    int64
}

// EgressHealthFunc must be non-blocking: it is called under the scheduler lock
// for every candidate. The host backs it with a short-TTL cache.
type EgressHealthFunc func(pluginID, accountID string) EgressVerdict

// Scheduler owns the in-memory scheduling state (in-flight counters, EWMA
// latency, cooldowns, pool health and the sticky cache).
type Scheduler struct {
	mu sync.Mutex

	stickyStore StickyStore
	nowFn       func() time.Time
	sleepFn     func(time.Duration)
	randFn      func() float64
	ewmAlpha    float64

	inFlight  map[AccountKey]int
	ewma      map[AccountKey]float64
	credits   map[AccountKey]float64
	accountCD map[AccountKey]int64
	modelCD   map[modelCooldownKey]cooldownState
	pool5xx   map[string]int
	circuit   map[string]int64
	// rpmHits logs the recent Acquire timestamps per account for the P5
	// per-minute request cap. The window slides by dropping entries older
	// than rpmWindowSeconds; Acquire is the only writer, so a restarted
	// process starts with an empty window (advisory, in-memory by design).
	rpmHits map[AccountKey][]int64
	// usageHits logs the recent Acquire timestamps per account for the D-25②
	// usage half-life: compositeScores decay each hit by usageHalfLifeSeconds
	// so a just-heavily-used account is briefly demoted. In-memory by design —
	// a restart starts from a neutral usage view.
	usageHits map[AccountKey][]int64

	stickyEntries map[stickyKey]stickyEntry
	stickyLoaded  map[string]bool

	// groupWeightState is the smooth weighted round-robin cursor per group and
	// source (P11): plugin/model → current weight.
	groupWeightState map[string]map[string]int64

	// probeSeq rotates an unmeasured account into the free/pace strategies
	// every probeEvery picks (D-44), so an account with no quota report is not
	// starved forever.
	probeSeq uint64

	// Durable cooldowns. Report enqueues writes on cooldownCh; a single worker
	// coalesces them by key and flushes after cooldownPersistDelay.
	cooldownStore CooldownStore
	cooldownLog   func(msg string, fields map[string]any)
	// eventLog receives operational scheduler events (D-46② strategy panic
	// fallback). Nil disables the logging.
	eventLog     func(msg string, fields map[string]any)
	cooldownCh   chan cooldownWrite
	cooldownStop chan struct{}
	cooldownDone chan struct{}
	cooldownOnce sync.Once // stop is idempotent
	restoreOnce  sync.Once
}

type cooldownRow struct {
	PluginID  string
	AccountID string
	Model     string
}

// cooldownWrite is one buffered upsert, delete or flush barrier. The last write
// for a key wins. A barrier shares the write queue so FIFO order guarantees
// every write enqueued before it has been applied.
type cooldownWrite struct {
	row     cooldownRow
	record  CooldownRecord
	remove  bool
	barrier chan struct{}
}

type modelCooldownKey struct {
	Key   AccountKey
	Model string
}

type cooldownState struct {
	Until   int64
	Strikes int
}

type stickyKey struct {
	Scope string
	Key   string
}

type stickyEntry struct {
	PluginID  string
	AccountID string
	ExpiresAt int64
}

const (
	defaultEWMAlpha   = 0.3
	defaultLatencyMS  = 500.0
	cooldown429Base   = 30 // seconds
	cooldown429Max    = 600
	cooldownServerErr = 60
	circuitThreshold  = 3
	circuitSeconds    = 60

	// rpmWindowSeconds is the sliding window of the P5 per-account request
	// cap: hits older than this drop out and free a slot again.
	rpmWindowSeconds = 60
	// rpmWaitBudget bounds how long PickWait parks a request while every
	// candidate account is paused by its per-minute cap (P5: 最多等 2 分钟,
	// then 429 account_rate_limited).
	rpmWaitBudget = 2 * time.Minute

	// cooldownPersistDelay debounces durable cooldown writes: a burst of
	// reports coalesces into one upsert/delete per account+model key.
	cooldownPersistDelay = 500 * time.Millisecond
	// cooldownPersistBuffer bounds the pending queue. A full queue drops the
	// write instead of blocking the request path; the in-memory cooldown still
	// protects the running process.
	cooldownPersistBuffer = 256
)

// New returns a scheduler. Nil options fields fall back to production values.
func New(opts Options) *Scheduler {
	alpha := opts.EWMAlpha
	if alpha <= 0 || alpha > 1 {
		alpha = defaultEWMAlpha
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	sleep := opts.Sleep
	random := opts.Rand
	if random == nil {
		random = rand.Float64
	}
	s := &Scheduler{
		stickyStore:   opts.Sticky,
		cooldownStore: opts.Cooldowns,
		cooldownLog:   opts.Log,
		eventLog:      opts.Log,
		nowFn:         now,
		sleepFn:       sleep,
		randFn:        random,
		ewmAlpha:      alpha,
		inFlight:      make(map[AccountKey]int),
		ewma:          make(map[AccountKey]float64),
		credits:       make(map[AccountKey]float64),
		accountCD:     make(map[AccountKey]int64),
		modelCD:       make(map[modelCooldownKey]cooldownState),
		pool5xx:       make(map[string]int),
		circuit:       make(map[string]int64),
		rpmHits:       make(map[AccountKey][]int64),
		usageHits:     make(map[AccountKey][]int64),
		stickyEntries: make(map[stickyKey]stickyEntry),
		stickyLoaded:  make(map[string]bool),
	}
	if s.cooldownStore != nil {
		s.cooldownCh = make(chan cooldownWrite, cooldownPersistBuffer)
		s.cooldownStop = make(chan struct{})
		s.cooldownDone = make(chan struct{})
		go s.cooldownWorker()
	}
	return s
}

// Close flushes buffered cooldown writes best-effort, stops the writer and
// drops the in-memory state. Sticky and cooldown rows stay in the store.
func (s *Scheduler) Close() {
	if s == nil {
		return
	}
	s.stopCooldownWorker()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inFlight = make(map[AccountKey]int)
	s.ewma = make(map[AccountKey]float64)
	s.credits = make(map[AccountKey]float64)
	s.accountCD = make(map[AccountKey]int64)
	s.modelCD = make(map[modelCooldownKey]cooldownState)
	s.pool5xx = make(map[string]int)
	s.circuit = make(map[string]int64)
	s.rpmHits = make(map[AccountKey][]int64)
	s.usageHits = make(map[AccountKey][]int64)
	s.stickyEntries = make(map[stickyKey]stickyEntry)
	s.stickyLoaded = make(map[string]bool)
}

// now returns the injected clock.
func (s *Scheduler) now() time.Time {
	if s != nil && s.nowFn != nil {
		return s.nowFn()
	}
	return time.Now()
}

// waitClock pauses until d elapses or ctx ends. Tests replace the sleeper
// (Options.Sleep) with one that advances the injected clock, so PickWait's
// two-minute budget stays instant under test.
func (s *Scheduler) waitClock(ctx context.Context, d time.Duration) {
	if s == nil {
		return
	}
	if s.sleepFn != nil {
		s.sleepFn(d)
		return
	}
	if d <= 0 {
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

// recordRPMHitLocked appends one usage timestamp to the account's window and
// prunes the entries that slid out.
func (s *Scheduler) recordRPMHitLocked(key AccountKey, now int64) {
	s.rpmHits[key] = append(rpmPruneHits(s.rpmHits[key], now), now)
}

// rpmPruneHits drops hits older than the sliding window, reusing the slice.
func rpmPruneHits(hits []int64, now int64) []int64 {
	cutoff := now - rpmWindowSeconds
	keep := hits[:0]
	for _, at := range hits {
		if at > cutoff {
			keep = append(keep, at)
		}
	}
	return keep
}

// rpmCountLocked returns the number of in-window hits for the account.
func (s *Scheduler) rpmCountLocked(key AccountKey, now int64) int {
	hits := rpmPruneHits(s.rpmHits[key], now)
	s.rpmHits[key] = hits
	return len(hits)
}

// rpmWakeUnixLocked returns the unix second at which the account's window
// drops back below its cap. The caller only invokes it for paused accounts
// (count >= rpm): the (count-rpm+1)-th oldest hit must slide out first.
func (s *Scheduler) rpmWakeUnixLocked(key AccountKey, rpm int, now int64) int64 {
	hits := rpmPruneHits(s.rpmHits[key], now)
	s.rpmHits[key] = hits
	if len(hits) == 0 {
		return now
	}
	idx := len(hits) - rpm
	if idx < 0 || idx > len(hits)-1 {
		idx = 0
	}
	return hits[idx] + rpmWindowSeconds
}

// randFloat returns a value in [0,1).
func (s *Scheduler) randFloat() float64 {
	if s == nil || s.randFn == nil {
		return 0.5
	}
	v := s.randFn()
	if v < 0 {
		return 0
	}
	if v >= 1 {
		return math.Nextafter(1, 0)
	}
	return v
}

// Acquire counts one in-flight request for key. Call Release when it ends.
// It also records one hit in the account's RPM window (P5 每分钟请求数) and one
// in the D-25② usage half-life window: every attempt that is about to go
// upstream counts, including retries and the hedged second attempt.
func (s *Scheduler) Acquire(key AccountKey) {
	if s == nil || key.AccountID == "" {
		return
	}
	now := s.now().Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inFlight[key]++
	s.recordRPMHitLocked(key, now)
	s.recordUsageHitLocked(key, now)
}

// Release decrements the in-flight counter without going negative.
func (s *Scheduler) Release(key AccountKey) {
	if s == nil || key.AccountID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := s.inFlight[key]; n <= 1 {
		delete(s.inFlight, key)
	} else {
		s.inFlight[key] = n - 1
	}
}

// InFlight reports the current in-flight count of one account.
func (s *Scheduler) InFlight(key AccountKey) int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inFlight[key]
}

// Credits returns the credits reported through Report for one account.
func (s *Scheduler) Credits(key AccountKey) float64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.credits[key]
}

// CircuitOpen reports whether one pool's circuit breaker is currently open.
func (s *Scheduler) CircuitOpen(pluginID string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.circuitOpenLocked(pluginID, s.now().Unix())
}

func (s *Scheduler) circuitOpenLocked(pluginID string, now int64) bool {
	until := s.circuit[pluginID]
	if until == 0 {
		return false
	}
	if until <= now {
		delete(s.circuit, pluginID)
		return false
	}
	return true
}

// Report records the outcome of one request: EWMA first-byte latency, account
// and model cooldowns, and the pool circuit breaker.
//
// Failure cooldowns are tiered by cause since D-24 (cooldown_policy.go):
//
//	429            -> (plugin,account,model) cooldown 30s doubling to 10m
//	401            -> account cooldown 30m (auth_expired)
//	5xx / 408 / 409 -> account cooldown 60s; 3 consecutive open the pool 60s
//	0 (transport)  -> account cooldown 15s (network)
//	other statuses -> success bookkeeping (resets streaks), as before
func (s *Scheduler) Report(key AccountKey, model string, status int, ttfbMS int64, credits float64) {
	s.ReportCause(key, model, FailureCause{Status: status}, ttfbMS, credits)
}

// ReportCause is Report with the D-24 attribution inputs attached: the error
// text for quota/egress classification, the upstream Retry-After hint and the
// quota window reset time. Callers that only know the status keep using
// Report, which reproduces the pre-D-24 statuses bit for bit.
func (s *Scheduler) ReportCause(key AccountKey, model string, cause FailureCause, ttfbMS int64, credits float64) {
	if s == nil || key.AccountID == "" {
		return
	}
	now := s.now().Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyReportLocked(key, model, cause, ttfbMS, credits, now)
}

// applyReportLocked is the shared body of Report/ReportCause. Caller holds
// s.mu and now is the injected clock's unix second.
func (s *Scheduler) applyReportLocked(key AccountKey, model string, cause FailureCause, ttfbMS int64, credits float64, now int64) {
	if credits > 0 {
		s.credits[key] += credits
	}
	status := cause.Status

	success := status >= 200 && status < 400
	if success {
		if ttfbMS > 0 {
			if old, ok := s.ewma[key]; ok && old > 0 {
				s.ewma[key] = old*(1-s.ewmAlpha) + float64(ttfbMS)*s.ewmAlpha
			} else {
				s.ewma[key] = float64(ttfbMS)
			}
		}
		// A success clears the same rows Report may have persisted. Clearing a
		// key that was never set does not produce a store write.
		if _, ok := s.accountCD[key]; ok {
			delete(s.accountCD, key)
			s.enqueueCooldownDelete(key.PluginID, key.AccountID, "")
		}
		mk := modelCooldownKey{Key: key, Model: model}
		if _, ok := s.modelCD[mk]; ok {
			delete(s.modelCD, mk)
			if model != "" {
				s.enqueueCooldownDelete(key.PluginID, key.AccountID, model)
			}
		}
		s.pool5xx[key.PluginID] = 0
		return
	}

	// D-24: attribute the failure before picking the tier. An empty class
	// keeps the legacy "unclassified failures do not cool down" behavior (a
	// request-shaped 4xx such as 400 must not park the account).
	// D-46①: a NoPenalty report (duplicate error signature inside one dispatch
	// round, i.e. the failure follows the request/IP, not the account) records
	// nothing: no strike counter, no cooldown, no circuit evidence. Returning
	// before any state write keeps the report free of side effects except the
	// credit accumulation above.
	if cause.NoPenalty {
		return
	}
	why := ClassifyCooldown(status, cause.Message)
	if why == "" {
		// A non-5xx answer proves the pool is alive.
		s.pool5xx[key.PluginID] = 0
		return
	}
	by := key.String()

	switch why {
	case CooldownRateLimit:
		// Rate limits are model-specific: another model of the same account
		// may still work.
		mk := modelCooldownKey{Key: key, Model: model}
		state := s.modelCD[mk]
		state.Strikes++
		until := now + int64(cooldown429Backoff(state.Strikes))
		if cause.RetryAfter > 0 {
			// 尊重上游 Retry-After，最多信任 1 小时（05 号文档 longestWait）。
			seconds := int64(cause.RetryAfter / time.Second)
			if seconds <= 0 {
				seconds = 1
			}
			if seconds > cooldownRetryCapSeconds {
				seconds = cooldownRetryCapSeconds
			}
			until = now + seconds
		}
		state.Until = until
		s.modelCD[mk] = state
		// An empty model would collide with the account-wide row (model_id ''),
		// so such a report stays in memory only.
		if model != "" {
			s.enqueueCooldown(CooldownRecord{
				PluginID: key.PluginID, AccountID: key.AccountID, Model: model,
				Until: state.Until, Reason: "status 429", Why: why, By: by,
				Strikes: state.Strikes,
			})
		}
		s.pool5xx[key.PluginID] = 0
	case CooldownUpstreamError, CooldownQuotaExhausted, CooldownAuthExpired,
		CooldownNetwork, CooldownEgress, CooldownOther:
		until := now + accountCooldownSeconds(why, cause, now)
		s.accountCD[key] = until
		s.enqueueCooldown(CooldownRecord{
			PluginID: key.PluginID, AccountID: key.AccountID,
			Until: until, Reason: cooldownReasonText(status, cause, why), Why: why, By: by,
		})
		// The pool circuit breaker keeps its pre-D-24 condition exactly: only
		// 5xx/transport/408 streaks open it. Auth/quota/egress/network-tier
		// cooldowns park the account without blaming the pool.
		if status >= 500 || status == 0 || status == 408 {
			s.pool5xx[key.PluginID]++
			if s.pool5xx[key.PluginID] >= circuitThreshold {
				s.circuit[key.PluginID] = now + circuitSeconds
				s.pool5xx[key.PluginID] = 0
			}
		} else {
			s.pool5xx[key.PluginID] = 0
		}
	}
}

// cooldown429Backoff returns the strike-driven 429 backoff seconds: 30s
// doubling capped at 10 minutes (the pre-D-24 ladder, kept verbatim).
func cooldown429Backoff(strikes int) int64 {
	backoff := int64(cooldown429Base)
	for i := 1; i < strikes && backoff < cooldown429Max; i++ {
		backoff *= 2
	}
	if backoff > cooldown429Max {
		backoff = cooldown429Max
	}
	return backoff
}

// cooldownReasonText renders the durable human reason of a cooldown. The
// historic "status N" strings are kept so old rows and new rows read alike;
// message-driven tiers name their cause.
func cooldownReasonText(status int, cause FailureCause, why string) string {
	switch why {
	case CooldownQuotaExhausted:
		return "quota exhausted"
	case CooldownEgress:
		return "egress rejected"
	case CooldownNetwork:
		return "network error"
	case CooldownRateLimit:
		if cause.RetryAfter > 0 {
			return "status 429 retry-after"
		}
		return "status 429"
	}
	// upstream_error / auth_expired / other keep the historic "status N" form.
	return fmt.Sprintf("status %d", status)
}

// ClearCooldown forgets every request-path cooldown of one account (D-45 手动
// 解除): the account-wide row and all its per-model rows. The clear is
// account-scoped on purpose — the UI button sits on the account row, and
// leaving a per-model row behind would keep the account unpickable for that
// model right after the user asked for a clear. The pool circuit breaker is
// deliberately untouched: it is pool-level health, not per-account state, and
// a hostile pool stays guarded even when one account is pardoned. Unknown
// accounts clear nothing and stay silent.
func (s *Scheduler) ClearCooldown(pluginID, accountID string) {
	if s == nil || accountID == "" {
		return
	}
	key := AccountKey{PluginID: pluginID, AccountID: accountID}
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.accountCD[key]; ok {
		delete(s.accountCD, key)
		s.enqueueCooldownDelete(key.PluginID, key.AccountID, "")
	}
	models := make([]string, 0, 2)
	for mk := range s.modelCD {
		if mk.Key == key {
			models = append(models, mk.Model)
		}
	}
	for _, m := range models {
		delete(s.modelCD, modelCooldownKey{Key: key, Model: m})
		// An empty-model row lives in memory only (Report never persists it),
		// so its delete must not target the account-wide store row.
		if m != "" {
			s.enqueueCooldownDelete(key.PluginID, key.AccountID, m)
		}
	}
}

// ---------------------------------------------------------- durable cooldowns

// RestoreCooldowns loads persisted cooldowns into the in-memory maps. Rows that
// already expired are ignored (the store prunes them on load), so a restart
// never resurrects a finished cooldown. The 429 strike counter is restored too:
// the next 429 continues the doubling instead of restarting at 30s. It is
// idempotent and a no-op when no CooldownStore is wired.
func (s *Scheduler) RestoreCooldowns() {
	if s == nil || s.cooldownStore == nil {
		return
	}
	s.restoreOnce.Do(func() {
		now := s.now().Unix()
		rows, err := s.cooldownStore.LoadCooldowns(now)
		if err != nil {
			return
		}
		accounts, models := 0, 0
		s.mu.Lock()
		for _, row := range rows {
			if row.Until <= now || row.PluginID == "" || row.AccountID == "" {
				continue
			}
			key := AccountKey{PluginID: row.PluginID, AccountID: row.AccountID}
			if row.Model == "" {
				if row.Until > s.accountCD[key] {
					s.accountCD[key] = row.Until
					accounts++
				}
				continue
			}
			mk := modelCooldownKey{Key: key, Model: row.Model}
			state := s.modelCD[mk]
			if row.Until > state.Until {
				state.Until = row.Until
			}
			if row.Strikes > state.Strikes {
				state.Strikes = row.Strikes
			}
			if state.Strikes < 1 {
				// Any persisted model row means at least one 429 happened.
				state.Strikes = 1
			}
			s.modelCD[mk] = state
			models++
		}
		s.mu.Unlock()
		if s.cooldownLog != nil && accounts+models > 0 {
			// Counts only: no account ids or labels.
			s.cooldownLog("scheduler cooldowns restored", map[string]any{
				"count": accounts + models, "accounts": accounts, "models": models,
			})
		}
	})
}

// FlushCooldowns waits until every buffered write has been handed to the store.
// Close calls it implicitly; tests use it to observe the debounced writes.
func (s *Scheduler) FlushCooldowns() {
	if s == nil || s.cooldownStore == nil || s.cooldownCh == nil {
		return
	}
	// The barrier rides the write queue, so FIFO order makes it observe every
	// write enqueued before it.
	ack := make(chan struct{})
	select {
	case s.cooldownCh <- cooldownWrite{barrier: ack}:
	case <-s.cooldownDone:
		return
	}
	select {
	case <-ack:
	case <-s.cooldownDone:
	}
}

// enqueueCooldown buffers one upsert. A full buffer drops the write instead of
// blocking the request path.
func (s *Scheduler) enqueueCooldown(record CooldownRecord) {
	if s == nil || s.cooldownStore == nil || s.cooldownCh == nil {
		return
	}
	select {
	case s.cooldownCh <- cooldownWrite{
		row:    cooldownRow{PluginID: record.PluginID, AccountID: record.AccountID, Model: record.Model},
		record: record,
	}:
	default:
	}
}

// enqueueCooldownDelete buffers one delete.
func (s *Scheduler) enqueueCooldownDelete(pluginID, accountID, model string) {
	if s == nil || s.cooldownStore == nil || s.cooldownCh == nil {
		return
	}
	select {
	case s.cooldownCh <- cooldownWrite{
		row:    cooldownRow{PluginID: pluginID, AccountID: accountID, Model: model},
		remove: true,
	}:
	default:
	}
}

// stopCooldownWorker flushes pending writes and stops the writer. It blocks
// until the flush finished, which is bounded by the store operation timeout.
func (s *Scheduler) stopCooldownWorker() {
	if s == nil || s.cooldownStore == nil || s.cooldownStop == nil {
		return
	}
	s.cooldownOnce.Do(func() {
		close(s.cooldownStop)
		<-s.cooldownDone
	})
}

// cooldownWorker coalesces buffered writes by key and flushes them after a
// short debounce. It exits after a final drain when Close stops it.
func (s *Scheduler) cooldownWorker() {
	defer close(s.cooldownDone)
	pending := make(map[cooldownRow]cooldownWrite)
	var timer *time.Timer
	var timerC <-chan time.Time

	flush := func() {
		for _, write := range pending {
			if write.remove {
				_ = s.cooldownStore.DeleteCooldown(write.row.PluginID, write.row.AccountID, write.row.Model)
			} else {
				_ = s.cooldownStore.SaveCooldown(write.record)
			}
		}
		if len(pending) > 0 {
			pending = make(map[cooldownRow]cooldownWrite)
		}
		if timer != nil {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timerC = nil
		}
	}
	apply := func(write cooldownWrite) {
		pending[write.row] = write
		if timer == nil {
			timer = time.NewTimer(cooldownPersistDelay)
		} else {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(cooldownPersistDelay)
		}
		timerC = timer.C
	}

	for {
		select {
		case write := <-s.cooldownCh:
			if write.barrier != nil {
				flush()
				close(write.barrier)
				continue
			}
			apply(write)
		case <-timerC:
			flush()
		case <-s.cooldownStop:
			for {
				select {
				case write := <-s.cooldownCh:
					if write.barrier != nil {
						flush()
						close(write.barrier)
						continue
					}
					pending[write.row] = write
				default:
					flush()
					return
				}
			}
		}
	}
}

// ---------------------------------------------------------------- health view

// egressDeadReason explains a skipped account in Explain output and retry logs.
func egressDeadReason(v EgressEvidence) string {
	label := strings.TrimSpace(v.Label)
	if label == "" {
		label = "代理"
	}
	reason := "出口不可用：代理「" + label + "」最近失败"
	if msg := strings.TrimSpace(v.Error); msg != "" {
		reason += "（" + msg + "）"
	}
	return reason
}

// SelectableCount reports how many accounts of snap are selectable right now,
// plus the total number of snapshot accounts. It mirrors A's healthz predicate
// (TraeWorkAssistant, MIT; api_server/routes.rs:489-524 -> pool.has_selectable,
// api_server/pool.rs:959-971 and fn selectable at pool.rs:1135-1164): an
// account counts when its pool upstream is enabled, it is a pool member, it is
// enabled with a credential, it is not in an account cooldown, its credits are
// neither expired nor depleted and its plugin-reported status is not one the
// plugin itself treats as unusable (unservableStatusReason). Model cooldowns and
// the pool circuit are request-specific and therefore not part of this
// account-level view.
//
// Egress health belongs to the account-level verdict too: an account whose
// assigned proxy failed inside the evidence window is not counted, because a
// health probe must not report capacity that routing skips.
//
// The method only reads the snapshot, the in-memory cooldown maps and the
// cached egress verdict; unlike Pick it never writes sticky bindings.
func (s *Scheduler) SelectableCount(snap *Snapshot) (selectable, total int) {
	selectable, total, _ = s.HealthCounts(snap)
	return selectable, total
}

// HealthCounts is SelectableCount plus the number of accounts that would be
// selectable but for a dead egress proxy. /healthz reports all three so "no
// account" and "every exit is down" stay distinguishable.
func (s *Scheduler) HealthCounts(snap *Snapshot) (selectable, total, egressFailed int) {
	if s == nil || snap == nil {
		return 0, 0, 0
	}
	now := s.now().Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, pool := range snap.Pools {
		if pool == nil {
			continue
		}
		for _, acc := range pool.Accounts {
			total++
			if !pool.UpstreamEnabled {
				continue
			}
			if pool.Members != nil && !pool.Members[acc.ID] {
				continue
			}
			if !acc.Enabled || !acc.CredentialPresent {
				continue
			}
			if unservableStatusReason(acc) != "" {
				continue
			}
			key := AccountKey{PluginID: pool.PluginID, AccountID: acc.ID}
			if s.accountCooldownLocked(acc, key, now) > now {
				continue
			}
			if creditIneligibleReason(acc, now) != "" {
				continue
			}
			if acc.Egress.Failed {
				egressFailed++
				continue
			}
			selectable++
		}
	}
	return selectable, total, egressFailed
}

// EWMALatency returns the current smoothed first-byte latency of one account.
func (s *Scheduler) EWMALatency(key AccountKey) float64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ewma[key]
}

// ---------------------------------------------------------------- sticky cache

// CachedSticky returns the in-memory sticky entries, sorted by scope and key.
func (s *Scheduler) CachedSticky() []StickyBinding {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]StickyBinding, 0, len(s.stickyEntries))
	for k, v := range s.stickyEntries {
		out = append(out, StickyBinding{
			Scope: k.Scope, Key: k.Key, PluginID: v.PluginID,
			AccountID: v.AccountID, ExpiresAt: v.ExpiresAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// ForgetSticky drops one in-memory pin (the caller already deleted the row).
func (s *Scheduler) ForgetSticky(scope, key string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.stickyEntries, stickyKey{Scope: scope, Key: key})
}

// ForgetAllSticky drops every in-memory pin.
func (s *Scheduler) ForgetAllSticky() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stickyEntries = make(map[stickyKey]stickyEntry)
	s.stickyLoaded = make(map[string]bool)
}

func (s *Scheduler) ensureStickyLocked(scope string) {
	if s.stickyLoaded[scope] {
		return
	}
	s.stickyLoaded[scope] = true
	if s.stickyStore == nil {
		return
	}
	rows, err := s.stickyStore.ListSticky(scope)
	if err != nil {
		return
	}
	for _, row := range rows {
		s.stickyEntries[stickyKey{Scope: row.Scope, Key: row.Key}] = stickyEntry{
			PluginID: row.PluginID, AccountID: row.AccountID, ExpiresAt: row.ExpiresAt,
		}
	}
}

// lookupSticky returns one unexpired entry. Expired rows are deleted lazily.
func (s *Scheduler) lookupSticky(scope, key string, now int64) (stickyEntry, bool) {
	entry, found, expired := s.lookupStickyState(scope, key, now)
	if expired {
		return stickyEntry{}, false
	}
	return entry, found
}

// lookupStickyState is lookupSticky with the D-43 expired/absent distinction:
// it reports whether a row existed at all and whether the row it found had
// already lapsed (the row is deleted lazily, exactly like lookupSticky does).
func (s *Scheduler) lookupStickyState(scope, key string, now int64) (entry stickyEntry, found, expired bool) {
	if key == "" {
		return stickyEntry{}, false, false
	}
	s.ensureStickyLocked(scope)
	e, ok := s.stickyEntries[stickyKey{Scope: scope, Key: key}]
	if !ok {
		return stickyEntry{}, false, false
	}
	if e.ExpiresAt <= now {
		delete(s.stickyEntries, stickyKey{Scope: scope, Key: key})
		if s.stickyStore != nil {
			_ = s.stickyStore.DeleteSticky(scope, key)
		}
		return e, true, true
	}
	return e, true, false
}

func (s *Scheduler) saveSticky(scope, key, pluginID, accountID string, expiresAt int64) {
	if s == nil || key == "" || expiresAt <= 0 {
		return
	}
	s.mu.Lock()
	s.stickyEntries[stickyKey{Scope: scope, Key: key}] = stickyEntry{
		PluginID: pluginID, AccountID: accountID, ExpiresAt: expiresAt,
	}
	store := s.stickyStore
	s.mu.Unlock()
	if store != nil {
		_ = store.SaveSticky(StickyBinding{
			Scope: scope, Key: key, PluginID: pluginID, AccountID: accountID, ExpiresAt: expiresAt,
		})
	}
}

func (s *Scheduler) dropStickyLocked(scope, key string) {
	if key == "" {
		return
	}
	delete(s.stickyEntries, stickyKey{Scope: scope, Key: key})
	if s.stickyStore != nil {
		_ = s.stickyStore.DeleteSticky(scope, key)
	}
}

// ---------------------------------------------------------------- resolution

// resolveModel turns a request model into logical name plus ordered sources.
func resolveModel(snap *Snapshot, model string) (string, []Source, error) {
	model = strings.TrimSpace(model)
	if snap == nil || model == "" {
		return "", nil, fmt.Errorf("%w: %q", ErrUnknownModel, model)
	}
	// A logical binding is a first-class model id even when its name contains
	// a slash. Only treat the name as "<plugin>/<model>" when that plugin
	// actually exists; otherwise "vendor/model" would be advertised but never
	// routable.
	binding, bound := snap.Bindings[model]
	if !bound || len(binding.Items) == 0 {
		if pluginID, physical, ok := strings.Cut(model, "/"); ok &&
			strings.TrimSpace(pluginID) != "" && strings.TrimSpace(physical) != "" {
			return "", []Source{{PluginID: strings.TrimSpace(pluginID), Model: strings.TrimSpace(physical)}}, nil
		}
		return "", nil, fmt.Errorf("%w: %q", ErrUnknownModel, model)
	}
	if pluginID, physical, ok := strings.Cut(model, "/"); ok && snap.Pools[strings.TrimSpace(pluginID)] != nil {
		// An installed plugin owns the prefix; the plugin path takes
		// precedence, matching the gateway's matchPlugin.
		return "", []Source{{PluginID: strings.TrimSpace(pluginID), Model: strings.TrimSpace(physical)}}, nil
	}
	items := make([]Source, 0, len(binding.Items))
	for _, item := range binding.Items {
		if strings.TrimSpace(item.PluginID) == "" || strings.TrimSpace(item.Model) == "" {
			continue
		}
		items = append(items, item)
	}
	if len(items) == 0 {
		return "", nil, fmt.Errorf("%w: %q has no sources", ErrUnknownModel, model)
	}
	return binding.Logical, items, nil
}

// interPoolMode returns the snapshot mode with the documented default.
func interPoolMode(snap *Snapshot) string {
	if snap != nil && ValidInterPool(snap.InterPool) {
		return snap.InterPool
	}
	return InterPoolSmart
}

// ModelMultiplier returns a model's credit multiplier; free models are 0 and
// unknown models score as 1.0 (design §6.2).
func ModelMultiplier(models map[string]model.ModelInfo, modelID string) float64 {
	info, ok := models[modelID]
	if !ok {
		return 1
	}
	if info.Free {
		return 0
	}
	if info.Multiplier > 0 {
		return info.Multiplier
	}
	return 1
}

// ---------------------------------------------------------------- planning

type poolCandidate struct {
	source     Source
	pool       *Pool
	healthy    []Account // pass every filter, including Exclude
	stickyOK   []Account // pass every filter except Exclude (sticky validation)
	selectable []Account
	full       bool
	outcomes   []accountOutcome
}

type accountOutcome struct {
	account    Account
	selectable bool
	reason     string
	// permanent marks blockers that never recover by waiting (disabled / no
	// credential / not a pool member); they veto the P5 RPM wait.
	permanent bool
	// rpmLimited marks a P5 per-minute-cap pause; rpmUntil is the unix second
	// the window slides back below the cap.
	rpmLimited bool
	rpmUntil   int64
}

// poolTrace keeps the outcome of every source, including pools that failed
// before account selection, so Explain can report them.
type poolTrace struct {
	source  Source
	pool    *Pool
	pc      *poolCandidate
	outcome []accountOutcome
	reason  string
}

type plan struct {
	decision       Decision
	candidates     []Candidate
	reasons        []string
	pool           *Pool
	source         Source
	request        Request
	stickyScope    string
	stickyKey      string
	skipConvSticky bool
	skipPoolSticky bool
	// strictRepin marks a D-43 strict hold past the window expiry: the lapsed
	// pin was lazily deleted, so persistSticky must write a fresh pin to keep
	// holding affinity across requests.
	strictRepin bool
	// rpmWaitable marks a failed plan whose only recoverable blocker is the
	// per-account RPM window; rpmWake is the earliest second at which some
	// candidate slides back in (P5 每分钟请求数).
	rpmWaitable bool
	rpmWake     time.Time
}

// Pick selects one account for req. It never mutates snap.
func (s *Scheduler) Pick(ctx context.Context, snap *Snapshot, req Request) (Decision, error) {
	if s == nil {
		return Decision{}, fmt.Errorf("%w: scheduler is nil", ErrNoCandidate)
	}
	p, err := s.plan(snap, req)
	if err != nil {
		return Decision{}, err
	}
	s.persistSticky(p)
	return p.decision, nil
}

// PickWait is Pick with the P5 per-account RPM wait appended: when a plan
// fails only because every candidate account hit its per-minute request cap,
// the call re-plans as the windows slide, at most rpmWaitBudget (2 minutes),
// before returning ErrAccountRateLimited. Without an RPM pause the first plan
// decides, exactly like Pick.
func (s *Scheduler) PickWait(ctx context.Context, snap *Snapshot, req Request) (Decision, error) {
	if s == nil {
		return Decision{}, fmt.Errorf("%w: scheduler is nil", ErrNoCandidate)
	}
	p, err := s.plan(snap, req)
	if err == nil {
		s.persistSticky(p)
		return p.decision, nil
	}
	if !p.rpmWaitable {
		return Decision{}, err
	}
	deadline := s.now().Add(rpmWaitBudget)
	for {
		if ctx.Err() != nil {
			return Decision{}, err
		}
		wake := p.rpmWake
		if wake.After(deadline) {
			return Decision{}, fmt.Errorf("%w: every candidate account is paused by its per-minute request cap", ErrAccountRateLimited)
		}
		s.waitClock(ctx, wake.Sub(s.now()))
		p, err = s.plan(snap, req)
		if err == nil {
			s.persistSticky(p)
			return p.decision, nil
		}
		if !p.rpmWaitable {
			return Decision{}, err
		}
	}
}

// Explain returns the same decision as Pick together with every candidate and
// its rejection reason. It does not create sticky bindings.
func (s *Scheduler) Explain(ctx context.Context, snap *Snapshot, req Request) (Explanation, error) {
	out := Explanation{InterPool: interPoolMode(snap), Candidates: []Candidate{}}
	if s == nil {
		return out, fmt.Errorf("%w: scheduler is nil", ErrNoCandidate)
	}
	p, err := s.plan(snap, req)
	if p != nil {
		out.Candidates = p.candidates
		out.Reasons = p.reasons
	}
	if err != nil {
		return out, err
	}
	decision := p.decision
	out.Decision = &decision
	out.Reasons = decision.Reasons
	return out, nil
}

func (s *Scheduler) plan(snap *Snapshot, req Request) (*plan, error) {
	logical, sources, err := resolveModel(snap, strings.TrimSpace(req.Model))
	if err != nil {
		return nil, err
	}
	mode := interPoolMode(snap)
	nowTime := s.now()
	now := nowTime.Unix()

	s.mu.Lock()
	defer s.mu.Unlock()

	var (
		evaluated  []*poolCandidate
		traces     []*poolTrace
		rejections []string
		forcedKey  string
	)
	if logical != "" {
		if binding, ok := snap.Bindings[logical]; ok {
			if len(binding.Rules) > 0 {
				if items, rule := bindingSourcesForRequest(binding, req, nowTime); rule != "" {
					sources = items
					rejections = append(rejections, "binding rule: "+rule)
				}
			}
			if len(binding.Weights) > 0 {
				sources = s.weightedSourcesLocked(logical, sources, binding.Weights)
				if len(sources) > 0 {
					forcedKey = sources[0].PluginID + "/" + sources[0].Model
				}
			}
		}
	}
	for _, src := range sources {
		pool := snap.Pools[src.PluginID]
		if pool == nil {
			reason := "unknown plugin pool " + src.PluginID
			rejections = append(rejections, reason)
			traces = append(traces, &poolTrace{source: src, reason: reason})
			continue
		}
		if !pool.UpstreamEnabled {
			reason := src.PluginID + ": upstream disabled"
			rejections = append(rejections, reason)
			traces = append(traces, &poolTrace{source: src, pool: pool, reason: reason})
			continue
		}
		if s.circuitOpenLocked(pool.PluginID, now) {
			reason := src.PluginID + ": circuit open"
			rejections = append(rejections, reason)
			traces = append(traces, &poolTrace{source: src, pool: pool, reason: reason})
			continue
		}
		healthy, stickyOK, selectable, full, outcomes := s.evaluateAccountsLocked(pool, src.Model, req, now)
		pc := &poolCandidate{
			source: src, pool: pool, healthy: healthy, stickyOK: stickyOK,
			selectable: selectable, full: full, outcomes: outcomes,
		}
		if len(selectable) == 0 {
			reason := src.PluginID + ": no eligible account"
			var details []string
			for _, oc := range outcomes {
				if oc.reason != "" {
					details = append(details, oc.account.ID+": "+oc.reason)
				}
			}
			if len(details) > 0 {
				reason += " (" + strings.Join(details, ", ") + ")"
			}
			rejections = append(rejections, reason)
			traces = append(traces, &poolTrace{source: src, pool: pool, outcome: outcomes, reason: reason})
			continue
		}
		evaluated = append(evaluated, pc)
		traces = append(traces, &poolTrace{source: src, pool: pool, pc: pc, outcome: outcomes})
	}
	if len(evaluated) == 0 {
		reasons := rejections
		if len(reasons) == 0 {
			reasons = []string{"no candidate pool"}
		}
		p := &plan{candidates: traceCandidates(traces, nil, Account{}, false, StickyNoSession, s, now), reasons: reasons}
		p.rpmWaitable, p.rpmWake = rpmWaitWindow(traces)
		return p, fmt.Errorf("%w: %s", ErrNoCandidate, strings.Join(reasons, "; "))
	}

	// Fixed keeps the configured source order (binding items / pool priority);
	// smart reorders by the §6.2 keys; cheap puts the lowest configured price
	// first and falls back to smart for unknown/equal prices (D-44).
	switch mode {
	case InterPoolFixed:
	case InterPoolCheap:
		sort.SliceStable(evaluated, func(i, j int) bool {
			pi, oki := snapshotPrice(snap, evaluated[i].source)
			pj, okj := snapshotPrice(snap, evaluated[j].source)
			if oki && okj && pi != pj {
				return pi < pj
			}
			if oki != okj {
				return oki
			}
			return s.poolLessLocked(evaluated[i], evaluated[j], now)
		})
	default:
		sort.SliceStable(evaluated, func(i, j int) bool {
			return s.poolLessLocked(evaluated[i], evaluated[j], now)
		})
	}
	// Smart ordering would otherwise undo the weighted rotation: put the source
	// the group chose this round back in front.
	if forcedKey != "" {
		for i := range evaluated {
			key := evaluated[i].source.PluginID + "/" + evaluated[i].source.Model
			if key != forcedKey || i == 0 {
				continue
			}
			chosen := evaluated[i]
			copy(evaluated[1:i+1], evaluated[:i])
			evaluated[0] = chosen
			break
		}
	}

	// Sticky lookup: conversation first, then pool (model + client key).
	// D-43 wraps the pre-existing pin mechanics in the seven-state StickyWhy
	// verdict (sticky_why.go): every branch below records why the pin was
	// honored, held strict, skipped for one attempt, or dropped.
	var (
		stickyPC       *poolCandidate
		stickyAcc      Account
		stickyKind     string
		stickyKey      string
		skipConvSticky bool
		skipPoolSticky bool
		strictRepin    bool
		stickyWhy      = StickyNoSession
	)
	conv := strings.TrimSpace(req.ConversationID)
	if conv != "" {
		v := s.resolveStickyLocked(evaluated, StickyScopeConversation, conv, true, req, now)
		if v.pc != nil {
			stickyPC, stickyAcc, stickyKind, stickyKey = v.pc, v.acc, StickyScopeConversation, conv
		}
		skipConvSticky = v.skip
		strictRepin = v.repin
		stickyWhy = v.why
		if v.rejection != "" {
			rejections = append(rejections, v.rejection)
		}
	}
	if stickyPC == nil {
		if clientKey := strings.TrimSpace(req.ClientKey); clientKey != "" {
			poolKey := poolStickyKey(req.Model, clientKey)
			v := s.resolveStickyLocked(evaluated, StickyScopePool, poolKey, false, req, now)
			if v.pc != nil {
				stickyPC, stickyAcc, stickyKind, stickyKey = v.pc, v.acc, StickyScopePool, poolKey
				strictRepin = v.repin
			}
			skipPoolSticky = v.skip
			// The pin that was actually used decides; otherwise the more
			// specific verdict wins (sticky_why.go rank).
			if v.pc != nil || stickyWhyMoreSpecific(stickyWhy, v.why) {
				stickyWhy = v.why
			}
			if v.rejection != "" {
				rejections = append(rejections, v.rejection)
			}
		}
	}

	// Sticky yield under saturation (A pool.rs:704-790): a pinned conversation
	// must not overrun its account's concurrency cap while another account of the
	// pool sits idle — concurrency health outranks upstream KV-cache reuse. The
	// pin itself is kept when no idle alternative exists: waiting on the pinned
	// account beats breaking affinity. A yield re-pins to the account that serves
	// (persistSticky below), mirroring A's router-side CAS.
	if stickyPC != nil && s.accountAtLimitLocked(stickyPC.pool, stickyAcc) {
		if s.hasIdleAlternativeLocked(stickyPC, stickyAcc) {
			rejections = append(rejections, "sticky yield: "+stickyAcc.ID+
				" at concurrency limit, idle alternative exists")
			stickyPC, stickyAcc, stickyKind, stickyKey = nil, Account{}, "", ""
			// D-43: the pinned account is unusable for this decision, so the
			// verdict says why the affinity was left (strict never overrides
			// the concurrency-yield protection; A pool.rs:704-790 outranks it).
			stickyWhy = StickyAccountDown
		} else {
			rejections = append(rejections, "sticky fallback: all accounts busy")
		}
	}

	// Choose the pool and account.
	var (
		chosenPC  *poolCandidate
		chosenAcc Account
	)
	if stickyPC != nil {
		chosenPC = stickyPC
		chosenAcc = stickyAcc
	} else {
		chosenPC = evaluated[0]
		idx, _ := s.selectAccountLocked(chosenPC, now)
		chosenAcc = chosenPC.selectable[idx]
	}

	decision := Decision{
		PluginID:     chosenPC.pool.PluginID,
		AccountID:    chosenAcc.ID,
		Model:        chosenPC.source.Model,
		LogicalModel: logical,
		Sticky:       stickyPC != nil,
		StickyWhy:    stickyWhy,
	}
	decision.Reasons = append(decision.Reasons, "sticky_why="+stickyWhy)
	if stickyPC != nil {
		decision.Reasons = append(decision.Reasons, "sticky:"+stickyKind)
	} else {
		strategy := chosenPC.pool.Strategy
		if !ValidStrategy(strategy) {
			strategy = StrategyCreditFirst
		}
		decision.Reasons = append(decision.Reasons, "strategy="+strategy)
		if chosenPC.full {
			decision.Reasons = append(decision.Reasons, "all accounts at concurrency limit; least loaded")
		}
	}
	decision.Reasons = append(decision.Reasons,
		fmt.Sprintf("inter_pool=%s rank=%d", mode, poolRank(evaluated, chosenPC)))
	if chosenAcc.ExpiresAt > 0 {
		decision.Reasons = append(decision.Reasons, fmt.Sprintf("expires_at=%d", chosenAcc.ExpiresAt))
	}
	decision.Reasons = append(decision.Reasons, fmt.Sprintf("remaining=%.1f", chosenAcc.Remaining))
	if len(rejections) > 0 {
		decision.Reasons = append(decision.Reasons, rejections...)
	}

	candidates := traceCandidates(traces, chosenPC, chosenAcc, stickyPC != nil, stickyWhy, s, now)

	return &plan{
		decision: decision, candidates: candidates, reasons: decision.Reasons,
		pool: chosenPC.pool, source: chosenPC.source, request: req,
		stickyScope: stickyKind, stickyKey: stickyKey,
		skipConvSticky: skipConvSticky, skipPoolSticky: skipPoolSticky,
		strictRepin: strictRepin,
	}, nil
}

// persistSticky writes the conversation/pool pins after a successful Pick.
func (s *Scheduler) persistSticky(p *plan) {
	if p == nil || p.pool == nil {
		return
	}
	now := s.now().Unix()
	if p.stickyScope != "" {
		if !p.strictRepin {
			// A sticky hit keeps the existing expiry; do not extend it.
			return
		}
		// D-43 strict hold past expiry: the lapsed pin was already lazily
		// deleted by the lookup, so holding affinity across requests needs a
		// fresh write with the scope's effective window.
		s.saveSticky(p.stickyScope, p.stickyKey, p.pool.PluginID, p.decision.AccountID,
			now+int64(p.effectiveTTL(p.stickyScope == StickyScopeConversation)))
		return
	}
	if conv := strings.TrimSpace(p.request.ConversationID); conv != "" &&
		p.effectiveTTL(true) > 0 && !p.skipConvSticky {
		s.saveSticky(StickyScopeConversation, conv, p.pool.PluginID, p.decision.AccountID,
			now+int64(p.effectiveTTL(true)))
	}
	if clientKey := strings.TrimSpace(p.request.ClientKey); clientKey != "" &&
		p.pool.PoolStickyTTL > 0 && !p.skipPoolSticky {
		s.saveSticky(StickyScopePool, poolStickyKey(p.request.Model, clientKey),
			p.pool.PluginID, p.decision.AccountID, now+int64(p.pool.PoolStickyTTL))
	}
}

// effectiveTTL returns the affinity window of one scope in seconds. The D-43
// request-scoped Within overrides the pool's ConvStickyTTL for the
// conversation scope; the pool scope keeps its configured TTL.
func (p *plan) effectiveTTL(conversation bool) int {
	if !conversation {
		return p.pool.PoolStickyTTL
	}
	if p.request.Within > 0 {
		return int(p.request.Within / time.Second)
	}
	return p.pool.ConvStickyTTL
}

// poolStickyKey builds the pool-sticky key: model + client key.
func poolStickyKey(model, clientKey string) string {
	return strings.TrimSpace(model) + "\x00" + strings.TrimSpace(clientKey)
}

// matchSticky finds entry's pool among candidates and validates the account.
// It returns the pool candidate (nil when the pool is absent from candidates
// or the account is not usable right now), the account, the effective TTL
// (0 means stickiness is disabled for that scope), whether the account is
// merely excluded for this attempt (the pin must survive such a retry) and
// whether the pinned pool itself is among the candidates. The D-43
// request-scoped Within overrides the pool's conversation TTL.
func matchSticky(candidates []*poolCandidate, entry stickyEntry, conversation bool, req Request) (pc *poolCandidate, acc Account, ttl int, excluded bool, poolFound bool) {
	for _, c := range candidates {
		if c.pool.PluginID != entry.PluginID {
			continue
		}
		poolFound = true
		ttl = c.pool.PoolStickyTTL
		if conversation {
			ttl = c.pool.ConvStickyTTL
			if req.Within > 0 {
				ttl = int(req.Within / time.Second)
			}
		}
		for _, a := range c.stickyOK {
			if a.ID != entry.AccountID {
				continue
			}
			key := AccountKey{PluginID: c.pool.PluginID, AccountID: a.ID}
			return c, a, ttl, req.Exclude[key], true
		}
		return nil, Account{}, ttl, false, true
	}
	return nil, Account{}, 0, false, false
}

// stickyVerdict is the D-43 outcome of evaluating one persisted pin. A nil pc
// means the pin did not produce a selection for this decision.
type stickyVerdict struct {
	pc   *poolCandidate
	acc  Account
	why  string // one of the sticky_why.go states
	skip bool   // pin alive; its account is excluded for this attempt only
	// repin marks a strict hold past window expiry: the lapsed pin was already
	// deleted, so the caller must persist a fresh one to keep holding.
	repin bool
	// rejection is the human-readable note for decision.Reasons.
	rejection string
}

// resolveStickyLocked looks up one persisted pin and classifies the outcome
// into a stickyVerdict. conversation selects the scope's TTL rules (and the
// D-43 Within override); req.Strict enables the hold-past-blocker mode.
// Caller holds s.mu.
func (s *Scheduler) resolveStickyLocked(evaluated []*poolCandidate, scope, key string, conversation bool, req Request, now int64) stickyVerdict {
	if key == "" {
		return stickyVerdict{why: StickyNoSession}
	}
	label := "pool"
	if conversation {
		label = "conv"
	}
	entry, found, expired := s.lookupStickyState(scope, key, now)
	if expired {
		// The window lapsed and the row was lazily deleted: reselect, unless
		// strict mode holds the pin for one more window.
		if req.Strict {
			if pc, acc, ok := strictHoldTarget(evaluated, entry); ok {
				return stickyVerdict{
					pc: pc, acc: acc, why: StickyStrictHold, repin: true,
					rejection: "sticky strict hold: window expired, affinity kept",
				}
			}
		}
		return stickyVerdict{why: StickyWindowExpired}
	}
	if !found {
		return stickyVerdict{why: StickyFirstPick}
	}
	pc, acc, ttl, excluded, poolFound := matchSticky(evaluated, entry, conversation, req)
	// The pool-missing check must precede the ttl check: a pool that is absent
	// from the candidates never supplies a TTL, so a ttl-first ordering would
	// mask StickyPoolChanged behind the "stickiness disabled" verdict and make
	// the pool_changed state unreachable (D-43 bug fix).
	switch {
	case !poolFound:
		// The pin's pool left the candidate set (disabled upstream, open
		// circuit, whole pool rejected): affinity never crosses pools.
		s.dropStickyLocked(scope, key)
		return stickyVerdict{why: StickyPoolChanged, rejection: "sticky " + label + " binding invalid; dropped"}
	case ttl <= 0:
		// Stickiness is disabled for this scope (TTL 0 and no Within): the
		// stale pin cannot be honored. A zero-length window reads as expired.
		s.dropStickyLocked(scope, key)
		return stickyVerdict{why: StickyWindowExpired, rejection: "sticky " + label + " binding invalid; dropped"}
	case excluded:
		// A retry exclusion is per attempt: keep the pin for the next request
		// but do not select it now.
		return stickyVerdict{
			skip: true, why: StickyAccountDown,
			rejection: "sticky " + label + " account excluded for this attempt",
		}
	case pc != nil:
		return stickyVerdict{pc: pc, acc: acc, why: StickySessionPinned}
	case req.Strict:
		// The pinned account is temporarily unusable (cooldown, RPM pause,
		// soft credit/egress filter). Strict keeps the affinity; only a
		// permanent blocker breaks it.
		if holdPC, holdAcc, ok := strictHoldTarget(evaluated, entry); ok {
			return stickyVerdict{
				pc: holdPC, acc: holdAcc, why: StickyStrictHold,
				rejection: "sticky strict hold: pinned account temporarily unavailable",
			}
		}
	}
	// Not strict (or strict could not hold): drop the pin and reselect.
	s.dropStickyLocked(scope, key)
	return stickyVerdict{why: StickyAccountDown, rejection: "sticky " + label + " binding invalid; dropped"}
}

// strictHoldTarget locates the pinned account inside the evaluated pools while
// ignoring temporary filters (cooldowns, RPM pauses, soft credit/egress
// filters, concurrency). It returns ok=false when the account is permanently
// unusable (disabled / no credential / not a member / removed from the pool
// snapshot) — the only conditions D-43 strict affinity yields to.
func strictHoldTarget(evaluated []*poolCandidate, entry stickyEntry) (*poolCandidate, Account, bool) {
	for _, pc := range evaluated {
		if pc.pool.PluginID != entry.PluginID {
			continue
		}
		for _, oc := range pc.outcomes {
			if oc.account.ID != entry.AccountID {
				continue
			}
			if oc.permanent {
				return nil, Account{}, false
			}
			return pc, oc.account, true
		}
		// The pool matched but the account vanished from its snapshot.
		return nil, Account{}, false
	}
	return nil, Account{}, false
}

// evaluateAccountsLocked filters one pool's accounts and applies concurrency.
// stickyOK keeps accounts that only fail the per-attempt Exclude filter so a
// retry never destroys a sticky pin.
func (s *Scheduler) evaluateAccountsLocked(pool *Pool, modelID string, req Request, now int64) (healthy, stickyOK, selectable []Account, full bool, outcomes []accountOutcome) {
	limit := pool.AccountConcurrency
	// Hard account filters first: a member that is disabled, credential-less,
	// cooling down or RPM-paused is not a candidate at all.
	candidates := make([]Account, 0, len(pool.Accounts))
	for _, acc := range pool.Accounts {
		key := AccountKey{PluginID: pool.PluginID, AccountID: acc.ID}
		reason := ""
		oc := accountOutcome{account: acc, selectable: false}
		switch {
		case pool.Members != nil && !pool.Members[acc.ID]:
			reason, oc.permanent = "not a pool member", true
		case !acc.Enabled:
			reason, oc.permanent = "account disabled", true
		case !acc.CredentialPresent:
			reason, oc.permanent = "no credential", true
		case s.accountCooldownLocked(acc, key, now) > now:
			reason = "account cooldown until " + unixString(s.accountCooldownLocked(acc, key, now))
		case s.modelCooldownLocked(acc, key, modelID, now) > now:
			reason = "model cooldown until " + unixString(s.modelCooldownLocked(acc, key, modelID, now))
		case acc.RPM > 0 && s.rpmCountLocked(key, now) >= acc.RPM:
			// P5 每分钟请求数: the sliding 60s window already holds the cap;
			// the account becomes selectable again when the oldest surplus
			// hits slide out.
			oc.rpmLimited = true
			oc.rpmUntil = s.rpmWakeUnixLocked(key, acc.RPM, now)
			reason = fmt.Sprintf("rpm limit %d/min until %s", acc.RPM, unixString(oc.rpmUntil))
		}
		if reason != "" {
			oc.reason = reason
			outcomes = append(outcomes, oc)
			continue
		}
		candidates = append(candidates, acc)
	}

	// Credit eligibility (A pool.rs:1135-1164): a known-depleted or known-expired
	// account yields to a funded one. It is a soft filter — when every candidate
	// is credit-ineligible they stay selectable, because a total outage is worse
	// than calling an account that may still work (A keeps zero-credit accounts
	// only out of the candidate set while others exist).
	//
	// The plugin-reported unservable status rides the same soft filter: an
	// account the plugin itself calls expired/invalid yields to a usable one,
	// but a pool where every account carries that status still gets an attempt
	// rather than a hard 503 — the status can be stale in the cached snapshot,
	// and SelectableCount (health) is the place that reports "known-good count".
	eligible := make(map[string]bool, len(candidates))
	eligibleCount := 0
	for _, acc := range candidates {
		if creditIneligibleReason(acc, now) == "" && unservableStatusReason(acc) == "" {
			eligible[acc.ID] = true
			eligibleCount++
		}
	}
	failOpenCredits := eligibleCount == 0

	// Egress is the same kind of soft filter: an account whose assigned proxy
	// is known-dead yields to one whose exit works. When every otherwise-usable
	// candidate is egress-dead the filter fails open — the evidence can be
	// stale, and a hard "no candidate" would turn a recoverable proxy problem
	// into a total outage.
	egressDead := make(map[string]EgressEvidence)
	egressChecked := 0
	for _, acc := range candidates {
		if !failOpenCredits && !eligible[acc.ID] {
			continue
		}
		egressChecked++
		if acc.Egress.Failed {
			egressDead[acc.ID] = acc.Egress
		}
	}
	failOpenEgress := egressChecked > 0 && len(egressDead) == egressChecked

	for _, acc := range candidates {
		key := AccountKey{PluginID: pool.PluginID, AccountID: acc.ID}
		if !failOpenCredits && !eligible[acc.ID] {
			reason := creditIneligibleReason(acc, now)
			if reason == "" {
				reason = unservableStatusReason(acc)
			}
			outcomes = append(outcomes, accountOutcome{
				account: acc, selectable: false, reason: reason,
			})
			continue
		}
		if verdict, dead := egressDead[acc.ID]; dead && !failOpenEgress {
			outcomes = append(outcomes, accountOutcome{
				account: acc, selectable: false, reason: egressDeadReason(verdict),
			})
			continue
		}
		stickyOK = append(stickyOK, acc)
		if req.Exclude[key] {
			outcomes = append(outcomes, accountOutcome{account: acc, selectable: false, reason: "excluded for this attempt"})
			continue
		}
		healthy = append(healthy, acc)
	}
	if len(healthy) == 0 {
		return nil, stickyOK, nil, false, outcomes
	}

	if limit <= 0 {
		selectable = append(selectable, healthy...)
		for _, acc := range healthy {
			outcomes = append(outcomes, accountOutcome{account: acc, selectable: true})
		}
		return healthy, stickyOK, selectable, false, outcomes
	}

	minLoad := math.MaxInt
	for _, acc := range healthy {
		if n := s.inFlight[accKey(pool.PluginID, acc.ID)]; n < minLoad {
			minLoad = n
		}
	}
	// Prefer accounts below the limit; when every account is full, fall back
	// to the least-loaded ones (§6.4).
	available := make([]Account, 0, len(healthy))
	for _, acc := range healthy {
		if s.inFlight[accKey(pool.PluginID, acc.ID)] < limit {
			available = append(available, acc)
		}
	}
	if len(available) > 0 {
		selectable = available
	} else {
		full = true
		for _, acc := range healthy {
			if s.inFlight[accKey(pool.PluginID, acc.ID)] == minLoad {
				selectable = append(selectable, acc)
			}
		}
	}
	selected := make(map[string]bool, len(selectable))
	for _, acc := range selectable {
		selected[acc.ID] = true
	}
	for _, acc := range healthy {
		outcome := accountOutcome{account: acc, selectable: selected[acc.ID]}
		if !selected[acc.ID] {
			outcome.reason = "concurrency full"
		}
		outcomes = append(outcomes, outcome)
	}
	return healthy, stickyOK, selectable, full, outcomes
}

// selectAccountLocked applies the pool strategy to the selectable accounts.
func (s *Scheduler) selectAccountLocked(pc *poolCandidate, now int64) (int, []float64) {
	scores := s.compositeScores(pc.pool, pc.selectable, now)
	return s.strategyIndexLocked(pc, now, scores), scores
}

// strategyIndexLocked runs the in-pool strategy over the selectable accounts
// (kept in pool.Accounts order) and returns the winning index. It is the
// D-46② fail-open boundary: a panic inside a strategy comparison (a future
// strategy, a bad numeric edge in a less function, a panicking rand hook) is
// contained here, logged once, and answered with the unweighted original
// order — selectable[0] — instead of crashing the request goroutine. The
// selection layer must not turn a scheduling bug into a dead request; the
// panic is a routing-quality bug, and serving in configured order is the
// closest safe answer.
func (s *Scheduler) strategyIndexLocked(pc *poolCandidate, now int64, scores []float64) (idx int) {
	defer func() {
		if r := recover(); r != nil {
			idx = 0
			if s != nil && s.eventLog != nil {
				plugin, strategy := "", ""
				if pc != nil && pc.pool != nil {
					plugin = pc.pool.PluginID
					strategy = pc.pool.Strategy
				}
				s.eventLog("scheduler strategy panic fallback", map[string]any{
					"plugin":   plugin,
					"strategy": strategy,
					"panic":    fmt.Sprintf("%v", r),
				})
			}
		}
	}()
	accounts := pc.selectable
	strategy := pc.pool.Strategy
	if !ValidStrategy(strategy) {
		strategy = StrategyCreditFirst
	}
	switch strategy {
	case StrategyRandom:
		return uniformIndex(s.randFloat(), len(accounts))
	case StrategyWeighted3:
		return weightedIndex(scores, s.randFloat())
	case StrategyP2C:
		return p2cIndex(scores, s.randFloat(), s.randFloat())
	case StrategyRemainingFirst:
		return indexByLess(accounts, func(a, b Account) bool {
			if a.Remaining != b.Remaining {
				return a.Remaining > b.Remaining
			}
			return expiryLess(a.ExpiresAt, b.ExpiresAt)
		})
	case StrategyExpiringFirst:
		return indexByLess(accounts, expiringFirstLess)
	case StrategyFree:
		if idx, ok := s.probePickLocked(accounts); ok {
			return idx
		}
		return indexByLess(accounts, freeFirstLess)
	case StrategyPace:
		if idx, ok := s.probePickLocked(accounts); ok {
			return idx
		}
		return indexByLess(accounts, func(a, b Account) bool {
			return paceLess(a, b, now)
		})
	case StrategyFixed:
		return indexByLess(accounts, func(a, b Account) bool {
			if a.Priority != b.Priority {
				return a.Priority < b.Priority
			}
			// D-29: Priority 相同才按 Weight 排序（Weight=0 视为中性 1，等权
			// 时仍按 ID，维持旧顺序）。
			if wa, wb := effectiveWeight(a.Weight), effectiveWeight(b.Weight); wa != wb {
				return wa > wb
			}
			return a.ID < b.ID
		})
	default: // credit_first
		return indexByLess(accounts, func(a, b Account) bool {
			sa, ka := expiryOf(a.ExpiresAt)
			sb, kb := expiryOf(b.ExpiresAt)
			if sa != sb {
				return sa < sb
			}
			if ka != kb {
				return ka < kb
			}
			if a.Remaining != b.Remaining {
				return a.Remaining > b.Remaining
			}
			if a.Priority != b.Priority {
				return a.Priority < b.Priority
			}
			// D-29: 同 Priority 的 credit_first 也让权重发言（降序）。
			if wa, wb := effectiveWeight(a.Weight), effectiveWeight(b.Weight); wa != wb {
				return wa > wb
			}
			return a.ID < b.ID
		})
	}
}

// compositeScores computes the weighted3 score of every account (§6.3). The
// base urgency/ratio/latency blend is multiplied by the D-29 account weight
// (equal-priority ties only, see score_factors.go) and the D-25② usage
// half-life factor (recently used accounts are briefly demoted). Both factors
// are 1 when they do not apply, so legacy scores stay bit-identical.
func (s *Scheduler) compositeScores(pool *Pool, accounts []Account, now int64) []float64 {
	scores := make([]float64, len(accounts))
	if len(accounts) == 0 {
		return scores
	}
	adv := make([]float64, len(accounts))
	maxAdv := 0.0
	for i, acc := range accounts {
		latency := s.ewma[accKey(pool.PluginID, acc.ID)]
		if latency <= 0 {
			latency = defaultLatencyMS
		}
		raw := 1 / math.Max(1, latency)
		adv[i] = raw
		if raw > maxAdv {
			maxAdv = raw
		}
	}
	weights := weightFactors(accounts)
	for i, acc := range accounts {
		urgency := expiryUrgency(acc.ExpiresAt, now)
		ratio := remainingRatio(acc.Remaining, acc.Total)
		latency := 0.0
		if maxAdv > 0 {
			latency = adv[i] / maxAdv
		}
		usage := s.decayedUsageLocked(accKey(pool.PluginID, acc.ID), now)
		scores[i] = (0.45*urgency + 0.40*ratio + 0.15*latency) * weights[i] * usageFactor(usage)
	}
	return scores
}

// accountScoreLocked scores one account in isolation (explain rows of rejected
// accounts, where no comparison set exists). The D-29 weight is not applied
// here: it is a comparison-set factor, not an absolute one.
func (s *Scheduler) accountScoreLocked(pluginID string, acc Account, now int64) float64 {
	latency := s.ewma[accKey(pluginID, acc.ID)]
	if latency <= 0 {
		latency = defaultLatencyMS
	}
	adv := 1 / math.Max(1, latency)
	base := 0.45*expiryUrgency(acc.ExpiresAt, now) +
		0.40*remainingRatio(acc.Remaining, acc.Total) +
		0.15*adv
	return base * usageFactor(s.decayedUsageLocked(accKey(pluginID, acc.ID), now))
}

// traceCandidates renders the explain rows for every traced source. Rejected
// pools get one row of their own plus a row per rejected account. stickyWhy is
// the D-43 verdict attached to the selected row.
func traceCandidates(traces []*poolTrace, chosenPC *poolCandidate, chosenAcc Account, sticky bool, stickyWhy string, s *Scheduler, now int64) []Candidate {
	out := make([]Candidate, 0, len(traces)*2)
	for _, tr := range traces {
		if tr.pc == nil {
			out = append(out, Candidate{
				PluginID: tr.source.PluginID, Model: tr.source.Model,
				Rejected: true, Reason: tr.reason,
			})
			for _, oc := range tr.outcome {
				out = append(out, Candidate{
					PluginID: tr.source.PluginID, Model: tr.source.Model,
					AccountID: oc.account.ID,
					Score:     roundScore(s.accountScoreLocked(tr.source.PluginID, oc.account, now)),
					Rejected:  true, Reason: oc.reason,
				})
			}
			continue
		}
		scores := s.compositeScores(tr.pc.pool, tr.pc.healthy, now)
		scoreByID := make(map[string]float64, len(tr.pc.healthy))
		for i, acc := range tr.pc.healthy {
			scoreByID[acc.ID] = scores[i]
		}
		for _, oc := range tr.pc.outcomes {
			score, ok := scoreByID[oc.account.ID]
			if !ok {
				score = s.accountScoreLocked(tr.pc.pool.PluginID, oc.account, now)
			}
			row := Candidate{
				PluginID:  tr.pc.pool.PluginID,
				Model:     tr.pc.source.Model,
				AccountID: oc.account.ID,
				Score:     roundScore(score),
			}
			switch {
			case tr.pc == chosenPC && oc.account.ID == chosenAcc.ID:
				row.Selected = true
				row.Sticky = sticky
				row.StickyWhy = stickyWhy
			case !oc.selectable:
				row.Rejected = true
				row.Reason = oc.reason
			case tr.pc != chosenPC:
				row.Rejected = true
				row.Reason = "pool not selected"
			default:
				row.Reason = "lower score than " + chosenAcc.ID
			}
			out = append(out, row)
		}
	}
	return out
}

func (s *Scheduler) accountCooldownLocked(acc Account, key AccountKey, now int64) int64 {
	until := acc.CooldownUntil
	if v := s.accountCD[key]; v > until {
		until = v
	}
	return until
}

// creditIneligibleReason ports A's credit filters of `selectable`
// (TraeWorkAssistant, MIT, 朱天伟; api_server/pool.rs:1135-1164, issues #91/#102):
// expired credits are skipped ("expire_time=0 视为无过期时间，不跳过") and a
// depleted balance is not scheduled while a funded account exists.
//
// A keeps credits as Option<f64> plus a snapshot freshness stamp, so it can tell
// "unknown" from 0. This Account snapshot has no such marker (and the app-side
// producer is outside this port), so the zero struct — Remaining 0, Total 0,
// ExpiresAt 0 — is treated as unknown and stays eligible (fail open). A non-zero
// Total or ExpiresAt proves the snapshot carried quota data, which makes
// Remaining <= 0 a real depletion. The reserve-floor setting does not exist on
// Pool today, so only the zero-balance floor is applied.
func creditIneligibleReason(acc Account, now int64) string {
	if acc.ExpiresAt > 0 && acc.ExpiresAt <= now {
		return "credits expired at " + unixString(acc.ExpiresAt)
	}
	if !acc.Unlimited && acc.Remaining <= 0 && (acc.Total > 0 || acc.ExpiresAt > 0) {
		return "credits depleted"
	}
	return ""
}

// unservableStatusReason reports why the plugin-reported account status (GET
// /accounts "status", vocabulary ok/expired/invalid/rate_limited/unknown) rules
// an account out of the health view. Only the two statuses the plugin itself
// treats as unusable are listed: "expired" (dead credential; minimax, gemini,
// cline, codearts) and "invalid" (credential the plugin refuses to use;
// codearts chat_accounts.go, officeace lifecycle.go). "ok", "rate_limited"
// (a cooldown the host already tracks) and "unknown" stay selectable; whether an
// account is switched off is reported by Enabled, not by status.
func unservableStatusReason(acc Account) string {
	switch strings.ToLower(strings.TrimSpace(acc.Status)) {
	case "expired":
		return "account expired (plugin status)"
	case "invalid":
		return "account invalid (plugin status)"
	}
	return ""
}

// accountAtLimitLocked reports whether acc already reached the pool's
// per-account concurrency cap (limit <= 0 disables the cap).
func (s *Scheduler) accountAtLimitLocked(pool *Pool, acc Account) bool {
	limit := pool.AccountConcurrency
	if limit <= 0 {
		return false
	}
	return s.inFlight[accKey(pool.PluginID, acc.ID)] >= limit
}

// rpmWaitWindow inspects the trace outcomes of a failed plan: waiting is
// worthwhile when at least one candidate is paused by its per-minute request
// cap and no account is blocked by a permanent filter (disabled / no
// credential / not a member) — those never recover, so the request would fail
// however long it waits. Recoverable non-RPM blockers (cooldowns, per-attempt
// exclusions) do not veto the wait: their windows slide too, and the
// caller's budget bounds the total wait anyway.
func rpmWaitWindow(traces []*poolTrace) (bool, time.Time) {
	waitable := false
	var wake time.Time
	for _, tr := range traces {
		for _, oc := range tr.outcome {
			if oc.selectable || oc.permanent {
				// A failed plan never has selectable accounts; treat either
				// as "no point waiting" to stay on the safe side.
				return false, time.Time{}
			}
			if oc.rpmLimited {
				waitable = true
				until := time.Unix(oc.rpmUntil, 0)
				if wake.IsZero() || until.Before(wake) {
					wake = until
				}
			}
		}
	}
	if !waitable {
		return false, time.Time{}
	}
	// Re-plan one second past the slide so the window boundary is crossed.
	return true, wake.Add(time.Second)
}

// hasIdleAlternativeLocked reports whether another selectable account of the
// candidate pool is below its cap right now (A pool.rs:704-790 checks the same
// condition before yielding a busy sticky binding).
func (s *Scheduler) hasIdleAlternativeLocked(pc *poolCandidate, sticky Account) bool {
	limit := pc.pool.AccountConcurrency
	if limit <= 0 {
		return false
	}
	for _, acc := range pc.selectable {
		if acc.ID == sticky.ID {
			continue
		}
		if s.inFlight[accKey(pc.pool.PluginID, acc.ID)] < limit {
			return true
		}
	}
	return false
}

func (s *Scheduler) modelCooldownLocked(acc Account, key AccountKey, modelID string, now int64) int64 {
	until := acc.ModelCooldowns[modelID]
	if state, ok := s.modelCD[modelCooldownKey{Key: key, Model: modelID}]; ok && state.Until > until {
		until = state.Until
	}
	return until
}

// poolLessLocked orders pools for smart inter-pool scheduling: soonest credit
// expiry, then lowest multiplier, then most remaining credits, then pool
// priority.
func (s *Scheduler) poolLessLocked(a, b *poolCandidate, now int64) bool {
	aExpiry, bExpiry := poolExpiry(a.healthy), poolExpiry(b.healthy)
	if aExpiry != bExpiry {
		return aExpiry < bExpiry
	}
	aMult := ModelMultiplier(a.pool.Models, a.source.Model)
	bMult := ModelMultiplier(b.pool.Models, b.source.Model)
	if aMult != bMult {
		return aMult < bMult
	}
	aRemaining, bRemaining := poolRemaining(a.healthy), poolRemaining(b.healthy)
	if aRemaining != bRemaining {
		return aRemaining > bRemaining
	}
	if a.pool.PoolPriority != b.pool.PoolPriority {
		return a.pool.PoolPriority < b.pool.PoolPriority
	}
	return a.pool.PluginID < b.pool.PluginID
}

// ---------------------------------------------------------------- helpers

func accKey(pluginID, accountID string) AccountKey {
	return AccountKey{PluginID: pluginID, AccountID: accountID}
}

// poolExpiry returns the earliest *known* expiry among the accounts, or
// math.MaxInt64 when none is known. Unknown accounts are ignored rather than
// being treated as never-expire: a pool whose accounts all report no expiry
// must not look "more available forever" than one that does.
func poolExpiry(accounts []Account) int64 {
	best := int64(0)
	for _, acc := range accounts {
		if st, at := expiryOf(acc.ExpiresAt); st == expiryKnown {
			if best == 0 || at < best {
				best = at
			}
		}
	}
	if best == 0 {
		return math.MaxInt64
	}
	return best
}

func poolRemaining(accounts []Account) float64 {
	var total float64
	for _, acc := range accounts {
		if acc.Remaining > 0 {
			total += acc.Remaining
		}
	}
	return total
}

func poolRank(candidates []*poolCandidate, target *poolCandidate) int {
	for i, pc := range candidates {
		if pc == target {
			return i + 1
		}
	}
	return 0
}

func expiryUrgency(expiresAt, now int64) float64 {
	if expiresAt <= 0 {
		return 0.05
	}
	days := float64(expiresAt-now) / 86400
	if days < 1 {
		return 1
	}
	return 1 / days
}

func remainingRatio(remaining, total float64) float64 {
	if remaining < 0 {
		remaining = 0
	}
	den := math.Max(math.Max(total, remaining), 1)
	return remaining / den
}

func uniformIndex(r float64, n int) int {
	if n <= 1 {
		return 0
	}
	idx := int(r * float64(n))
	if idx < 0 {
		idx = 0
	}
	if idx >= n {
		idx = n - 1
	}
	return idx
}

func weightedIndex(scores []float64, r float64) int {
	var total float64
	for _, score := range scores {
		if score > 0 {
			total += score
		}
	}
	if total <= 0 {
		return uniformIndex(r, len(scores))
	}
	target := r * total
	var cumulative float64
	for i, score := range scores {
		if score <= 0 {
			continue
		}
		cumulative += score
		if target < cumulative {
			return i
		}
	}
	return len(scores) - 1
}

func p2cIndex(scores []float64, r1, r2 float64) int {
	n := len(scores)
	if n <= 1 {
		return 0
	}
	a := uniformIndex(r1, n)
	b := uniformIndex(r2, n)
	if a == b {
		b = (b + 1) % n
	}
	if scores[b] > scores[a] {
		return b
	}
	return a
}

func indexByLess(accounts []Account, less func(a, b Account) bool) int {
	best := 0
	for i := 1; i < len(accounts); i++ {
		if less(accounts[i], accounts[best]) {
			best = i
		}
	}
	return best
}

func roundScore(score float64) float64 {
	return math.Round(score*10000) / 10000
}

func unixString(at int64) string {
	return fmt.Sprintf("%d", at)
}
