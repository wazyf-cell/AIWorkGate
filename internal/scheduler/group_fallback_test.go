package scheduler

// D-50 备用链接组（group/<id> fallback）与 D-30② group_only 第三态单测。

import (
	"context"
	"strings"
	"testing"

	"aiworkgate/internal/model"
)

func groupBinding(snap *Snapshot, name string, items []Source, weights map[string]int) {
	snap.Bindings[name] = Binding{Logical: name, Display: name, Items: items, Weights: weights}
}

func TestExpandGroupItems(t *testing.T) {
	snap := snapOf(pool("p"), pool("q"))
	groupBinding(snap, "g2", []Source{{PluginID: "p", Model: "m1"}}, nil)
	groupBinding(snap, "g3", []Source{{PluginID: "q", Model: "m2"}}, nil)

	// direct member, group member, nested group, empty fields
	items := []Source{
		{PluginID: "p", Model: "m2"},
		{PluginID: groupPlugin, Model: "g2"},
		{PluginID: groupPlugin, Model: "g3"},
		{PluginID: "", Model: "x"},
	}
	got, notes := expandGroupItems(snap, items)
	if len(got) != 3 {
		t.Fatalf("expanded = %+v, want 3 sources", got)
	}
	if got[0] != (Source{PluginID: "p", Model: "m2"}) ||
		got[1] != (Source{PluginID: "p", Model: "m1"}) ||
		got[2] != (Source{PluginID: "q", Model: "m2"}) {
		t.Fatalf("expanded = %+v", got)
	}
	if len(notes) != 2 || !strings.Contains(notes[0], "g2") {
		t.Fatalf("notes = %v", notes)
	}

	// a group that expands to nothing is noted
	if _, n := expandGroupItems(snap, []Source{{PluginID: groupPlugin, Model: "missing"}}); len(n) != 1 || !strings.Contains(n[0], "无可展开成员") {
		t.Fatalf("missing group notes = %v", n)
	}
}

func TestExpandGroupItemsNoCycle(t *testing.T) {
	snap := snapOf(pool("p"))
	// a → b → a
	groupBinding(snap, "a", []Source{{PluginID: groupPlugin, Model: "b"}}, nil)
	groupBinding(snap, "b", []Source{{PluginID: groupPlugin, Model: "a"}, {PluginID: "p", Model: "m1"}}, nil)
	got, _ := expandGroupItems(snap, []Source{{PluginID: groupPlugin, Model: "a"}})
	if len(got) != 1 || got[0] != (Source{PluginID: "p", Model: "m1"}) {
		t.Fatalf("cycle expansion = %+v, want just p/m1", got)
	}
}

func TestGroupOnlyNotDirectlyRoutable(t *testing.T) {
	p := pool("p", account("a"))
	p.GroupOnly = true
	snap := snapOf(p)
	if _, _, err := resolveModel(snap, "p/m1"); err == nil ||
		!strings.Contains(err.Error(), "group_only") {
		t.Fatalf("direct group_only route err = %v, want group_only rejection", err)
	}
}

func TestGroupOnlyMemberSchedules(t *testing.T) {
	p := pool("p", account("a"))
	p.GroupOnly = true
	snap := snapOf(p)
	groupBinding(snap, "g", []Source{{PluginID: groupPlugin, Model: "inner"}}, nil)
	groupBinding(snap, "inner", []Source{{PluginID: "p", Model: "m1"}}, nil)
	s := New(Options{})
	dec := pick(t, s, snap, "g")
	if dec.PluginID != "p" || dec.Model != "m1" {
		t.Fatalf("decision = %+v, want p/m1 via the group", dec)
	}
}

func TestExplainGroupFallback(t *testing.T) {
	p := pool("p", account("a"))
	snap := snapOf(p)
	groupBinding(snap, "g", []Source{{PluginID: groupPlugin, Model: "inner"}}, nil)
	groupBinding(snap, "inner", []Source{{PluginID: "p", Model: "m1"}}, nil)
	s := New(Options{})
	expl, err := s.Explain(context.Background(), snap, Request{Model: "g"})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(expl.GroupFallbacks) == 0 {
		t.Fatalf("GroupFallbacks empty, want the group/<id> note")
	}
	if !strings.Contains(expl.GroupFallbacks[0], "group/inner") {
		t.Fatalf("GroupFallbacks = %v", expl.GroupFallbacks)
	}
}

// 兼容性确认：group_only 池仍可作为普通组员被 binding 直接列出。
func TestGroupOnlyListedMember(t *testing.T) {
	p := pool("p", account("a"))
	p.GroupOnly = true
	snap := snapOf(p)
	groupBinding(snap, "g", []Source{{PluginID: "p", Model: "m1"}}, nil)
	s := New(Options{})
	if _, err := s.Pick(context.Background(), snap, Request{Model: "g"}); err != nil {
		t.Fatalf("Pick via binding: %v", err)
	}
	_ = model.ModelInfo{}
}
