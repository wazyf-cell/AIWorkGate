package app

import (
	"errors"
	"os"
	"strings"
	"time"

	"aiworkgate/internal/model"
	"aiworkgate/internal/presets"
)

// D-49 provider 目录快照接线：syncPoolCatalog 成功后把该插件的模型目录落盘到
// <dataDir>\catalog-snapshot.json；同步失败或拿到空列表时，把快照条目作为兜底
// 源喂回 model_catalog（D-27⑤ 兜底链的最底层），保证「插件卸载或回退后模型
// 不消失」。落盘本身归 presets 包管（原子写、0600），这里只做调用与日志。

// catalogSnapshotPath is where the D-49 snapshot lives for this host.
func (a *App) catalogSnapshotPath() string {
	return presets.SnapshotPath(a.dataDir)
}

// saveCatalogSnapshot merges one plugin's freshly synced catalog into the
// snapshot file. Failure is logged, never fatal: the snapshot is a fallback
// convenience, and a broken write must not fail a successful sync.
func (a *App) saveCatalogSnapshot(pluginID string, entries []model.ModelCatalogEntry) {
	if strings.TrimSpace(a.dataDir) == "" || strings.TrimSpace(pluginID) == "" {
		return
	}
	infos := make([]model.ModelInfo, 0, len(entries))
	for _, e := range entries {
		if strings.TrimSpace(e.ModelID) == "" {
			continue
		}
		infos = append(infos, model.ModelInfo{
			ID:            e.ModelID,
			Name:          e.DisplayName,
			Vendor:        e.Vendor,
			ContextLength: e.ContextLength,
			Vision:        e.Vision,
			Thinking:      e.Thinking,
			Free:          e.Free,
		})
	}
	if len(infos) == 0 {
		return
	}
	snap := presets.Snapshot{Version: presets.SnapshotVersion, SavedAt: time.Now().Unix()}
	// Keep the other plugins' sections: reload, upsert this one, save.
	if old, err := presets.LoadSnapshot(a.catalogSnapshotPath()); err == nil {
		snap = old
	} else if a.logger != nil && !isSnapshotMissing(err) {
		a.logger.Warn("app", "catalog snapshot reload failed, rewriting", map[string]any{
			"error": err.Error(),
		})
	}
	snap.UpsertProvider(presets.SnapshotFromModelInfos(pluginID, infos))
	snap.SavedAt = time.Now().Unix()
	if err := presets.SaveSnapshot(a.catalogSnapshotPath(), snap); err != nil && a.logger != nil {
		a.logger.Warn("app", "catalog snapshot save failed", map[string]any{
			"plugin": pluginID, "error": err.Error(),
		})
	}
}

// restoreCatalogFromSnapshot feeds the persisted snapshot back into
// model_catalog when a sync failed or came back empty (D-27⑤ bottom of the
// chain). Returns the number of rows restored; 0 means "nothing to fall back
// to" and leaves the catalog untouched.
func (a *App) restoreCatalogFromSnapshot(pluginID string) int {
	if a.store == nil || strings.TrimSpace(a.dataDir) == "" || strings.TrimSpace(pluginID) == "" {
		return 0
	}
	snap, err := presets.LoadSnapshot(a.catalogSnapshotPath())
	if err != nil {
		if a.logger != nil && !isSnapshotMissing(err) {
			a.logger.Warn("app", "catalog snapshot load failed", map[string]any{"error": err.Error()})
		}
		return 0
	}
	section := snap.Section(pluginID)
	if len(section) == 0 {
		return 0
	}
	now := time.Now().Unix()
	entries := make([]model.ModelCatalogEntry, 0, len(section))
	for _, e := range presets.SnapshotEntriesToModelInfos(section) {
		entries = append(entries, model.ModelCatalogEntry{
			PluginID:      pluginID,
			ModelID:       e.ID,
			DisplayName:   e.Name,
			Vendor:        e.Vendor,
			Thinking:      e.Thinking,
			ContextLength: e.ContextLength,
			Vision:        e.Vision,
			Free:          e.Free,
			SyncedAt:      now,
		})
	}
	if err := a.store.ReplaceModelCatalog(pluginID, entries); err != nil {
		if a.logger != nil {
			a.logger.Warn("app", "catalog snapshot restore failed", map[string]any{
				"plugin": pluginID, "error": err.Error(),
			})
		}
		return 0
	}
	a.snapMu.Lock()
	delete(a.snapModels, pluginID)
	a.snapMu.Unlock()
	a.bumpSchedulerVersion()
	if a.logger != nil {
		a.logger.Info("app", "model catalog restored from snapshot", map[string]any{
			"plugin": pluginID, "models": len(entries), "saved_at": snap.SavedAt,
		})
	}
	return len(entries)
}

// isSnapshotMissing reports whether the error is just "no snapshot file yet".
func isSnapshotMissing(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}
