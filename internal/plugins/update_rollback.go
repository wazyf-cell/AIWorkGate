package plugins

// P8-④ 更新回退：备份 → 应用 → 健康检查 → 失败回滚（docs/p8/02 §4.3）。
//
// 目录惯例与 install.go 保持一致：旧树备份为 <dest>.old-<UnixNano>，原子
// rename 换入新树。与 UpdateDir 的区别：UpdateDir 装完即忘、自删备份、不查
// 健康；这里在 UpdateDir 之外补上「换树前先保住旧树 + 换树后健康检查 + 失败
// 回滚」三层，因此滚动失败时旧版本仍然可服务（P8 验收④）。
//
// 本文件不含任何真实进程/文件系统调用：所有副作用都是注入的闭包，单测用
// fake health checker 即可覆盖成功/回滚/回滚失败三条路径（rollback_test.go）。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// maxKeepOldBackups is the retention ceiling per plugin (docs/p8/02 §4.3):
// the newest two .old-<ts> copies are kept, older ones pruned after a
// successful update.
const maxKeepOldBackups = 2

// updateRollbackPlan is the pure, injected update-with-rollback sequence.
// Every step is a closure so rollback_test.go can drive it with fakes and no
// real plugin process ever runs.
type updateRollbackPlan struct {
	// backupDir moves the current installed tree to <dest>.old-<ts> BEFORE the
	// apply, so a failed swap always has a previous copy to come back to.
	backupDir func() error
	// apply installs the new version tree into <dest> (m.UpdateDir in wiring).
	apply func() error
	// stop stops a running (possibly unhealthy) plugin process.
	stop func() error
	// startAndWait starts the plugin if needed and waits for /health; the
	// first call health-checks the NEW tree, the second (after restore) the
	// OLD tree.
	startAndWait func(ctx context.Context) error
	// restoreOld removes <dest> and renames <dest>.old-<ts> back into place,
	// then re-syncs manager state and settings to the restored manifest.
	restoreOld func() error
	// prune trims old backups after a successful update (retention policy).
	prune func()
	// logInfo / logWarn mirror Manager.logInfo/logWarn.
	logInfo func(msg string, fields map[string]any)
	logWarn func(msg string, fields map[string]any)
}

// run executes the update and, on a health failure, the rollback. Semantics:
//
//	(false, nil)  update applied and healthy; no rollback happened
//	(true,  nil)  new version failed health; old tree is serving again
//	(true,  err)  update failed pre-swap / apply; old tree was restored
//	(false, err)  rollback ALSO failed (or no previous copy): plugin is DOWN,
//	              the error names both attempts so an operator can debug
func (p *updateRollbackPlan) run(ctx context.Context, id string) (bool, error) {
	if err := p.backupDir(); err != nil {
		return false, fmt.Errorf("plugins: update %q: backup current copy: %w", id, err)
	}

	if err := p.apply(); err != nil {
		if rerr := p.restoreOld(); rerr != nil {
			return false, fmt.Errorf("plugins: update %q failed (%v); rollback ALSO failed: %v — plugin installation is DOWN", id, err, rerr)
		}
		p.logInfo("plugin update failed before swap; previous copy restored",
			map[string]any{"plugin": id, "error": err.Error()})
		return true, fmt.Errorf("plugins: update %q: %v (previous copy restored)", id, err)
	}

	if err := p.startAndWait(ctx); err != nil {
		p.logWarn("new version failed its health check; rolling back to previous copy",
			map[string]any{"plugin": id, "error": err.Error()})
		if serr := p.stop(); serr != nil {
			p.logWarn("stop of the unhealthy new version failed; continuing with restore",
				map[string]any{"plugin": id, "error": serr.Error()})
		}
		if rerr := p.restoreOld(); rerr != nil {
			return false, fmt.Errorf("plugins: update %q: new version failed health check (%v); rollback ALSO failed: %v — plugin is DOWN", id, err, rerr)
		}
		if herr := p.startAndWait(ctx); herr != nil {
			return false, fmt.Errorf("plugins: update %q: rolled back to previous copy but IT also fails: %v (original failure: %v) — plugin is DOWN", id, herr, err)
		}
		p.logInfo("plugin update rolled back; previous version is serving", map[string]any{"plugin": id})
		return true, nil
	}

	p.prune()
	p.logInfo("plugin update applied and healthy; old backups pruned", map[string]any{"plugin": id})
	return false, nil
}

