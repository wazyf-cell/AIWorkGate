package plugins

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"aiworkgate/internal/config"
	"aiworkgate/internal/logging"
	"aiworkgate/internal/model"
	"aiworkgate/internal/secrets"
	"aiworkgate/internal/store"
)

// Options wires the runtime to the rest of the host. Only DataDir is
// mandatory; a nil Store/Secrets/Log/PluginLog simply disables persistence,
// token storage or log files (useful in tests).
type Options struct {
	DataDir   string
	Config    func() config.Config
	Store     *store.Store
	Secrets   *secrets.Store
	Log       *logging.Logger
	PluginLog *logging.PluginLogWriter

	// DirectEgress reports the loopback URL of the host listener that records a
	// plugin's direct (unproxied) outbound traffic, or "" when the child must
	// not be pointed at one. The policy behind that answer - the direct-logging
	// switch, strict egress, a configured host proxy - lives in the app; this
	// package only spells the result into the child environment, and a nil hook
	// means "never inject".
	DirectEgress func(pluginID string) string
}

// pluginSettings mirrors the editable part of the store row.
type pluginSettings struct {
	enabled     bool
	mode        string
	concurrency int
	checkin     string
	strict      bool
	installedAt int64
}

// pluginState is the in-memory record for one plugin. All fields are guarded
// by Manager.mu; process fields are only mutated by the supervisor or the
// lifecycle methods under that lock.
type pluginState struct {
	id       string
	dir      string
	manifest model.Manifest
	loadErr  string

	enabled     bool
	mode        string
	concurrency int
	checkin     string
	strict      bool
	installedAt int64
	idleTimeout time.Duration

	failed    bool
	failedAt  time.Time
	lastError string

	running bool
	healthy bool
	// directEgress is the loopback listener the *running* child's environment was
	// handed at launch ("" when it was pointed at none). A process environment
	// cannot be rewritten from outside, so this record is the only way the host
	// can tell that a live child still names a listener it has since closed.
	// finishRun clears it; see ReconcileDirectEgress.
	directEgress string
	// starting is true between spawning the child and the first successful
	// /health answer (or the launch failing). Endpoint uses it to tell an
	// in-flight start apart from a plugin the supervisor has declared dead.
	starting  bool
	port      int
	pid       int
	token     string
	startedAt int64

	runCancel context.CancelFunc
	runDone   chan struct{}

	// inflight counts proxied calls that have not returned yet. The idle
	// reaper never stops a plugin with inflight > 0, so a long check-in cannot
	// be cut off by a short manifest idle_timeout_s.
	inflight int

	lastUsed time.Time
	logLines []string
}

const maxPluginLogLines = 200

// coldStartTimeout is the fallback bound for a request waiting on an
// on-demand plugin to become healthy. It is deliberately equal to the default
// supervisor startTimeout (15s): the supervisor allows the child 15s to answer
// /health, so a shorter request wait (the old 8s) answered 503 while the plugin
// was still legitimately booting. Both phases now share one bound, which means
// a cold start either succeeds or reports a timeout at the same deadline the
// supervisor gives up at.
const coldStartTimeout = 15 * time.Second

// Manager owns plugin discovery, installation and process supervision.
type Manager struct {
	opts    Options
	dataDir string

	mu      sync.Mutex
	plugins map[string]*pluginState
	closed  bool

	closeCh chan struct{}
	wg      sync.WaitGroup

	now           func() time.Time
	healthEvery   time.Duration
	healthTimeout time.Duration
	startTimeout  time.Duration
	stopGrace     time.Duration
	idleTimeout   time.Duration
	reaperEvery   time.Duration
	backoff       []time.Duration
	failureWindow time.Duration
	maxFailures   int

	// portVerifyTimeout bounds the wait for the child's listener to appear;
	// verifyOwner confirms that listener belongs to the child before the first
	// token-bearing request. Tests replace verifyOwner.
	portVerifyTimeout time.Duration
	verifyOwner       func(port int, pid int, timeout time.Duration) error
}

