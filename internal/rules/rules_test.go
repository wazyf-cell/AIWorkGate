package rules

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"aiworkgate/internal/logging"
	"aiworkgate/internal/model"
	"aiworkgate/internal/store"
)

// recordingReloader remembers reload calls and can fail on demand.
type recordingReloader struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (r *recordingReloader) Reload(ctx context.Context, pluginID string) error {
	if ctx == nil {
		return errors.New("nil context")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, pluginID)
	return r.err
}

func (r *recordingReloader) called() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func (r *recordingReloader) setErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

func newTestManager(t *testing.T, reloader Reloader) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	log, err := logging.NewLogger(dir)
	if err != nil {
		t.Fatalf("logging.NewLogger: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	return New(st, dir, log, reloader), dir
}

func mustSave(t *testing.T, m *Manager, p model.RulePack) {
	t.Helper()
	if err := m.SavePack(p); err != nil {
		t.Fatalf("SavePack(%q): %v", p.Name, err)
	}
}

func readMerged(t *testing.T, path string) model.MergedRules {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc model.MergedRules
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return doc
}

func TestSeedBuiltinsIdempotent(t *testing.T) {
	m, _ := newTestManager(t, nil)

	for i := 0; i < 2; i++ {
		if err := m.SeedBuiltins(); err != nil {
			t.Fatalf("SeedBuiltins #%d: %v", i+1, err)
		}
	}

	packs, err := m.ListPacks()
	if err != nil {
		t.Fatalf("ListPacks: %v", err)
	}
	if len(packs) != 1 {
		t.Fatalf("ListPacks returned %d packs, want 1", len(packs))
	}
	p := packs[0]
	if p.Name != BuiltinWBPack {
		t.Fatalf("pack name = %q, want %q", p.Name, BuiltinWBPack)
	}
	if got, want := len(p.Rules), len(builtinWBRules()); got != want {
		t.Fatalf("builtin rules = %d, want %d", got, want)
	}
	// F80 counted 13 pairs; A's current table has 18. Guard against a partial port.
	if len(p.Rules) < 13 {
		t.Fatalf("builtin rules = %d, want at least 13", len(p.Rules))
	}
	if len(p.Strip) != 1 {
		t.Fatalf("builtin strip = %d patterns, want 1", len(p.Strip))
	}
	if _, err := regexp.Compile(p.Strip[0].Pattern); err != nil {
		t.Fatalf("builtin strip does not compile: %v", err)
	}

	// Spot-check A's first and last pairs to prove the table was ported verbatim.
	wantPairs := map[string]string{
		"You are Claude Code, Anthropic's official CLI for Claude.": "You are Claude Code, Anthropic's official CLI tool for Claude.",
		"You are a coding agent powered by the glm-":                "You are a coding agent built on the glm-",
	}
	for from, to := range wantPairs {
		found := false
		for _, r := range p.Rules {
			if r.From == from && r.To == to {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("builtin pack missing pair %q -> %q", from, to)
		}
	}

	// A user edit must survive another seed.
	p.Description = "edited"
	p.Rules = p.Rules[:1]
	mustSave(t, m, p)
	if err := m.SeedBuiltins(); err != nil {
		t.Fatalf("SeedBuiltins after edit: %v", err)
	}
	got, ok, err := m.GetPack(BuiltinWBPack)
	if err != nil || !ok {
		t.Fatalf("GetPack = (%v, %v)", ok, err)
	}
	if got.Description != "edited" || len(got.Rules) != 1 {
		t.Fatalf("seed overwrote user edit: description %q, %d rules", got.Description, len(got.Rules))
	}

	// DeletePack is idempotent and rejects unsafe names.
	if err := m.DeletePack(BuiltinWBPack); err != nil {
		t.Fatalf("DeletePack: %v", err)
	}
	if _, ok, err := m.GetPack(BuiltinWBPack); err != nil || ok {
		t.Fatalf("GetPack after delete = (%v, %v), want (false, nil)", ok, err)
	}
	if err := m.DeletePack(BuiltinWBPack); err != nil {
		t.Fatalf("second DeletePack: %v", err)
	}
	if err := m.DeletePack("Bad_Name"); !errors.Is(err, ErrInvalidPack) {
		t.Fatalf("DeletePack(Bad_Name) = %v, want ErrInvalidPack", err)
	}
}

func TestWriteForPluginMergeOrderAndDedupe(t *testing.T) {
	reloader := &recordingReloader{}
	m, dir := newTestManager(t, reloader)

	mustSave(t, m, model.RulePack{
		Name:  "pack-a",
		Rules: []model.Rule{{From: "a1", To: "A1"}, {From: "a2", To: "A2"}},
		Strip: []model.StripPattern{{Pattern: `cc_sig=[A-Za-z0-9]+`}, {Pattern: `shared_[a-z]+`}},
	})
	mustSave(t, m, model.RulePack{
		Name:  "pack-b",
		Rules: []model.Rule{{From: "b1", To: "B1"}},
		Strip: []model.StripPattern{{Pattern: `shared_[a-z]+`}, {Pattern: `x-anthropic-[a-z]+=\S+`}},
	})

	path, err := m.WriteForPlugin(context.Background(), "plug-1", []string{"pack-a", "pack-b", "pack-a"})
	if err != nil {
		t.Fatalf("WriteForPlugin: %v", err)
	}
	if want := filepath.Join(dir, "runtime", "plug-1", "rules.json"); path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}

	doc := readMerged(t, path)
	if got, want := strings.Join(doc.Packs, ","), "pack-a,pack-b"; got != want {
		t.Fatalf("packs = %q, want %q", got, want)
	}
	var froms []string
	for _, r := range doc.Rules {
		froms = append(froms, r.From)
	}
	if got, want := strings.Join(froms, ","), "a1,a2,b1"; got != want {
		t.Fatalf("rules order = %q, want %q", got, want)
	}
	var patterns []string
	for _, s := range doc.Strip {
		patterns = append(patterns, s.Pattern)
	}
	wantPatterns := `cc_sig=[A-Za-z0-9]+,shared_[a-z]+,x-anthropic-[a-z]+=\S+`
	if got := strings.Join(patterns, ","); got != wantPatterns {
		t.Fatalf("strip = %q, want %q", got, wantPatterns)
	}

	if got := reloader.called(); len(got) != 1 || got[0] != "plug-1" {
		t.Fatalf("reload calls = %v, want [plug-1]", got)
	}

	// A plugin with no packs still gets a valid empty document.
	emptyPath, err := m.WriteForPlugin(context.Background(), "plug-2", nil)
	if err != nil {
		t.Fatalf("WriteForPlugin(no packs): %v", err)
	}
	emptyDoc := readMerged(t, emptyPath)
	if len(emptyDoc.Packs) != 0 || len(emptyDoc.Rules) != 0 || len(emptyDoc.Strip) != 0 {
		t.Fatalf("empty merge = %+v, want empty arrays", emptyDoc)
	}
	// The arrays must be encoded as [] rather than null for strict readers.
	if !strings.Contains(readFileString(t, emptyPath), `"rules": []`) {
		t.Fatalf("empty merge did not encode empty arrays: %s", readFileString(t, emptyPath))
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestWriteForPluginAtomicReplace(t *testing.T) {
	m, _ := newTestManager(t, nil)
	mustSave(t, m, model.RulePack{Name: "pack-a", Rules: []model.Rule{{From: "a", To: "A"}}})
	mustSave(t, m, model.RulePack{Name: "pack-b", Rules: []model.Rule{{From: "b", To: "B"}}})

	path, err := m.WriteForPlugin(context.Background(), "plug-1", []string{"pack-a"})
	if err != nil {
		t.Fatalf("WriteForPlugin: %v", err)
	}
	first := readFileString(t, path)

	// A failed merge must leave the previous document untouched.
	if _, err := m.WriteForPlugin(context.Background(), "plug-1", []string{"missing"}); !errors.Is(err, ErrPackNotFound) {
		t.Fatalf("WriteForPlugin(missing) = %v, want ErrPackNotFound", err)
	}
	if got := readFileString(t, path); got != first {
		t.Fatalf("failed write changed the file:\n%s", got)
	}

	// A successful rewrite replaces the content.
	if _, err := m.WriteForPlugin(context.Background(), "plug-1", []string{"pack-b"}); err != nil {
		t.Fatalf("second WriteForPlugin: %v", err)
	}
	second := readFileString(t, path)
	if second == first {
		t.Fatal("rewrite left the old content in place")
	}

	// No temp files may survive in the runtime directory.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "rules.json" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("runtime dir = %v, want only rules.json", names)
	}
}

func TestSavePackRejectsInvalid(t *testing.T) {
	m, _ := newTestManager(t, nil)

	cases := []struct {
		name string
		pack model.RulePack
	}{
		{"empty name", model.RulePack{Name: "", Rules: []model.Rule{{From: "a", To: "b"}}}},
		{"uppercase name", model.RulePack{Name: "BadPack", Rules: []model.Rule{{From: "a", To: "b"}}}},
		{"too short", model.RulePack{Name: "a", Rules: []model.Rule{{From: "a", To: "b"}}}},
		{"too long", model.RulePack{Name: strings.Repeat("a", 33), Rules: []model.Rule{{From: "a", To: "b"}}}},
		{"leading dash", model.RulePack{Name: "-pack", Rules: []model.Rule{{From: "a", To: "b"}}}},
		{"empty from", model.RulePack{Name: "pack-a", Rules: []model.Rule{{From: "", To: "b"}}}},
		{"blank to", model.RulePack{Name: "pack-a", Rules: []model.Rule{{From: "a", To: "   "}}}},
		{"bad regexp", model.RulePack{Name: "pack-a", Strip: []model.StripPattern{{Pattern: "("}}}},
		{"empty pattern", model.RulePack{Name: "pack-a", Strip: []model.StripPattern{{Pattern: "  "}}}},
	}
	for _, tc := range cases {
		err := m.SavePack(tc.pack)
		if !errors.Is(err, ErrInvalidPack) {
			t.Errorf("%s: SavePack = %v, want ErrInvalidPack", tc.name, err)
		}
	}

	packs, err := m.ListPacks()
	if err != nil {
		t.Fatalf("ListPacks: %v", err)
	}
	if len(packs) != 0 {
		t.Fatalf("invalid packs were stored: %+v", packs)
	}
}

func TestWriteForPluginReload(t *testing.T) {
	reloader := &recordingReloader{}
	m, _ := newTestManager(t, reloader)
	mustSave(t, m, model.RulePack{Name: "pack-a", Rules: []model.Rule{{From: "a", To: "A"}}})

	path, err := m.WriteForPlugin(context.Background(), "plug-1", []string{"pack-a"})
	if err != nil {
		t.Fatalf("WriteForPlugin: %v", err)
	}
	if got := reloader.called(); len(got) != 1 || got[0] != "plug-1" {
		t.Fatalf("reload calls = %v, want [plug-1]", got)
	}

	// Invalid plugin ids never touch the filesystem or the reloader.
	if _, err := m.WriteForPlugin(context.Background(), "Bad_ID", nil); !errors.Is(err, ErrInvalidPluginID) {
		t.Fatalf("WriteForPlugin(Bad_ID) = %v, want ErrInvalidPluginID", err)
	}
	if got := reloader.called(); len(got) != 1 {
		t.Fatalf("reload calls after invalid id = %v, want 1 call", got)
	}

	// Reload errors are reported, but the file stays in place for the retry.
	reloader.setErr(errors.New("reload failed"))
	path2, err := m.WriteForPlugin(context.Background(), "plug-2", []string{"pack-a"})
	if err == nil || !strings.Contains(err.Error(), "reload failed") {
		t.Fatalf("WriteForPlugin with failing reloader = %v, want reload error", err)
	}
	if path2 == "" {
		t.Fatal("path is empty after a reload error; file was written")
	}
	if _, statErr := os.Stat(path2); statErr != nil {
		t.Fatalf("rules file missing after reload error: %v", statErr)
	}
	_ = path
}

func TestWriteAllMultiplePlugins(t *testing.T) {
	reloader := &recordingReloader{}
	m, dir := newTestManager(t, reloader)
	mustSave(t, m, model.RulePack{Name: "pack-a", Rules: []model.Rule{{From: "a", To: "A"}}})
	mustSave(t, m, model.RulePack{Name: "pack-b", Rules: []model.Rule{{From: "b", To: "B"}}})

	err := m.WriteAll(context.Background(), map[string][]string{
		"plug-b": {"pack-a", "pack-b"},
		"Bad_Id": {"pack-a"},
		"plug-a": {"pack-a"},
	})
	if !errors.Is(err, ErrInvalidPluginID) {
		t.Fatalf("WriteAll = %v, want ErrInvalidPluginID", err)
	}

	for plugin, want := range map[string]string{
		"plug-a": "pack-a",
		"plug-b": "pack-a,pack-b",
	} {
		doc := readMerged(t, filepath.Join(dir, "runtime", plugin, "rules.json"))
		if got := strings.Join(doc.Packs, ","); got != want {
			t.Fatalf("%s packs = %q, want %q", plugin, got, want)
		}
	}
	if got := reloader.called(); strings.Join(got, ",") != "plug-a,plug-b" {
		t.Fatalf("reload order = %v, want [plug-a plug-b]", got)
	}

	// A fully valid call reports success and refreshes every plugin.
	if err := m.WriteAll(context.Background(), map[string][]string{"plug-a": {"pack-b"}}); err != nil {
		t.Fatalf("WriteAll(valid): %v", err)
	}
	doc := readMerged(t, filepath.Join(dir, "runtime", "plug-a", "rules.json"))
	if got := strings.Join(doc.Packs, ","); got != "pack-b" {
		t.Fatalf("plug-a packs after rewrite = %q, want pack-b", got)
	}
}

func TestBuiltinPackReturnsFreshCopy(t *testing.T) {
	first, ok := BuiltinPack(BuiltinWBPack)
	if !ok {
		t.Fatalf("BuiltinPack(%q) reported unknown", BuiltinWBPack)
	}
	if first.Name != BuiltinWBPack || len(first.Rules) == 0 || len(first.Strip) == 0 {
		t.Fatalf("builtin pack = %+v, want the shipped content", first)
	}

	// Mutating a copy must not leak into later copies or seeds.
	first.Description = "mutated"
	first.Rules[0].To = "mutated"
	first.Strip[0].Pattern = "mutated"
	first.Rules = append(first.Rules, model.Rule{From: "x", To: "y"})

	second, ok := BuiltinPack("  " + BuiltinWBPack + "  ")
	if !ok {
		t.Fatal("BuiltinPack did not trim whitespace around the name")
	}
	if second.Description == "mutated" {
		t.Error("description was shared between copies")
	}
	if want := builtinWBRules()[0].To; second.Rules[0].To != want {
		t.Errorf("rule copy = %q, want %q", second.Rules[0].To, want)
	}
	if want := builtinWBStrip()[0].Pattern; second.Strip[0].Pattern != want {
		t.Errorf("strip copy = %q, want %q", second.Strip[0].Pattern, want)
	}
	if len(second.Rules) != len(builtinWBRules()) {
		t.Errorf("rules copy has %d entries, want %d", len(second.Rules), len(builtinWBRules()))
	}

	// The copy must be a valid pack, so "reset" can simply save it.
	m, _ := newTestManager(t, nil)
	mustSave(t, m, second)

	for _, name := range []string{"", "unknown-pack", "WB-MODERATION"} {
		if _, ok := BuiltinPack(name); ok {
			t.Errorf("BuiltinPack(%q) reported a builtin", name)
		}
	}
}

func TestBuiltinStripRegexp(t *testing.T) {
	re := regexp.MustCompile(builtinWBStrip()[0].Pattern)

	cases := []struct{ in, want string }{
		{"keep cc_sig=abc123 bar", "keep bar"},
		{"header x-anthropic-beta=foo,next", "header next"},
		{`json {"cc_sig": "abc"}`, `json {"cc_sig": "abc"}`},
		{"use cc_flags for build", "use cc_flags for build"},
		{"nothing to strip here", "nothing to strip here"},
	}
	for _, tc := range cases {
		if got := re.ReplaceAllString(tc.in, ""); got != tc.want {
			t.Errorf("strip(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
