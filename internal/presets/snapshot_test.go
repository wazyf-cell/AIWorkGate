package presets

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"aiworkgate/internal/model"
)

func TestSnapshotRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := SnapshotPath(dir)

	snap := Snapshot{
		Version: SnapshotVersion,
		SavedAt: 1760000000,
	}
	snap.UpsertProvider(SnapshotFromModelInfos("p1", []model.ModelInfo{
		{ID: "m1", Name: "Model One", Vendor: "V", ContextLength: 8192, Vision: true, Thinking: []string{"low", "high"}, Free: true},
		{ID: "", Name: "dropped"}, // 无 id 的条目丢弃
	}))
	snap.UpsertProvider(SnapshotFromModelInfos("p2", []model.ModelInfo{
		{ID: "m2", Vendor: "V2", ContextLength: 32768},
	}))

	if err := SaveSnapshot(path, snap); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	got, err := LoadSnapshot(path)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if got.Version != SnapshotVersion || got.SavedAt != 1760000000 {
		t.Fatalf("header = %+v", got)
	}
	if len(got.Providers) != 2 {
		t.Fatalf("providers = %d, want 2", len(got.Providers))
	}
	p1 := got.Section("p1")
	if len(p1) != 1 {
		t.Fatalf("p1 entries = %+v, want the single id-bearing entry", p1)
	}
	if p1[0].ID != "m1" || p1[0].DisplayName != "Model One" || p1[0].ContextLength != 8192 ||
		!p1[0].Vision || !p1[0].Free || len(p1[0].Efforts) != 2 {
		t.Fatalf("p1 entry = %+v", p1[0])
	}

	// 回到 model.ModelInfo 形状（兜底链快照源的输入）。
	infos := SnapshotEntriesToModelInfos(p1)
	if len(infos) != 1 || infos[0].ID != "m1" || !infos[0].Vision || infos[0].ContextLength != 8192 {
		t.Fatalf("ToModelInfos = %+v", infos)
	}
	if infos[0].Thinking[0] != "low" || infos[0].Thinking[1] != "high" {
		t.Fatalf("efforts lost in round trip: %+v", infos[0].Thinking)
	}

	// 落盘为 0600（Unix 上可断言；Windows 不保留权限位）。
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat snapshot: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("snapshot perm = %o, want 600", perm)
		}
	}
}

func TestSnapshotUpsertReplacesSectionOnly(t *testing.T) {
	snap := Snapshot{}
	snap.UpsertProvider(SnapshotProvider{Provider: "p1", Entries: []SnapshotEntry{{ID: "old"}}})
	snap.UpsertProvider(SnapshotProvider{Provider: "p2", Entries: []SnapshotEntry{{ID: "keep"}}})
	snap.UpsertProvider(SnapshotProvider{Provider: "p1", Entries: []SnapshotEntry{{ID: "new"}}})
	if len(snap.Providers) != 2 {
		t.Fatalf("providers = %d, want 2 after upsert", len(snap.Providers))
	}
	if got := snap.Section("p1"); len(got) != 1 || got[0].ID != "new" {
		t.Fatalf("p1 = %+v, want replaced", got)
	}
	if got := snap.Section("p2"); len(got) != 1 || got[0].ID != "keep" {
		t.Fatalf("p2 = %+v, want untouched", got)
	}
}

func TestSnapshotMissingAndCorrupt(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadSnapshot(SnapshotPath(dir))
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing snapshot error = %v, want fs.ErrNotExist", err)
	}

	corrupt := filepath.Join(dir, SnapshotFileName)
	if err := os.WriteFile(corrupt, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSnapshot(corrupt); err == nil {
		t.Fatal("corrupt snapshot must be an error")
	}
}

// TestSnapshotAtomicWrite pins the tmp+rename discipline: a finished save
// leaves exactly the snapshot file behind (no *.tmp residue), and a failing
// save (rename onto a directory) cleans its temp file and leaves the target
// untouched.
func TestSnapshotAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	path := SnapshotPath(dir)

	snap := Snapshot{}
	snap.UpsertProvider(SnapshotProvider{Provider: "p", Entries: []SnapshotEntry{{ID: "m"}}})
	if err := SaveSnapshot(path, snap); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != SnapshotFileName {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("dir after save = %v, want only %s (tmp+rename must not leak)", names, SnapshotFileName)
	}

	// 失败路径：目标路径本身是一个目录 → rename 失败，临时文件被清理。
	blocker := filepath.Join(dir, "occupied")
	if err := os.MkdirAll(blocker, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SaveSnapshot(blocker, snap); err == nil {
		t.Fatal("rename onto a directory must fail")
	}
	entries, err = os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var tmpLeft []string
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			tmpLeft = append(tmpLeft, e.Name())
		}
	}
	if len(tmpLeft) != 0 {
		t.Fatalf("failed save leaked temp files: %v", tmpLeft)
	}
	// 原快照文件仍在且可读。
	if _, err := LoadSnapshot(path); err != nil {
		t.Fatalf("old snapshot damaged by failed save: %v", err)
	}
}