// New builds a manager and starts the idle reaper. Call Close to release the
// background goroutine and every plugin process.
func New(opts Options) *Manager {
	m := &Manager{
		opts:          opts,
		dataDir:       strings.TrimSpace(opts.DataDir),
		plugins:       make(map[string]*pluginState),
		closeCh:       make(chan struct{}),
		now:           time.Now,
		healthEvery:   healthCheckEvery,
		healthTimeout: 2 * time.Second,
		startTimeout:  15 * time.Second,
		stopGrace:     5 * time.Second,
		idleTimeout:   DefaultIdleTimeoutS * time.Second,
		reaperEvery:   30 * time.Second,
		backoff:       []time.Duration{1 * time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second},
		failureWindow: 10 * time.Minute,
		maxFailures:   5,

		portVerifyTimeout: defaultPortVerifyTimeout,
		verifyOwner:       verifyPortOwner,
	}
	if m.dataDir != "" {
		_ = os.MkdirAll(m.dataDir, 0o755)
	}
	m.wg.Add(1)
	go m.reaperLoop()
	return m
}

// Close stops the reaper and every running plugin. It is safe to call twice.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	close(m.closeCh)
	var cancels []context.CancelFunc
	for _, p := range m.plugins {
		if p.runCancel != nil {
			cancels = append(cancels, p.runCancel)
		}
	}
	m.mu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(m.stopGrace + 10*time.Second):
		return errors.New("plugins: close timed out waiting for plugin processes")
	}
	return nil
}

// pluginsDir returns <dataDir>\plugins.
func (m *Manager) pluginsDir() string {
	if m.dataDir == "" {
		return ""
	}
	return filepath.Join(m.dataDir, pluginsDirName)
}

// installDir returns <dataDir>\plugins\<id>, or "" for an invalid id.
func (m *Manager) installDir(id string) string {
	if m.dataDir == "" || !validPluginID(id) {
		return ""
	}
	return filepath.Join(m.dataDir, pluginsDirName, id)
}

// pluginDataDir returns <dataDir>\plugin-data\<id>.
func (m *Manager) pluginDataDir(id string) string {
	if m.dataDir == "" || !validPluginID(id) {
		return ""
	}
	return filepath.Join(m.dataDir, pluginDataDirName, id)
}

// RuntimeDir returns <dataDir>\runtime\<id> and ensures it exists.
func (m *Manager) RuntimeDir(id string) string {
	if m.dataDir == "" || !validPluginID(id) {
		return ""
	}
	dir := filepath.Join(m.dataDir, runtimeDirName, id)
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// RulesFile is the merged rule pack path handed to the plugin.
func (m *Manager) RulesFile(id string) string {
	dir := m.RuntimeDir(id)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "rules.json")
}

// EgressFile is the per-account egress map handed to the plugin.
func (m *Manager) EgressFile(id string) string {
	dir := m.RuntimeDir(id)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "egress.json")
}

// directEgressURL asks the app hook where this plugin's direct traffic should
// be recorded. Three cases answer "" and leave the child environment untouched:
// no hook, a host-wide proxy the user configured on purpose (a direct listener
// must not quietly replace it), and a hook failure - including a panic, since
// losing the recording is acceptable and losing the plugin is not.
func (m *Manager) directEgressURL(id string, proxy config.ProxyConfig) (url string) {
	if m.opts.DirectEgress == nil || proxy.Enabled {
		return ""
	}
	defer func() {
		if r := recover(); r != nil {
			url = ""
		}
	}()
	return strings.TrimSpace(m.opts.DirectEgress(id))
}

