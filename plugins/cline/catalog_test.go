package main

// Catalog parsing and merge guards: which models are marked free, what a
// recommended list yields, and how models.dev metadata is folded in.

import (
	"testing"
)

func TestParseRecommendedModels(t *testing.T) {
	body := []byte(`{
		"free":[{"id":"cline-free/x","name":"X","description":"d"}],
		"recommended":[{"id":"anthropic/y","name":"Y"}],
		"clinePass":[{"id":"cline-pass/z","name":"Z"}]
	}`)
	got := parseRecommendedModels(body)
	if len(got.Free) != 1 || got.Free[0].ID != "cline-free/x" {
		t.Fatalf("free = %+v", got.Free)
	}
	if len(got.Recommended) != 1 || len(got.ClinePass) != 1 {
		t.Fatalf("recommended/clinePass = %+v/%+v", got.Recommended, got.ClinePass)
	}
	// A malformed envelope must yield empty lists, not a panic.
	empty := parseRecommendedModels([]byte(`{"free":"not a list"}`))
	if len(empty.Free) != 0 {
		t.Fatalf("malformed free = %+v", empty.Free)
	}
}

func TestIsFreeModelSignals(t *testing.T) {
	remote := map[string]bool{"remote/free-model": true}
	if !isFreeModel("remote/free-model", remote) {
		t.Fatal("a remote free id must count as free")
	}
	// The two naming conventions both mark a free model.
	if !isFreeModel("vendor/thing:free", map[string]bool{}) {
		t.Fatal(":free suffix not recognised")
	}
	if !isFreeModel("cline-free/thing", map[string]bool{}) {
		t.Fatal("cline-free/ prefix not recognised")
	}
	// A fallback-table entry stays free even when the remote list omits it.
	if !isFreeModel("cline-free/mimo-v2.6-flash", map[string]bool{}) {
		t.Fatal("fallback free model not recognised")
	}
	if isFreeModel("anthropic/claude-sonnet-5.5", map[string]bool{}) {
		t.Fatal("a paid model was marked free")
	}
}

func TestNameFromID(t *testing.T) {
	if got := nameFromID("cline-free/mimo-v2.6-flash"); got != "Mimo V2.6 Flash" {
		t.Fatalf("nameFromID = %q", got)
	}
	if got := nameFromID("stealth/space-bunny-alpha"); got != "Space Bunny Alpha" {
		t.Fatalf("nameFromID = %q", got)
	}
	// The :free suffix is stripped from the display name.
	if got := nameFromID("vendor/thing:free"); got != "Thing" {
		t.Fatalf("nameFromID = %q", got)
	}
}

func TestMergeModelsFreeFirstAndDropsUnreachableFallbacks(t *testing.T) {
	remote := &recommendedModels{
		Free:        []recommendedEntry{{ID: "cline-free/a", Name: "A"}},
		Recommended: []recommendedEntry{{ID: "anthropic/b", Name: "B"}},
	}
	dev := map[string]ModelInfo{"anthropic/b": {ID: "anthropic/b", ContextLength: 200000, SupportsImage: true}}
	models := mergeModels(remote, []string{"anthropic/b", "x/c"}, dev)
	if len(models) == 0 {
		t.Fatal("merge produced nothing")
	}
	if models[0].ID != "cline-free/a" || !models[0].Free {
		t.Fatalf("first model = %+v, want the free id first", models[0])
	}
	var found bool
	for _, m := range models {
		if m.ID == "anthropic/b" {
			found = true
			// models.dev fills the context window the Cline endpoints omit.
			if m.ContextLength != 200000 || !m.SupportsImage {
				t.Fatalf("metadata not merged: %+v", m)
			}
			if m.Free {
				t.Fatalf("a paid model was marked free: %+v", m)
			}
		}
	}
	// A fallback model the remote list does not mention must be dropped: offering
	// it would promise something the account cannot reach.
	for _, m := range models {
		if m.ID == "stealth/space-bunny-alpha" {
			t.Fatalf("unreachable fallback leaked into the catalog: %+v", m)
		}
	}
	if !found {
		t.Fatalf("authenticated model missing from %+v", models)
	}
}

func TestMergeModelsWithoutRemoteKeepsFallbacks(t *testing.T) {
	models := mergeModels(&recommendedModels{}, nil, nil)
	// Both remotes down: the measured fallback table must be served so the model
	// list is not empty.
	if len(models) != len(fallbackModels) {
		t.Fatalf("got %d models, want %d fallbacks", len(models), len(fallbackModels))
	}
	for _, m := range models {
		if !m.Free {
			t.Fatalf("fallback model not marked free: %+v", m)
		}
	}
}

func TestParseClineModelsDevReadsTopLevelAndProviders(t *testing.T) {
	// The block may sit at the root or under "providers"; both must parse.
	topLevel := []byte(`{"cline-pass":{"models":{"m1":{"name":"M1","limit":{"context":123000},"modalities":{"input":["text","image"]}}}}}`)
	got := parseClineModelsDev(topLevel)
	info, ok := got["cline-pass/m1"]
	if !ok {
		t.Fatalf("m1 missing: %v", got)
	}
	if info.ContextLength != 123000 || !info.SupportsImage {
		t.Fatalf("m1 = %+v", info)
	}
	nested := []byte(`{"providers":{"cline-pass":{"models":{"m2":{"name":"M2"}}}}}`)
	if _, ok := parseClineModelsDev(nested)["cline-pass/m2"]; !ok {
		t.Fatal("nested providers block not parsed")
	}
	if len(parseClineModelsDev([]byte(`{}`))) != 0 {
		t.Fatal("an absent block must yield an empty map")
	}
}

func TestParseModelIDs(t *testing.T) {
	body := []byte(`{"object":"list","data":[{"id":"a/b:free"},{"id":"  "},{"id":"c/d"}]}`)
	ids := parseModelIDs(body)
	if len(ids) != 2 || ids[0] != "a/b:free" || ids[1] != "c/d" {
		t.Fatalf("ids = %v", ids)
	}
}
