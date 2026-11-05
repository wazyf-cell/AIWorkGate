package plugins

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"aiworkgate/internal/config"
	"aiworkgate/internal/market"
	"aiworkgate/internal/opchost"
	"aiworkgate/internal/store"
)

// UpdateTimer periodically runs the P2-5 market update check: for every row of
// market_installs it asks the registry for a newer stable release and records
// the from→to diff. Policy "prompt" (the default) only records the available
// update; policy "auto" is an explicit opt-in that prepares the new tarball and
// applies it through UpdateDirWithRollback — never a bare UpdateDir — so a
// failing health check rolls back to the previous tree.
//
// Nothing is installed without auto being configured: the whole point of the
// two-policy split is that a fresh host with default config only ever reports.
type UpdateTimer struct {
	mgr *Manager
	st  *store.Store
	cfg func() config.Config

	// preparer and updater are seams for tests: the production values prepare
	// through the real market installer and apply through the Manager.
	preparer updatePreparer
	updater  pluginUpdater

	now      func() time.Time
	interval func() time.Duration // nil = read config minutes

	mu       sync.Mutex
	lastScan *UpdateScan
}

// updatePreparer prepares a market package into a staging directory
// (market.Installer implements it).
type updatePreparer interface {
	Prepare(ctx context.Context, spec string, opts market.Options) (*market.Prepared, error)
	Cleanup(p *market.Prepared) error
}

// pluginUpdater applies a prepared update directory with rollback semantics.
// *Manager implements it via UpdateDirWithRollback.
type pluginUpdater interface {
	UpdateDirWithRollback(ctx context.Context, id, srcDir string) (bool, error)
}

// UpdateApplied is one update the auto policy applied during a scan.
type UpdateApplied struct {
	PluginID string `json:"plugin_id"`
	From     string `json:"from"`
	To       string `json:"to"`
}

// UpdateScan is the report of one completed scan. Available are the diffs the
// policy is allowed to see; Applied only the ones auto actually installed.
type UpdateScan struct {
	CheckedAt time.Time           `json:"checked_at"`
	Policy    string              `json:"policy"`
	Available []market.UpdateInfo `json:"available"`
	Applied   []UpdateApplied     `json:"applied"`
	Errors    map[string]string   `json:"errors,omitempty"`
}

// NewUpdateTimer builds the timer for one manager and store. The registries the
// scans use come from cfg (Plugins.MarketMirror) at scan time, so a config
// reload changes the mirror without restarting the timer.
func NewUpdateTimer(mgr *Manager, st *store.Store, cfg func() config.Config) *UpdateTimer {
	return &UpdateTimer{
		mgr:      mgr,
		st:       st,
		cfg:      cfg,
		preparer: market.NewInstaller(nil, ""),
		updater:  mgr,
		now:      time.Now,
	}
}

// delay returns the configured interval; 0 (or an unset config) disables the
// timer. Tests override it through t.interval for a short cadence.
func (t *UpdateTimer) delay() time.Duration {
	if t.interval != nil {
		d := t.interval()
		if d <= 0 {
			return 0
		}
		return d
	}
	if t.cfg == nil {
		return 0
	}
	m := t.cfg().Plugins.MarketUpdateIntervalMinutes
	if m <= 0 {
		return 0
	}
	return time.Duration(m) * time.Minute
}

// policy returns the configured update policy, defaulting to prompt.
func (t *UpdateTimer) policy() string {
	if t.cfg == nil {
		return config.MarketUpdatePolicyPrompt
	}
	p := strings.ToLower(strings.TrimSpace(t.cfg().Plugins.MarketUpdatePolicy))
	if p == config.MarketUpdatePolicyAuto {
		return config.MarketUpdatePolicyAuto
	}
	return config.MarketUpdatePolicyPrompt
}

// Run loops until ctx is done, running one scan every configured interval. A
// disabled interval (0 or missing config) returns immediately. Each scan is
// synchronous: a slow registry never stacks scans on top of each other.
func (t *UpdateTimer) Run(ctx context.Context) {
	for {
		d := t.delay()
		if d <= 0 {
			return
		}
		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		case <-timer.C:
			_, _ = t.Tick(ctx)
		}
	}
}