// Discover scans <dataDir>\plugins for installed plugins, validates their
// manifests and merges the persisted settings. A broken or missing manifest
// marks that plugin failed instead of aborting the scan; running processes of
// plugins whose directory disappeared are stopped.
func (m *Manager) Discover() error {
	if m.dataDir == "" {
		return errors.New("plugins: discover: data directory is not configured")
	}
	root := m.pluginsDir()
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("plugins: discover: create %q: %w", root, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("plugins: discover: read %q: %w", root, err)
	}

	byID := m.settingsMap()
	type discovered struct {
		id       string
		dir      string
		manifest model.Manifest
		loadErr  string
		settings pluginSettings
		idle     time.Duration
	}
	found := make([]discovered, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue // staging/unzip leftovers
		}
		dir := filepath.Join(root, name)
		mf, loadErr := loadManifestDir(dir)
		id := mf.ID
		if id == "" {
			id = name
		}
		item := discovered{id: id, dir: dir, manifest: mf}
		if loadErr != nil {
			item.loadErr = loadErr.Error()
			m.logWarn("plugin manifest rejected", map[string]any{"plugin": id, "error": item.loadErr})
		} else {
			item.settings = m.deriveSettings(id, mf, byID)
			item.idle = m.idleTimeoutFor(mf)
			// A misspelled capability is not an error (the plugin still loads),
			// but it silently turns the feature off, so say it out loud once.
			if unknown := UnknownCapabilities(mf); len(unknown) > 0 {
				m.logWarn("plugin declares unknown capabilities; they will never match",
					map[string]any{"plugin": id, "capabilities": unknown})
			}
			// A misspelled key is ignored by the decoder, so the manifest loads
			// with a field silently at its zero value. Only the raw bytes can
			// tell, hence the second read - and only after a clean load, so a
			// rejected manifest is reported once.
			if data, readErr := os.ReadFile(filepath.Join(dir, ManifestFileName)); readErr == nil {
				if unknown := UnknownManifestKeys(data); len(unknown) > 0 {
					m.logWarn("plugin manifest has unknown keys; they are ignored",
						map[string]any{"plugin": id, "keys": unknown})
				}
			}
		}
		found = append(found, item)
	}

	m.mu.Lock()
	merged := make(map[string]*pluginState, len(found))
	for _, f := range found {
		p := m.plugins[f.id]
		if p == nil {
			p = &pluginState{id: f.id}
		}
		p.dir = f.dir
		p.manifest = f.manifest
		p.loadErr = f.loadErr
		if f.loadErr == "" {
			p.applySettings(f.settings)
			p.idleTimeout = f.idle
		}
		merged[f.id] = p
	}
	var stale []*pluginState
	for id, p := range m.plugins {
		if _, ok := merged[id]; !ok {
			stale = append(stale, p)
		}
	}
	m.plugins = merged
	m.mu.Unlock()

	for _, p := range stale {
		m.stopState(p)
	}
	return nil
}

// List returns a snapshot of every discovered plugin ordered by id. Failure
// states older than the failure window are dropped first, so the snapshot heals
// without a manual Enable/Start.
func (m *Manager) List() []model.PluginStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.plugins))
	for id := range m.plugins {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	now := m.now()
	out := make([]model.PluginStatus, 0, len(ids))
	for _, id := range ids {
		p := m.plugins[id]
		m.expireFailure(p, now)
		out = append(out, p.status())
	}
	return out
}

// Get returns one plugin status. An expired failure state is cleared first.
func (m *Manager) Get(id string) (model.PluginStatus, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.plugins[id]
	if !ok {
		return model.PluginStatus{}, false
	}
	m.expireFailure(p, m.now())
	return p.status(), true
}

// expireFailure clears a failure state once the failure window has elapsed.
// The scheduler keeps retrying a failed plugin, so without this the stored
// start error would be replayed forever and only a manual Enable/Start could
// recover it. Caller holds m.mu.
func (m *Manager) expireFailure(p *pluginState, now time.Time) bool {
	if !p.failed {
		return true
	}
	window := m.failureWindow
	if window <= 0 {
		window = 10 * time.Minute
	}
	// A zero timestamp is an old state without one: treat it as expired so the
	// plugin is not wedged; retrying is the safe direction.
	if p.failedAt.IsZero() || now.Sub(p.failedAt) >= window {
		p.failed = false
		p.failedAt = time.Time{}
		p.lastError = ""
		return true
	}
	return false
}

