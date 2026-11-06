package app

import (
	"encoding/json"
	"errors"
	"testing"

	"aiworkgate/internal/config"
	"aiworkgate/internal/webui"
)

func newRoutingGroupsApp(t *testing.T) (*App, string) {
	t.Helper()
	_, st := newSchedulerApp(t)
	dir := t.TempDir()
	app := &App{store: st, dataDir: dir, cfg: config.Default()}
	return app, dir
}

func TestSaveRoutingGroupsValidates(t *testing.T) {
	app, _ := newRoutingGroupsApp(t)
	cases := []string{
		`[{"id":"","match":["*"]}]`,
		`[{"id":"a/b","match":["*"]}]`,
		`[{"id":"g","match":["*"]},{"id":"g","match":["*"]}]`,
		`[{"id":"g","match":[]}]`,
		`[{"id":"g","match":["re:["]}]`,
		`[{"id":"g","match":["*"],"rules":[{"match":["*"],"when":{"min_tokens":-1}}]}]`,
		`[{"id":"g","match":["*"],"intent":{"enabled":true,"labels":["a"],"route":{"a":"missing"}}}]`,
		`[{"id":"g","match":["*"],"intent":{"enabled":true,"labels":["a"],"route":{"b":"g"}}}]`,
		`[{"id":"g","match":["*"],"intent":{"enabled":true,"labels":["a"],"route":{"a":"g"},"default":"missing"}}]`,
	}
	for _, raw := range cases {
		if _, err := app.saveRoutingGroups(json.RawMessage(raw)); !errors.Is(err, webui.ErrInvalidRequest) {
			t.Fatalf("saveRoutingGroups(%s) error = %v, want ErrInvalidRequest", raw, err)
		}
	}
}

func TestSaveRoutingGroupsPersists(t *testing.T) {
	app, dir := newRoutingGroupsApp(t)
	payload, err := app.saveRoutingGroups(json.RawMessage(`[
		{"id":"smart","display":"智能","match":["*"]},
		{"id":"strong","match":["vendor/pro"],"intent":{
			"enabled":true,"labels":["simple","hard"],"route":{"hard":"strong"},"default":"smart"
		}}
	]`))
	if err != nil {
		t.Fatalf("saveRoutingGroups: %v", err)
	}
	view := payload.(map[string]any)
	if groups, _ := view["groups"].([]routingGroupView); len(groups) != 2 {
		t.Fatalf("view groups = %+v", view["groups"])
	}
	loaded, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if len(loaded.RoutingGroups) != 2 || loaded.RoutingGroups[1].Intent == nil {
		t.Fatalf("persisted groups = %+v", loaded.RoutingGroups)
	}
	if loaded.RoutingGroups[1].Intent.Route["hard"] != "strong" {
		t.Fatalf("intent route = %+v", loaded.RoutingGroups[1].Intent.Route)
	}
	if got := loaded.RoutingGroups[1].Intent.ProbeChars; got != 2000 {
		t.Fatalf("probe_chars = %d, want 2000", got)
	}
}
