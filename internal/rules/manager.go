// Package rules owns the shared rule packs (M6). The host keeps the packs in
// SQLite, merges the packs a plugin declares in its manifest into
// <dataDir>\runtime\<pluginID>\rules.json and then asks the plugin to reload.
// Cleaning itself is always executed inside the plugin; the host only supplies
// text. Rule packs are literal text replacements plus regexp strips.
package rules

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"aiworkgate/internal/logging"
	"aiworkgate/internal/model"
	"aiworkgate/internal/store"
)

const (
	// runtimeDirName is the per-plugin runtime directory under DataDir.
	runtimeDirName = "runtime"
	// rulesFileName is injected to plugins as AIWORKGATE_RULES_FILE.
	rulesFileName = "rules.json"
	// logModule is the module field of every host log line from this package.
	logModule = "rules"
)

var (
	// ErrInvalidPack is returned for packs that must not be stored: bad name,
	// empty rule side or a strip pattern that does not compile.
	ErrInvalidPack = errors.New("rules: invalid pack")
	// ErrPackNotFound is returned when a plugin declares an unknown pack.
	ErrPackNotFound = errors.New("rules: pack not found")
	// ErrInvalidPluginID is returned for ids that cannot name a runtime dir.
	ErrInvalidPluginID = errors.New("rules: invalid plugin id")
)