// status builds the host-visible view. Caller holds m.mu (or owns p).
func (p *pluginState) status() model.PluginStatus {
	st := model.PluginStatus{
		Manifest:    p.manifest,
		Dir:         p.dir,
		Enabled:     p.enabled,
		Running:     p.running,
		Healthy:     p.healthy,
		Failed:      p.failed || p.loadErr != "",
		Port:        p.port,
		PID:         p.pid,
		StartedAt:   p.startedAt,
		InstalledAt: p.installedAt,
		LastError:   p.lastError,
	}
	if p.loadErr != "" {
		st.LastError = p.loadErr
	}
	return st
}

func (p *pluginState) settings() pluginSettings {
	return pluginSettings{
		enabled:     p.enabled,
		mode:        p.mode,
		concurrency: p.concurrency,
		checkin:     p.checkin,
		strict:      p.strict,
		installedAt: p.installedAt,
	}
}

func (p *pluginState) applySettings(s pluginSettings) {
	p.enabled = s.enabled
	p.mode = s.mode
	p.concurrency = s.concurrency
	p.checkin = s.checkin
	p.strict = s.strict
	p.installedAt = s.installedAt
}

// currentConfig returns a normalized copy of the host configuration.
func (m *Manager) currentConfig() config.Config {
	var c config.Config
	if m.opts.Config != nil {
		c = m.opts.Config()
	}
	c.Normalize()
	return c
}

// idleTimeoutFor resolves startup.idle_timeout_s, falling back to the
// manager default (600s).
func (m *Manager) idleTimeoutFor(mf model.Manifest) time.Duration {
	if mf.Startup.IdleTimeoutS > 0 {
		return time.Duration(mf.Startup.IdleTimeoutS) * time.Second
	}
	if m.idleTimeout > 0 {
		return m.idleTimeout
	}
	return DefaultIdleTimeoutS * time.Second
}

// settingsMap loads every persisted settings row keyed by plugin id. A store
// failure only degrades persistence, it never fails discovery.
func (m *Manager) settingsMap() map[string]store.PluginSetting {
	out := make(map[string]store.PluginSetting)
	if m.opts.Store == nil {
		return out
	}
	rows, err := m.opts.Store.ListPluginSettings()
	if err != nil {
		m.logWarn("cannot read plugin settings", map[string]any{"error": err.Error()})
		return out
	}
	for _, row := range rows {
		out[row.PluginID] = row
	}
	return out
}

// deriveSettings merges manifest defaults with a persisted row.
//
// Precedence: the persisted row only overrides when the user actually set the
// value. concurrency stores NULL/0 for "跟随全局" (so clamp keeps the manifest/
// global base) and strict is applied only when row.StrictSet is true (column
// non-NULL); a NULL strict column leaves the global base in charge. The strict
// base is manifest.Egress.Strict OR the global plugins.strict_egress, matching
// the egress writer. Rows created by install carry a concrete copied value
// (never NULL), so an existing install keeps its override until the user
// explicitly chooses 跟随全局 in the settings dialog.
func (m *Manager) deriveSettings(id string, mf model.Manifest, byID map[string]store.PluginSetting) pluginSettings {
	cfg := m.currentConfig()
	checkin := strings.TrimSpace(mf.Checkin.DefaultTime)
	if !timeOfDayPattern.MatchString(checkin) {
		checkin = cfg.Checkin.DefaultTime
	}
	s := pluginSettings{
		enabled:     true,
		mode:        modeOrDefault(mf.Startup.Mode, cfg),
		concurrency: clampConcurrency(mf.Concurrency.Suggested, cfg.Plugins.Concurrency),
		checkin:     checkin,
		strict:      mf.Egress.Strict || cfg.Plugins.StrictEgress,
	}
	if row, ok := byID[id]; ok {
		s.enabled = row.Enabled
		if row.Mode == model.StartupAlways || row.Mode == model.StartupOnDemand {
			s.mode = row.Mode
		}
		// concurrency 0 (or NULL) = 跟随全局; clamp falls back to the base above.
		s.concurrency = clampConcurrency(row.Concurrency, s.concurrency)
		if timeOfDayPattern.MatchString(row.CheckinTime) {
			s.checkin = row.CheckinTime
		}
		// strict overrides only when the row carries an explicit value.
		if row.StrictSet {
			s.strict = row.Strict
		}
		s.installedAt = row.InstalledAt
	}
	return s
}

