package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"aiworkgate/internal/market"
	"aiworkgate/internal/model"
	"aiworkgate/internal/plugins"
	"aiworkgate/internal/store"
	"aiworkgate/internal/webui"
)

// P2-4 市场接线（task-20）：/api/market/{search,install,updates} 从 501 空转变成
// 真实可用。三个回调在 webOptions() 里挂载，实现在这里；契约形状对齐
// internal/webui/market.go（前端已定型），市场包本身不动。

// marketRegistry assembles the npm registry client. The base URL comes from
// config.Plugins.MarketMirror (P2-4): empty = official npm registry; set a
// mirror like https://registry.npmmirror.com to switch. Direct selection only,
// no automatic failover (failover is a later card).
func (a *App) marketRegistry() (*market.Registry, error) {
	base := strings.TrimSpace(a.configSnapshot().Plugins.MarketMirror)
	if base == "" {
		base = market.DefaultRegistry
	}
	return market.NewRegistry(base, nil)
}

// bridgeExecutablePath resolves the shared magpie-bridge.exe for market installs
// (P1/P8-⑥): <dataDir>\bridge\magpie-bridge.exe first, then dist\magpie-bridge.exe
// next to the running binary (dev tree) and in cwd (go run fallback). A missing
// bridge matters only when the prepared package needs a generated bridge
// manifest, so callers may tolerate the error and let Prepare report it.
func (a *App) bridgeExecutablePath() (string, error) {
	var candidates []string
	candidates = append(candidates, filepath.Join(a.dataDir, "bridge", "magpie-bridge.exe"))
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "dist", "magpie-bridge.exe"))
	}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(wd, "dist", "magpie-bridge.exe"))
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, nil
		}
	}
	return "", fmt.Errorf("market: bridge executable not found (tried: %s)", strings.Join(candidates, ", "))
}

// marketSearch backs GET /api/market/search.
func (a *App) marketSearch(query string, limit int) (any, error) {
	reg, err := a.marketRegistry()
	if err != nil {
		return nil, err
	}
	return reg.Search(context.Background(), query, limit)
}

// marketInstall backs POST /api/market/install. It runs the full chain the
// webui market test pinned (market prepare → plugins.InstallDirOpts disabled →
// store market record), and cleans the staging dir on every failure path that
// made one. Contract/configuration violations map to webui.ErrInvalidRequest so
// the admin API answers 400 rather than 500.
func (a *App) marketInstall(spec, source, localPath string) (any, error) {
	sp, err := market.ParseSpec(spec)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", webui.ErrInvalidRequest, err)
	}
	var pluginID string
	switch sp.Kind {
	case market.KindNPM:
		pluginID, err = market.PluginIDFor(sp.Name)
	case market.KindGitHub:
		// D-04: GitHub specs install the branch archive tarball.
		pluginID, err = market.PluginIDFor(sp.Repo)
	case market.KindLocal:
		pluginID, err = market.PluginIDFor(strings.ToLower(filepath.Base(filepath.Clean(sp.Dir))))
	default:
		return nil, fmt.Errorf("%w: source %q is not supported", webui.ErrInvalidRequest, sp.Kind)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", webui.ErrInvalidRequest, err)
	}

	reg, err := a.marketRegistry()
	if err != nil {
		return nil, err
	}
	bridgeExe, _ := a.bridgeExecutablePath() // 缺桥二进制也继续：只有需要生成桥 manifest 时才致命

	inst := market.NewInstaller(reg, filepath.Join(a.dataDir, "market-scratch"))
	p, err := inst.Prepare(context.Background(), spec, market.Options{
		Registry:         reg,
		FinalPluginDir:   filepath.Join(a.dataDir, "plugins", pluginID),
		BridgeExecutable: bridgeExe,
		StartupMode:      model.StartupOnDemand,
		IdleTimeoutS:     600,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", webui.ErrInvalidRequest, err)
	}
	if err := a.pluginMgr.InstallDirOpts(p.Root, plugins.InstallOpts{Enabled: false}); err != nil {
		_ = inst.Cleanup(p)
		return nil, err
	}
	_ = inst.Cleanup(p)

	rec := p.Record
	src := strings.TrimSpace(source)
	if src == "" {
		src = sp.Kind.String()
	}
	if err := a.store.UpsertMarketInstall(store.MarketInstall{
		PluginID: rec.PluginID,
		Package:  rec.Package,
		Source:   src,
		Version:  rec.Version,
		SHA256:   rec.SHA256,
	}); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "id": rec.PluginID, "version": rec.Version}, nil
}

// marketUpdater is the rollback-capable apply seam (*plugins.Manager).
type marketUpdater interface {
	UpdateDirWithRollback(ctx context.Context, id, srcDir string) (bool, error)
}

// marketUpdatePreparer prepares a market package into a staging directory
// (market.Installer implements it).
type marketUpdatePreparer interface {
	Prepare(ctx context.Context, spec string, opts market.Options) (*market.Prepared, error)
	Cleanup(p *market.Prepared) error
}