// nameRe is the manifest id rule (2..32 chars); pack names use it too so both
// stay safe as file/directory names.
var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,31}$`)

// Reloader is the plugin lifecycle hook needed after a rules file changes. The
// host's plugin manager implements it (POST /reload); this package stays free
// of any internal/plugins dependency.
type Reloader interface {
	Reload(ctx context.Context, pluginID string) error
}

// Manager stores packs and materializes the merged file each plugin reads.
type Manager struct {
	Store    *store.Store
	DataDir  string
	Log      *logging.Logger
	Reloader Reloader

	// mu serializes seed checks and rules.json writes.
	mu sync.Mutex
}

// New returns a manager. Any of the collaborators may be nil: without a store
// every operation fails, without a logger nothing is logged, and without a
// reloader the file is written but no plugin is notified.
func New(st *store.Store, dataDir string, log *logging.Logger, reloader Reloader) *Manager {
	return &Manager{Store: st, DataDir: dataDir, Log: log, Reloader: reloader}
}

// ListPacks returns every stored pack ordered by name, content decoded.
func (m *Manager) ListPacks() ([]model.RulePack, error) {
	st, err := m.store()
	if err != nil {
		return nil, err
	}
	recs, err := st.ListRulePacks()
	if err != nil {
		return nil, err
	}
	out := make([]model.RulePack, 0, len(recs))
	for _, rec := range recs {
		p, err := decodePack(rec.Name, rec.Content)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// GetPack loads one pack. The bool reports whether the pack exists.
func (m *Manager) GetPack(name string) (model.RulePack, bool, error) {
	st, err := m.store()
	if err != nil {
		return model.RulePack{}, false, err
	}
	content, ok, err := st.GetRulePack(name)
	if err != nil || !ok {
		return model.RulePack{}, ok, err
	}
	p, err := decodePack(name, content)
	if err != nil {
		return model.RulePack{}, false, err
	}
	return p, true, nil
}

// SavePack validates and persists a pack. Validation is strict because a bad
// pack would otherwise reach every plugin that declares it: the name must match
// the manifest id rule, both sides of every rule must be non-empty, and every
// strip pattern must compile as a Go regexp.
func (m *Manager) SavePack(p model.RulePack) error {
	st, err := m.store()
	if err != nil {
		return err
	}
	p.Name = strings.TrimSpace(p.Name)
	if err := validatePack(p); err != nil {
		return err
	}
	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("rules: encode pack %q: %w", p.Name, err)
	}
	if err := st.UpsertRulePack(p.Name, string(data)); err != nil {
		return err
	}
	m.log().Info(logModule, "rule pack saved", map[string]any{
		"name": p.Name, "rules": len(p.Rules), "strip": len(p.Strip),
	})
	return nil
}

// DeletePack removes a pack. Unknown names are a no-op.
func (m *Manager) DeletePack(name string) error {
	st, err := m.store()
	if err != nil {
		return err
	}
	if !nameRe.MatchString(name) {
		return fmt.Errorf("%w: name %q must match %s", ErrInvalidPack, name, nameRe)
	}
	if err := st.DeleteRulePack(name); err != nil {
		return err
	}
	m.log().Info(logModule, "rule pack deleted", map[string]any{"name": name})
	return nil
}

// SeedBuiltins creates the builtin pack when it is absent. It never overwrites
// an existing pack, so user edits survive restarts and repeated calls.
func (m *Manager) SeedBuiltins() error {
	st, err := m.store()
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	_, ok, err := st.GetRulePack(BuiltinWBPack)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	if err := m.SavePack(builtinWBPack()); err != nil {
		return fmt.Errorf("rules: seed %s: %w", BuiltinWBPack, err)
	}
	return nil
}

// WriteForPlugin merges the named packs (in the given order) and atomically
// replaces <DataDir>\runtime\<pluginID>\rules.json. An empty pack list still
// writes an empty document so plugins always find the file. On success it
// returns the path to inject as AIWORKGATE_RULES_FILE.
//
// Reloader failures are returned to the caller (the file is already written,
// so the path is returned alongside the error); a caller that treats "reload
// not implemented" as harmless can ignore that error.
func (m *Manager) WriteForPlugin(ctx context.Context, pluginID string, packs []string) (string, error) {
	if !nameRe.MatchString(pluginID) {
		return "", fmt.Errorf("%w: %q must match %s", ErrInvalidPluginID, pluginID, nameRe)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	merged, err := m.merge(packs)
	if err != nil {
		return "", err
	}
	path := m.rulesPath(pluginID)
	if err := writeAtomic(path, merged); err != nil {
		return "", err
	}
	if m.Reloader != nil {
		if err := m.Reloader.Reload(ctx, pluginID); err != nil {
			return path, fmt.Errorf("rules: reload plugin %q: %w", pluginID, err)
		}
	}
	m.log().Info(logModule, "rules written", map[string]any{
		"plugin": pluginID, "packs": merged.Packs,
		"rules": len(merged.Rules), "strip": len(merged.Strip),
	})
	return path, nil
}

// WriteAll refreshes the rules file of every plugin in assignments (plugin id
// to declared packs). Every plugin is attempted; failures are joined.
func (m *Manager) WriteAll(ctx context.Context, assignments map[string][]string) error {
	ids := make([]string, 0, len(assignments))
	for id := range assignments {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var errs []error
	for _, id := range ids {
		if _, err := m.WriteForPlugin(ctx, id, assignments[id]); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// merge concatenates rules in pack order and dedupes strip patterns (first
// occurrence wins, order preserved). Unknown packs abort the merge.
func (m *Manager) merge(packNames []string) (model.MergedRules, error) {
	st, err := m.store()
	if err != nil {
		return model.MergedRules{}, err
	}
	merged := model.MergedRules{
		Packs: []string{},
		Rules: []model.Rule{},
		Strip: []model.StripPattern{},
	}
	seenPacks := make(map[string]bool, len(packNames))
	seenStrip := make(map[string]bool)

	for _, name := range packNames {
		name = strings.TrimSpace(name)
		if name == "" || seenPacks[name] {
			continue
		}
		seenPacks[name] = true

		content, ok, err := st.GetRulePack(name)
		if err != nil {
			return model.MergedRules{}, err
		}
		if !ok {
			return model.MergedRules{}, fmt.Errorf("%w: %s", ErrPackNotFound, name)
		}
		p, err := decodePack(name, content)
		if err != nil {
			return model.MergedRules{}, err
		}
		merged.Packs = append(merged.Packs, name)
		for _, r := range p.Rules {
			if strings.TrimSpace(r.From) == "" {
				continue
			}
			merged.Rules = append(merged.Rules, r)
		}
		for _, s := range p.Strip {
			pattern := s.Pattern
			if strings.TrimSpace(pattern) == "" || seenStrip[pattern] {
				continue
			}
			seenStrip[pattern] = true
			merged.Strip = append(merged.Strip, model.StripPattern{Pattern: pattern})
		}
	}
	if err := validateStrip(merged.Strip); err != nil {
		return model.MergedRules{}, err
	}
	return merged, nil
}

// rulesPath is the merged document location for a plugin.
func (m *Manager) rulesPath(pluginID string) string {
	return filepath.Join(m.DataDir, runtimeDirName, pluginID, rulesFileName)
}

// store returns the store or a descriptive error.
func (m *Manager) store() (*store.Store, error) {
	if m == nil || m.Store == nil {
		return nil, errors.New("rules: manager has no store")
	}
	return m.Store, nil
}

// log returns the logger; nil is safe (logging.Logger methods tolerate it).
func (m *Manager) log() *logging.Logger {
	if m == nil {
		return nil
	}
	return m.Log
}

// decodePack turns a stored JSON document into a pack. The row name wins so a
// hand-edited document cannot rename a pack.
func decodePack(name, content string) (model.RulePack, error) {
	if strings.TrimSpace(content) == "" {
		return model.RulePack{Name: name}, nil
	}
	var p model.RulePack
	if err := json.Unmarshal([]byte(content), &p); err != nil {
		return model.RulePack{}, fmt.Errorf("rules: decode pack %q: %w", name, err)
	}
	p.Name = name
	return p, nil
}

// validatePack enforces the save-time rules.
func validatePack(p model.RulePack) error {
	if !nameRe.MatchString(p.Name) {
		return fmt.Errorf("%w: name %q must match %s", ErrInvalidPack, p.Name, nameRe)
	}
	for i, r := range p.Rules {
		if strings.TrimSpace(r.From) == "" {
			return fmt.Errorf("%w: %s: rule %d has an empty from", ErrInvalidPack, p.Name, i)
		}
		if strings.TrimSpace(r.To) == "" {
			return fmt.Errorf("%w: %s: rule %d has an empty to", ErrInvalidPack, p.Name, i)
		}
	}
	return validateStrip(p.Strip)
}

// validateStrip requires every pattern to be a non-empty, compiling regexp.
func validateStrip(strips []model.StripPattern) error {
	for i, s := range strips {
		if strings.TrimSpace(s.Pattern) == "" {
			return fmt.Errorf("%w: strip pattern %d is empty", ErrInvalidPack, i)
		}
		if _, err := regexp.Compile(s.Pattern); err != nil {
			return fmt.Errorf("%w: strip pattern %d %q: %w", ErrInvalidPack, i, s.Pattern, err)
		}
	}
	return nil
}

// writeAtomic writes doc to path via a temp file in the same directory plus a
// rename, so a plugin reading the file never sees a partial document.
func writeAtomic(path string, doc model.MergedRules) error {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("rules: encode merged rules: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("rules: create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".rules-*.tmp")
	if err != nil {
		return fmt.Errorf("rules: create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeded

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("rules: write %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("rules: sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("rules: close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rules: replace %s: %w", path, err)
	}
	return nil
}