// persistSettings writes the editable settings for id.
func (m *Manager) persistSettings(id string, s pluginSettings) error {
	if m.opts.Store == nil {
		return nil
	}
	if err := m.opts.Store.UpsertPluginSettings(id, s.enabled, s.mode, s.concurrency, s.checkin, s.strict, s.installedAt); err != nil {
		return fmt.Errorf("plugins: persist settings for %q: %w", id, err)
	}
	return nil
}

// setEnabled persists only the enabled flag. persistSettings would rewrite the
// resolved concurrency/strict, pinning a 跟随全局 value into an explicit override.
func (m *Manager) setEnabled(id string, enabled bool) error {
	if m.opts.Store == nil {
		return nil
	}
	var seed store.PluginSettingsView
	m.mu.Lock()
	if p, ok := m.plugins[id]; ok {
		s := p.settings()
		seed = store.PluginSettingsView{Mode: s.mode, CheckinTime: s.checkin, InstalledAt: s.installedAt}
	}
	m.mu.Unlock()
	return m.opts.Store.SetPluginEnabled(id, enabled, seed)
}

// refreshPlugin reloads one installed plugin from disk and merges the store
// settings. The caller has already stopped any process.
func (m *Manager) refreshPlugin(id, dir string, mf model.Manifest) error {
	byID := m.settingsMap()
	s := m.deriveSettings(id, mf, byID)
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.plugins[id]
	if !ok {
		p = &pluginState{id: id}
		m.plugins[id] = p
	}
	p.dir = dir
	p.manifest = mf
	p.loadErr = ""
	p.applySettings(s)
	p.idleTimeout = m.idleTimeoutFor(mf)
	p.failed = false
	p.lastError = ""
	return nil
}

// Enable persists enabled=true and starts the plugin.
func (m *Manager) Enable(id string) error {
	m.mu.Lock()
	p, ok := m.plugins[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNotInstalled, id)
	}
	p.enabled = true
	p.failed = false
	p.lastError = ""
	m.mu.Unlock()
	if err := m.setEnabled(id, true); err != nil {
		return err
	}
	return m.Start(id)
}

// Disable stops the plugin and persists enabled=false.
func (m *Manager) Disable(id string) error {
	m.mu.Lock()
	p, ok := m.plugins[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNotInstalled, id)
	}
	p.enabled = false
	m.mu.Unlock()
	if err := m.setEnabled(id, false); err != nil {
		return err
	}
	if err := m.Stop(id); err != nil && !errors.Is(err, ErrNotInstalled) {
		return err
	}
	return nil
}