// Tick runs one update scan and returns its report. It never aborts the loop
// for a single package: registry errors land in scan.Errors.
func (t *UpdateTimer) Tick(ctx context.Context) (UpdateScan, error) {
	scan := UpdateScan{
		CheckedAt: t.now(),
		Policy:    t.policy(),
		Errors:    map[string]string{},
	}
	if t == nil || t.st == nil {
		return scan, fmt.Errorf("plugins: update timer: store is not configured")
	}
	rows, err := t.st.ListMarketInstalls()
	if err != nil {
		return scan, fmt.Errorf("plugins: update timer: list market installs: %w", err)
	}
	if len(rows) == 0 {
		t.setLastScan(scan)
		return scan, nil
	}
	base := ""
	if t.cfg != nil {
		base = strings.TrimSpace(t.cfg().Plugins.MarketMirror)
	}
	reg, err := market.NewRegistry(base, nil)
	if err != nil {
		return scan, err
	}
	for _, row := range rows {
		info, err := market.CheckUpdate(ctx, reg, row.Package, row.Version, market.UpdatePolicy(scan.Policy))
		if err != nil {
			scan.Errors[row.PluginID] = err.Error()
			continue
		}
		if !info.Available {
			continue
		}
		scan.Available = append(scan.Available, info)
		if scan.Policy == config.MarketUpdatePolicyAuto {
			t.applyAuto(ctx, &scan, row, info, reg)
		}
	}
	sort.Slice(scan.Available, func(i, j int) bool {
		return scan.Available[i].PluginID < scan.Available[j].PluginID
	})
	sort.Slice(scan.Applied, func(i, j int) bool {
		return scan.Applied[i].PluginID < scan.Applied[j].PluginID
	})
	t.setLastScan(scan)
	return scan, nil
}

// applyAuto prepares the new version and applies it through the rollback
// primitive, then records the applied version so the next scan reports the
// next diff. Failures land in scan.Errors and never abort other packages.
func (t *UpdateTimer) applyAuto(ctx context.Context, scan *UpdateScan, row store.MarketInstall, info market.UpdateInfo, reg *market.Registry) {
	if t.mgr == nil || t.updater == nil {
		scan.Errors[row.PluginID] = "plugins: update timer: plugin manager is not configured"
		return
	}
	prepared, err := t.preparer.Prepare(ctx, row.Package+"@"+info.To, market.Options{
		Registry:         reg,
		FinalPluginDir:   t.mgr.installDir(row.PluginID),
		BridgeExecutable: t.bridgePath(),
	})
	if err != nil {
		scan.Errors[row.PluginID] = fmt.Sprintf("prepare %s: %v", info.To, err)
		return
	}
	defer t.preparer.Cleanup(prepared)

	rolledBack, err := t.updater.UpdateDirWithRollback(ctx, row.PluginID, prepared.Root)
	if err != nil {
		scan.Errors[row.PluginID] = fmt.Sprintf("apply %s: %v (rolled back: %v)", info.To, err, rolledBack)
		return
	}
	if err := t.st.UpsertMarketInstall(store.MarketInstall{
		PluginID: row.PluginID,
		Package:  row.Package,
		Source:   row.Source,
		Version:  info.To,
		SHA256:   prepared.Record.SHA256,
	}); err != nil {
		scan.Errors[row.PluginID] = fmt.Sprintf("record %s: %v", info.To, err)
		return
	}
	scan.Applied = append(scan.Applied, UpdateApplied{PluginID: row.PluginID, From: info.From, To: info.To})
}

// bridgePath resolves the shared magpie-bridge.exe under <dataDir>\bridge. It
// returns "" when the binary is missing: the prepare step then fails only if
// the package actually needs a generated bridge manifest, matching the
// interactive install path (internal/app market.go).
func (t *UpdateTimer) bridgePath() string {
	if t.mgr == nil {
		return ""
	}
	p := filepath.Join(t.mgr.dataDir, "bridge", opchost.DefaultBridgeExeName)
	if isRegularFile(p) {
		return p
	}
	return ""
}

func (t *UpdateTimer) setLastScan(scan UpdateScan) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastScan = &scan
}

// LastScan returns the most recent scan report, if any (prompt consumers — the
// webui updates endpoint already recomputes live — can read it as the timer's
// side channel).
func (t *UpdateTimer) LastScan() (UpdateScan, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.lastScan == nil {
		return UpdateScan{}, false
	}
	return *t.lastScan, true
}
