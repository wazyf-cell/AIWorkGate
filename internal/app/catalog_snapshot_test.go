package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"aiworkgate/internal/model"
	"aiworkgate/internal/presets"
	"aiworkgate/internal/store"
)

// D-49 接线测试：sync 成功 → 快照落盘（含其他插件 section 保留）；sync 失败/
// 空列表 → 快照条目兜底回 model_catalog。

func catalogTestEntries(pluginID string) []model.ModelCatalogEntry {
	now := time.Now().Unix()
	return []model.ModelCatalogEntry{
		{PluginID: pluginID, ModelID: "glm-5", DisplayName: "GLM-5", Vendor: "智谱",
			Thinking: []string{"low", "high"}, ContextLength: 200000, Vision: true, SyncedAt: now},
		{PluginID: pluginID, ModelID: "glm-5-air", DisplayName: "GLM-5-Air", Vendor: "智谱",
			ContextLength: 128000, Free: true, SyncedAt: now},
	}
}

func TestCatalogSnapshotSaveAndRestore(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	a := &App{store: st, dataDir: t.TempDir()}

	// 同步成功 → 快照落盘。
	entries := catalogTestEntries("plug-zhipu")
	a.saveCatalogSnapshot("plug-zhipu", entries)
	if _, err := os.Stat(a.catalogSnapshotPath()); err != nil {
		t.Fatalf("snapshot file missing: %v", err)
	}

	// 目录被清空（等价于插件卸载/回退后目录消失）。
	if err := st.ReplaceModelCatalog("plug-zhipu", nil); err != nil {
		t.Fatalf("wipe catalog: %v", err)
	}

	// 兜底：快照条目喂回 model_catalog。
	n := a.restoreCatalogFromSnapshot("plug-zhipu")
	if n != 2 {
		t.Fatalf("restored %d rows, want 2", n)
	}
	rows, err := st.ListModelCatalog("plug-zhipu")
	if err != nil {
		t.Fatalf("ListModelCatalog: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("catalog rows = %d, want 2", len(rows))
	}
	byID := map[string]model.ModelCatalogEntry{}
	for _, r := range rows {
		byID[r.ModelID] = r
	}
	m := byID["glm-5"]
	if m.Vendor != "智谱" || m.ContextLength != 200000 || !m.Vision || len(m.Thinking) != 2 {
		t.Fatalf("restored glm-5 = %+v, metadata lost", m)
	}
	if !byID["glm-5-air"].Free {
		t.Fatalf("restored glm-5-air = %+v, free flag lost", byID["glm-5-air"])
	}
}

func TestCatalogSnapshotKeepsOtherPlugins(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	a := &App{store: st, dataDir: t.TempDir()}

	a.saveCatalogSnapshot("p1", catalogTestEntries("p1"))
	a.saveCatalogSnapshot("p2", []model.ModelCatalogEntry{
		{PluginID: "p2", ModelID: "other-model", Vendor: "Other", SyncedAt: time.Now().Unix()},
	})

	// 两个 section 都在。
	snap, err := presets.LoadSnapshot(a.catalogSnapshotPath())
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if len(snap.Providers) != 2 || snap.Section("p2") == nil {
		t.Fatalf("snapshot providers = %+v, want p1+p2", snap.Providers)
	}
	// p1 重新同步（只保留一个模型）→ p2 section 不动。
	a.saveCatalogSnapshot("p1", catalogTestEntries("p1")[:1])
	snap, err = presets.LoadSnapshot(a.catalogSnapshotPath())
	if err != nil {
		t.Fatalf("LoadSnapshot after resync: %v", err)
	}
	if len(snap.Section("p1")) != 1 || len(snap.Section("p2")) != 1 {
		t.Fatalf("sections after resync: p1=%d p2=%d, want 1/1", len(snap.Section("p1")), len(snap.Section("p2")))
	}
	// 兜底互不串台。
	if n := a.restoreCatalogFromSnapshot("p2"); n != 1 {
		t.Fatalf("restore p2 = %d, want 1", n)
	}
	rows, _ := st.ListModelCatalog("p2")
	if len(rows) != 1 || rows[0].ModelID != "other-model" {
		t.Fatalf("p2 rows = %+v", rows)
	}
}

func TestRestoreCatalogFromSnapshotWithoutSnapshot(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// 无快照文件 → 0，目录不动。
	a := &App{store: st, dataDir: t.TempDir()}
	if n := a.restoreCatalogFromSnapshot("p1"); n != 0 {
		t.Fatalf("restore without snapshot = %d, want 0", n)
	}

	// 无 dataDir / 无 store → 0。
	if n := (&App{store: st}).restoreCatalogFromSnapshot("p1"); n != 0 {
		t.Fatalf("restore without dataDir = %d, want 0", n)
	}
	if n := (&App{dataDir: t.TempDir()}).restoreCatalogFromSnapshot("p1"); n != 0 {
		t.Fatalf("restore without store = %d, want 0", n)
	}

	// 损坏的快照 → 0，不 panic。
	corrupt := &App{store: st, dataDir: t.TempDir()}
	if err := os.WriteFile(corrupt.catalogSnapshotPath(), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n := corrupt.restoreCatalogFromSnapshot("p1"); n != 0 {
		t.Fatalf("restore with corrupt snapshot = %d, want 0", n)
	}
}

// TestCatalogSnapshotPathLayout pins the file name inside the caller-supplied
// data dir (the presets package owns the format; app only passes the path).
func TestCatalogSnapshotPathLayout(t *testing.T) {
	if got := (&App{dataDir: `C:\data`}).catalogSnapshotPath(); filepath.Base(got) != "catalog-snapshot.json" {
		t.Fatalf("snapshot path = %q, want base catalog-snapshot.json", got)
	}
}