// StartAll starts enabled plugins configured with mode "always". On-demand
// plugins stay stopped until a request or the dashboard wakes them.
func (m *Manager) StartAll() error {
	var errs []error
	for _, id := range m.enabledIDs(model.StartupAlways) {
		if err := m.Start(id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// PauseAll stops every enabled plugin, regardless of startup mode.
func (m *Manager) PauseAll() {
	for _, id := range m.enabledIDs("") {
		if err := m.Stop(id); err != nil && !errors.Is(err, ErrNotInstalled) {
			m.logWarn("pause plugin failed", map[string]any{"plugin": id, "error": err.Error()})
		}
	}
}

// ResumeAll restarts enabled "always" plugins. On-demand plugins stay stopped.
func (m *Manager) ResumeAll() {
	for _, id := range m.enabledIDs(model.StartupAlways) {
		if err := m.Start(id); err != nil && !errors.Is(err, ErrDisabled) {
			m.logWarn("resume plugin failed", map[string]any{"plugin": id, "error": err.Error()})
		}
	}
}

// enabledIDs lists enabled plugins, optionally filtered by startup mode.
func (m *Manager) enabledIDs(mode string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.plugins))
	for id, p := range m.plugins {
		if !p.enabled || p.loadErr != "" {
			continue
		}
		if mode != "" && p.mode != mode {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Start launches the supervisor for one plugin. It returns as soon as the
// process is spawned; health is awaited in the background (EnsureRunning waits
// for it). A previously failed plugin is retried.
func (m *Manager) Start(id string) error {
	m.mu.Lock()
	p, ok := m.plugins[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNotInstalled, id)
	}
	if p.loadErr != "" {
		err := fmt.Errorf("plugins: %q: %s", id, p.loadErr)
		m.mu.Unlock()
		return err
	}
	if !p.enabled {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrDisabled, id)
	}
	if m.closed {
		m.mu.Unlock()
		return errors.New("plugins: manager is closed")
	}
	if p.runCancel != nil {
		m.mu.Unlock()
		return nil
	}
	p.failed = false
	p.failedAt = time.Time{}
	p.lastError = ""
	p.lastUsed = m.now()
	ctx, cancel := context.WithCancel(context.Background())
	p.runCancel = cancel
	p.runDone = make(chan struct{})
	m.wg.Add(1)
	m.mu.Unlock()

	go m.runPlugin(p, ctx)
	return nil
}

// Stop asks the supervisor to terminate the plugin gracefully (5s) and falls
// back to killing the process tree.
func (m *Manager) Stop(id string) error {
	m.mu.Lock()
	p, ok := m.plugins[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNotInstalled, id)
	}
	cancel, done := p.runCancel, p.runDone
	m.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	if done != nil {
		select {
		case <-done:
		case <-time.After(m.stopGrace + 10*time.Second):
			return fmt.Errorf("plugins: stop %q: timed out", id)
		}
	}
	return nil
}

// stopState stops a state that may already have been removed from the map.
func (m *Manager) stopState(p *pluginState) {
	m.mu.Lock()
	cancel, done := p.runCancel, p.runDone
	m.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	if done != nil {
		select {
		case <-done:
		case <-time.After(m.stopGrace + 5*time.Second):
		}
	}
}

// EnsureRunning wakes an on-demand plugin and waits until /health answers,
// bounded by the supervisor start timeout (m.startTimeout, 15s by default) and
// ctx. An expired failure state does not block the retry: it is cleared first.
func (m *Manager) EnsureRunning(ctx context.Context, id string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	p, ok := m.plugins[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNotInstalled, id)
	}
	if p.loadErr != "" {
		err := fmt.Errorf("plugins: %q: %s", id, p.loadErr)
		m.mu.Unlock()
		return err
	}
	if !p.enabled {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrDisabled, id)
	}
	if p.running && p.healthy {
		m.mu.Unlock()
		m.Touch(id)
		return nil
	}
	m.expireFailure(p, m.now())
	if p.failed {
		err := p.lastError
		m.mu.Unlock()
		return fmt.Errorf("plugins: %q failed: %s", id, err)
	}
	m.mu.Unlock()

	if err := m.Start(id); err != nil {
		return err
	}

	// One bound for the whole cold start: the same value the supervisor gives
	// the child to answer /health (see coldStartTimeout).
	timeout := m.startTimeout
	if timeout <= 0 {
		timeout = coldStartTimeout
	}
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		m.mu.Lock()
		running, healthy, failed, lastErr := p.running, p.healthy, p.failed, p.lastError
		m.mu.Unlock()
		if running && healthy {
			m.Touch(id)
			return nil
		}
		if failed {
			return fmt.Errorf("plugins: %q failed: %s", id, lastErr)
		}
		if time.Now().After(deadline) {
			if lastErr != "" {
				return fmt.Errorf("plugins: %q did not become healthy: %s", id, lastErr)
			}
			return fmt.Errorf("plugins: %q did not become healthy within %s", id, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Touch records request activity for the on-demand idle reaper.
func (m *Manager) Touch(id string) {
	m.mu.Lock()
	if p, ok := m.plugins[id]; ok {
		p.lastUsed = m.now()
	}
	m.mu.Unlock()
}

// Concurrency returns the effective chat concurrency for a plugin: the store
// setting when present, otherwise the manifest/config default, clamped to
// 1..32.
func (m *Manager) Concurrency(id string) int {
	cfg := m.currentConfig()
	m.mu.Lock()
	fallback := 0
	if p, ok := m.plugins[id]; ok {
		fallback = p.concurrency
	}
	m.mu.Unlock()
	if m.opts.Store != nil {
		_, _, c, _, _, ok, err := m.opts.Store.GetPluginSettings(id)
		if err == nil && ok && c > 0 {
			fallback = c
		}
	}
	return clampConcurrency(fallback, cfg.Plugins.Concurrency)
}

// Endpoint returns the base URL and bearer token of a running plugin. A plugin
// that is running but marked unhealthy by the supervisor is reported as not
// available (ok=false) instead of exposing a port that no longer answers, so a
// request fails fast with 503 instead of burning its upstream timeout on a dead
// child. A plugin whose start is still in flight remains exposed: EnsureRunning
// (or the supervisor) is already waiting for it.
func (m *Manager) Endpoint(id string) (baseURL, token string, ok bool) {
	base, tok, err := m.endpoint(id)
	if err != nil {
		return "", "", false
	}
	return base, tok, true
}

// endpoint is Endpoint with the reason a plugin cannot be dialed; proxied calls
// use it so an unhealthy plugin surfaces as an explanatory error rather than a
// generic ErrNotRunning.
func (m *Manager) endpoint(id string) (baseURL, token string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, found := m.plugins[id]
	if !found {
		return "", "", fmt.Errorf("%w: %s", ErrNotInstalled, id)
	}
	if p.loadErr != "" {
		return "", "", fmt.Errorf("plugins: %q: %s", id, p.loadErr)
	}
	if !p.running || p.port == 0 {
		return "", "", fmt.Errorf("%w: %s", ErrNotRunning, id)
	}
	if !p.healthy && !p.starting {
		reason := p.lastError
		if reason == "" {
			reason = "health check failed"
		}
		return "", "", fmt.Errorf("plugins: %q is unhealthy: %s", id, reason)
	}
	return fmt.Sprintf("http://127.0.0.1:%d", p.port), p.token, nil
}

// BeginCall marks one proxied call as in flight for id and returns the release
// function, which the caller must defer. The idle reaper skips plugins with
// in-flight calls, so a long request (a check-in may take 5 minutes) cannot be
// killed by a short manifest startup.idle_timeout_s. The typed client calls it
// for every control call; the gateway should wrap its proxied chat attempts the
// same way so a long SSE body is protected too.
func (m *Manager) BeginCall(id string) func() {
	m.mu.Lock()
	if p, ok := m.plugins[id]; ok {
		p.inflight++
	}
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		if p, ok := m.plugins[id]; ok && p.inflight > 0 {
			p.inflight--
		}
		m.mu.Unlock()
	}
}

// RecentOutput returns up to max of the last captured stdout/stderr lines.
func (m *Manager) RecentOutput(id string, max int) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.plugins[id]
	if !ok {
		return nil
	}
	if max <= 0 || max > len(p.logLines) {
		max = len(p.logLines)
	}
	out := make([]string, max)
	copy(out, p.logLines[len(p.logLines)-max:])
	return out
}