// runUpdateRollback wires updateRollbackPlan to a real Manager. It reuses the
// existing UpdateDir install path (same validation/staging/swapping) without
// modifying install.go: the old tree is preserved here BEFORE the update, so
// UpdateDir's own internal (self-deleted) backup never becomes the rollback
// source - ours does.
func runUpdateRollback(ctx context.Context, id, srcDir string, m *Manager) (bool, error) {
	if !validPluginID(id) {
		return false, fmt.Errorf("plugins: update %q: invalid plugin id", id)
	}
	dest := m.installDir(id)
	backup := fmt.Sprintf("%s.old-%d", dest, m.now().UnixNano())

	// 插件未启用时没有进程可健康检查：换树成功即视为应用成功（不入回滚判定）。
	var enabled bool
	m.mu.Lock()
	if p, ok := m.plugins[id]; ok {
		enabled = p.enabled
	}
	m.mu.Unlock()

	plan := &updateRollbackPlan{
		backupDir: func() error {
			if _, err := os.Stat(dest); err != nil {
				if os.IsNotExist(err) {
					return fmt.Errorf("plugin %q is not installed (no %s)", id, dest)
				}
				return err
			}
			return os.Rename(dest, backup)
		},
		apply: func() error { return m.UpdateDir(srcDir) },
		stop:  func() error { return m.Stop(id) },
		startAndWait: func(ctx context.Context) error {
			if !enabled {
				return nil
			}
			return m.EnsureRunning(ctx, id)
		},
		restoreOld: func() error { return restorePluginTree(m, id, dest, backup) },
		prune:      func() { m.pruneOldBackups(id, maxKeepOldBackups) },
		logInfo:    m.logInfo,
		logWarn:    m.logWarn,
	}
	return plan.run(ctx, id)
}

// restorePluginTree swaps the previous copy back into <dest> and re-syncs the
// manager to the restored manifest: in-memory state (refreshPlugin) and the
// persisted settings row are both derived from the OLD manifest after an
// UpdateDir that had persisted the NEW one.
func restorePluginTree(m *Manager, id, dest, backup string) error {
	if _, err := os.Stat(backup); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("previous copy %s does not exist; nothing to restore", backup)
		}
		return err
	}
	if err := os.RemoveAll(dest); err != nil {
		return fmt.Errorf("remove failed new tree %s: %w", dest, err)
	}
	if err := os.Rename(backup, dest); err != nil {
		return fmt.Errorf("restore %s back to %s: %w", backup, dest, err)
	}
	mf, err := loadManifestDir(dest)
	if err != nil {
		return fmt.Errorf("restored tree %s has no valid manifest: %w", dest, err)
	}
	if err := m.refreshPlugin(id, dest, mf); err != nil {
		return fmt.Errorf("reload plugin state from restored tree: %w", err)
	}
	// UpdateDir 把新版本的 manifest 默认值写进了设置行；回滚后按旧 manifest
	// 重新派生并持久化（保留用户可编辑值，installed_at 锚点不动）。
	s := m.installSettings(mf, true, InstallOpts{})
	if err := m.persistSettings(id, s); err != nil {
		return fmt.Errorf("persist settings for restored tree: %w", err)
	}
	return nil
}

// pruneOldBackups deletes all but the newest keep copies of <dest>.old-*.
// The timestamp suffix is UnixNano, so lexicographic order equals time order.
func (m *Manager) pruneOldBackups(id string, keep int) {
	if keep < 0 {
		keep = 0
	}
	pattern := m.installDir(id) + ".old-*"
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) <= keep {
		return
	}
	sort.Strings(matches)
	for _, old := range matches[:len(matches)-keep] {
		if err := os.RemoveAll(old); err != nil {
			m.logWarn("cannot prune old plugin backup", map[string]any{"path": old, "error": err.Error()})
		}
	}
}