// Seams: production uses the app's plugin manager and a real market installer;
// tests inject fakes to drive the rollback branch without a network.
var (
	marketUpdaterFor = func(a *App) marketUpdater {
		if a == nil || a.pluginMgr == nil {
			return nil
		}
		return a.pluginMgr
	}
	marketUpdatePreparerFor = func(a *App) (marketUpdatePreparer, error) {
		reg, err := a.marketRegistry()
		if err != nil {
			return nil, err
		}
		return market.NewInstaller(reg, filepath.Join(a.dataDir, "market-scratch")), nil
	}
)

// marketUpdate applies one prepared update for a plugin id. It mirrors the
// auto path of the update timer (plugins/update_timer.go applyAuto) exactly:
// CheckUpdate resolves the diff, Prepare stages the new tarball, the rollback
// primitive applies it (a failing health check restores the previous tree),
// and only a healthy apply advances the market_installs record.
//
// Contract: unknown plugin → webui.ErrPluginNotFound (404), no update or a
// non-registry package → webui.ErrInvalidRequest (400), anything else 500.
func (a *App) marketUpdate(pluginID string) (webui.MarketUpdateResult, error) {
	pluginID = strings.TrimSpace(pluginID)
	if pluginID == "" {
		return webui.MarketUpdateResult{}, fmt.Errorf("%w: plugin id is required", webui.ErrInvalidRequest)
	}
	rows, err := a.store.ListMarketInstalls()
	if err != nil {
		return webui.MarketUpdateResult{}, err
	}
	var row *store.MarketInstall
	for i := range rows {
		if rows[i].PluginID == pluginID {
			row = &rows[i]
			break
		}
	}
	if row == nil {
		return webui.MarketUpdateResult{}, fmt.Errorf("%w: no market install for plugin %q", webui.ErrPluginNotFound, pluginID)
	}
	pkg := strings.TrimSpace(row.Package)
	// Only npm-shaped packages are update-checkable: a GitHub install
	// (owner/repo, D-04) or a local directory has no registry entry.
	if pkg == "" || (strings.Contains(pkg, "/") && !strings.HasPrefix(pkg, "@")) {
		return webui.MarketUpdateResult{}, fmt.Errorf("%w: package %q has no registry entry", webui.ErrInvalidRequest, pkg)
	}

	reg, err := a.marketRegistry()
	if err != nil {
		return webui.MarketUpdateResult{}, err
	}
	info, err := market.CheckUpdate(context.Background(), reg, pkg, row.Version, market.UpdatePolicyPrompt)
	if err != nil {
		return webui.MarketUpdateResult{}, err
	}
	if !info.Available {
		return webui.MarketUpdateResult{}, fmt.Errorf("%w: no update available for %s (%s -> %s)", webui.ErrInvalidRequest, pkg, info.From, info.To)
	}

	preparer, err := marketUpdatePreparerFor(a)
	if err != nil {
		return webui.MarketUpdateResult{}, err
	}
	bridgeExe, _ := a.bridgeExecutablePath() // 缺桥二进制也继续：只有需要生成桥 manifest 时才致命
	prepared, err := preparer.Prepare(context.Background(), row.Package+"@"+info.To, market.Options{
		Registry:         reg,
		FinalPluginDir:   filepath.Join(a.dataDir, "plugins", pluginID),
		BridgeExecutable: bridgeExe,
	})
	if err != nil {
		return webui.MarketUpdateResult{}, fmt.Errorf("%w: prepare %s: %v", webui.ErrInvalidRequest, info.To, err)
	}
	defer preparer.Cleanup(prepared)

	updater := marketUpdaterFor(a)
	if updater == nil {
		return webui.MarketUpdateResult{}, fmt.Errorf("app: market update: plugin manager is not available")
	}
	rolledBack, err := updater.UpdateDirWithRollback(context.Background(), pluginID, prepared.Root)
	if err != nil {
		return webui.MarketUpdateResult{}, fmt.Errorf("app: market update %s %s: apply failed (rolled back: %v): %w", pluginID, info.To, rolledBack, err)
	}

	if err := a.store.UpsertMarketInstall(store.MarketInstall{
		PluginID: row.PluginID,
		Package:  row.Package,
		Source:   row.Source,
		Version:  info.To,
		SHA256:   prepared.Record.SHA256,
	}); err != nil {
		return webui.MarketUpdateResult{}, err
	}
	return webui.MarketUpdateResult{OK: true, From: info.From, To: info.To, RolledBack: rolledBack}, nil
}

// marketUpdates backs GET /api/market/updates: the from→to diff of every
// recorded market install.
func (a *App) marketUpdates() (any, error) {
	rows, err := a.store.ListMarketInstalls()
	if err != nil {
		return nil, err
	}
	reg, err := a.marketRegistry()
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		// Only npm-shaped packages are update-checkable: a GitHub install
		// (owner/repo, D-04) or a local directory (no package) has no registry
		// entry, and asking would 404 the whole endpoint.
		pkg := strings.TrimSpace(row.Package)
		if pkg == "" || (strings.Contains(pkg, "/") && !strings.HasPrefix(pkg, "@")) {
			continue
		}
		info, err := market.CheckUpdate(context.Background(), reg, pkg, row.Version, market.UpdatePolicyPrompt)
		if err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"id":        row.PluginID,
			"from":      info.From,
			"to":        info.To,
			"available": info.Available,
		})
	}
	return out, nil
}