func (m *Manager) appendOutput(id, line string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.plugins[id]
	if !ok {
		return
	}
	p.logLines = append(p.logLines, line)
	if len(p.logLines) > maxPluginLogLines {
		p.logLines = p.logLines[len(p.logLines)-maxPluginLogLines:]
	}
}

// ipcTokenRemover is satisfied by secrets.Store builds that can delete a
// plugin token. It is optional so the runtime keeps compiling if the secrets
// package does not expose removal yet.
type pluginTokenRemover interface {
	RemovePluginToken(pluginID string) error
}

// Uninstall stops the plugin, removes its installation directory and,
// optionally, its private data. The settings row and the plugin token are
// deleted as well; the runtime directory is left for the scheduler to reuse.
func (m *Manager) Uninstall(id string, deleteData bool) error {
	if !validPluginID(id) {
		return fmt.Errorf("plugins: uninstall: invalid plugin id %q", id)
	}
	if err := m.Stop(id); err != nil && !errors.Is(err, ErrNotInstalled) {
		return err
	}
	if dir := m.installDir(id); dir != "" {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("plugins: uninstall %q: remove install dir: %w", id, err)
		}
	}
	if deleteData {
		if dir := m.pluginDataDir(id); dir != "" {
			if err := os.RemoveAll(dir); err != nil {
				return fmt.Errorf("plugins: uninstall %q: remove data dir: %w", id, err)
			}
		}
	}
	m.mu.Lock()
	delete(m.plugins, id)
	m.mu.Unlock()

	if m.opts.Store != nil && m.opts.Store.DB() != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := m.opts.Store.DB().ExecContext(ctx, `DELETE FROM plugin_settings WHERE plugin_id = ?`, id); err != nil {
			return fmt.Errorf("plugins: uninstall %q: delete settings: %w", id, err)
		}
	}
	if remover, ok := any(m.opts.Secrets).(pluginTokenRemover); ok {
		if err := remover.RemovePluginToken(id); err != nil {
			m.logWarn("cannot delete plugin token", map[string]any{"plugin": id, "error": err.Error()})
		}
	}
	m.logInfo("plugin uninstalled", map[string]any{"plugin": id, "delete_data": deleteData})
	return nil
}

