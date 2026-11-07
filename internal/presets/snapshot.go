package presets

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"aiworkgate/internal/model"
)

// D-49 provider catalog snapshot: the model directory a sync produced is
// persisted under the data directory so that "插件卸载或回退后模型不消失" and so
// the D-27⑤ fallback chain has a persistent floor below the plugin source.
// The store owns nothing here (the snapshot is not business data) — the file
// lives beside the database as plain JSON, written atomically.
const (
	// SnapshotFileName is the file name inside the caller-provided data dir.
	SnapshotFileName = "catalog-snapshot.json"
	// SnapshotVersion is the current on-disk schema version.
	SnapshotVersion = 1
)

// SnapshotEntry is one persisted model. The fields mirror the CatalogEntry /
// model.ModelInfo pair the fallback chain consumes (D-49: 快照结构 = 各 provider
// 的模型 id/name/context 等，按 fallback.go 的条目形状).
type SnapshotEntry struct {
	ID            string   `json:"id"`
	DisplayName   string   `json:"display_name,omitempty"`
	Vendor        string   `json:"vendor,omitempty"`
	ContextLength int      `json:"context_length,omitempty"`
	Vision        bool     `json:"vision,omitempty"`
	Efforts       []string `json:"efforts,omitempty"`
	Free          bool     `json:"free,omitempty"`
}

// SnapshotProvider is one provider's (plugin's) section of the snapshot.
type SnapshotProvider struct {
	Provider string          `json:"provider"`
	Entries  []SnapshotEntry `json:"entries"`
}

// Snapshot is the whole on-disk document.
type Snapshot struct {
	Version   int                `json:"version"`
	SavedAt   int64              `json:"saved_at"`
	Providers []SnapshotProvider `json:"providers"`
}

// SnapshotPath joins the data dir with the snapshot file name. The data dir is
// always a caller argument — the presets package never discovers it itself.
func SnapshotPath(dataDir string) string {
	return filepath.Join(dataDir, SnapshotFileName)
}

// SnapshotFromModelInfos converts one plugin's reported models into a snapshot
// section. Entries without an id are dropped.
func SnapshotFromModelInfos(provider string, infos []model.ModelInfo) SnapshotProvider {
	sec := SnapshotProvider{Provider: strings.TrimSpace(provider)}
	for _, info := range infos {
		id := strings.TrimSpace(info.ID)
		if id == "" {
			continue
		}
		sec.Entries = append(sec.Entries, SnapshotEntry{
			ID:            id,
			DisplayName:   strings.TrimSpace(info.Name),
			Vendor:        strings.TrimSpace(info.Vendor),
			ContextLength: info.ContextLength,
			Vision:        info.Vision,
			Efforts:       append([]string(nil), info.Thinking...),
			Free:          info.Free,
		})
	}
	return sec
}

// ToModelInfos flattens the section back into the shape the fallback chain's
// snapshot source consumes.
func (p SnapshotProvider) ToModelInfos() []model.ModelInfo {
	return SnapshotEntriesToModelInfos(p.Entries)
}

// SnapshotEntriesToModelInfos converts a bare entry list the same way.
func SnapshotEntriesToModelInfos(entries []SnapshotEntry) []model.ModelInfo {
	out := make([]model.ModelInfo, 0, len(entries))
	for _, e := range entries {
		id := strings.TrimSpace(e.ID)
		if id == "" {
			continue
		}
		out = append(out, model.ModelInfo{
			ID:            id,
			Name:          e.DisplayName,
			Vendor:        e.Vendor,
			ContextLength: e.ContextLength,
			Vision:        e.Vision,
			Thinking:      append([]string(nil), e.Efforts...),
			Free:          e.Free,
		})
	}
	return out
}

// UpsertProvider replaces the section of one provider, or appends it. This is
// how a per-plugin sync merges into the shared snapshot file without touching
// the other plugins' sections.
func (s *Snapshot) UpsertProvider(sec SnapshotProvider) {
	if strings.TrimSpace(sec.Provider) == "" {
		return
	}
	for i := range s.Providers {
		if s.Providers[i].Provider == sec.Provider {
			s.Providers[i] = sec
			return
		}
	}
	s.Providers = append(s.Providers, sec)
}

// Section returns one provider's entries (nil when absent).
func (s Snapshot) Section(provider string) []SnapshotEntry {
	provider = strings.TrimSpace(provider)
	for _, p := range s.Providers {
		if p.Provider == provider {
			return p.Entries
		}
	}
	return nil
}

// SaveSnapshot writes the snapshot atomically: the payload goes to a temp file
// in the same directory (0600, so a half-written catalog is not world readable)
// which is then renamed over the target. A crash between the two steps leaves
// the previous snapshot intact plus a *.tmp file that the next save's temp
// name never collides with.
func SaveSnapshot(path string, snap Snapshot) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("presets: snapshot path is required")
	}
	if snap.Version == 0 {
		snap.Version = SnapshotVersion
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("presets: encode snapshot: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, SnapshotFileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("presets: snapshot temp file: %w", err)
	}
	tmp := f.Name()
	if err := writeAndSync(f, data); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("presets: snapshot rename: %w", err)
	}
	return nil
}

func writeAndSync(f *os.File, data []byte) error {
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("presets: snapshot write: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("presets: snapshot sync: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("presets: snapshot close: %w", err)
	}
	return nil
}

// LoadSnapshot reads a snapshot written by SaveSnapshot. A missing file is
// reported with an error wrapping fs.ErrNotExist so callers can distinguish
// "no snapshot yet" from a corrupt one.
func LoadSnapshot(path string) (Snapshot, error) {
	var snap Snapshot
	if strings.TrimSpace(path) == "" {
		return snap, errors.New("presets: snapshot path is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return snap, fmt.Errorf("presets: load snapshot: %w", err)
	}
	if err := json.Unmarshal(data, &snap); err != nil {
		return snap, fmt.Errorf("presets: parse snapshot %s: %w", path, err)
	}
	if snap.Version == 0 {
		snap.Version = SnapshotVersion
	}
	// Drop blank provider sections and blank entries: they can only come from
	// a truncated or hand-edited file, and the fallback chain cannot use them.
	kept := make([]SnapshotProvider, 0, len(snap.Providers))
	for _, p := range snap.Providers {
		if strings.TrimSpace(p.Provider) == "" {
			continue
		}
		entries := make([]SnapshotEntry, 0, len(p.Entries))
		for _, e := range p.Entries {
			if strings.TrimSpace(e.ID) != "" {
				entries = append(entries, e)
			}
		}
		if len(entries) > 0 {
			p.Entries = entries
			kept = append(kept, p)
		}
	}
	snap.Providers = kept
	return snap, nil
}