// reaperLoop stops idle on-demand plugins.
func (m *Manager) reaperLoop() {
	defer m.wg.Done()
	every := m.reaperEvery
	if every <= 0 {
		every = 30 * time.Second
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-m.closeCh:
			return
		case <-ticker.C:
			m.reapIdle(m.now())
		}
	}
}

// reapIdle stops every on-demand plugin whose last request is older than its
// idle timeout.
func (m *Manager) reapIdle(now time.Time) {
	for _, id := range m.idleIDs(now) {
		m.logInfo("stopping idle on-demand plugin", map[string]any{"plugin": id})
		if err := m.Stop(id); err != nil {
			m.logWarn("idle stop failed", map[string]any{"plugin": id, "error": err.Error()})
		}
	}
}

// idleIDs reports the on-demand plugins that have been idle past their
// timeout at now. A plugin with a proxied call in flight is never idle: the
// call may legitimately outlive a small idle_timeout_s.
func (m *Manager) idleIDs(now time.Time) []string {
	var idle []string
	m.mu.Lock()
	for id, p := range m.plugins {
		if !p.enabled || p.mode != model.StartupOnDemand || !p.running {
			continue
		}
		if p.inflight > 0 {
			continue
		}
		timeout := p.idleTimeout
		if timeout <= 0 {
			timeout = DefaultIdleTimeoutS * time.Second
		}
		if now.Sub(p.lastUsed) >= timeout {
			idle = append(idle, id)
		}
	}
	m.mu.Unlock()
	sort.Strings(idle)
	return idle
}

// isRunning reports whether a supervisor loop is active for id.
func (m *Manager) isRunning(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.plugins[id]
	return ok && p.runCancel != nil
}

func (m *Manager) logInfo(msg string, fields map[string]any) {
	if m.opts.Log != nil {
		m.opts.Log.Info("plugins", msg, fields)
	}
}

func (m *Manager) logWarn(msg string, fields map[string]any) {
	if m.opts.Log != nil {
		m.opts.Log.Warn("plugins", msg, fields)
	}
}

func (m *Manager) logError(msg string, fields map[string]any) {
	if m.opts.Log != nil {
		m.opts.Log.Error("plugins", msg, fields)
	}
}

// modeOrDefault resolves an empty or unknown startup mode to the host default.
func modeOrDefault(mode string, cfg config.Config) string {
	switch strings.TrimSpace(mode) {
	case model.StartupAlways:
		return model.StartupAlways
	case model.StartupOnDemand:
		return model.StartupOnDemand
	}
	if cfg.Plugins.StartupMode == model.StartupOnDemand {
		return model.StartupOnDemand
	}
	return model.StartupAlways
}

// clampConcurrency clamps v into 1..32, using fallback when v is unset.
func clampConcurrency(v, fallback int) int {
	if v <= 0 {
		v = fallback
	}
	if v < config.MinConcurr {
		v = config.MinConcurr
	}
	if v > config.MaxConcurr {
		v = config.MaxConcurr
	}
	return v
}
