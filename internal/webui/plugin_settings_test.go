package webui

import (
	"fmt"
	"net/http"
	"testing"

	"aiworkgate/internal/model"
)

// pluginSettingsFixture returns Options whose Plugins list holds one "mock"
// plugin plus a tiny in-memory settings store, so the handler tests exercise
// the full round trip without the host.
func pluginSettingsFixture(saved *PluginSettingsUpdate, stored *map[string]any) Options {
	opts := Options{
		Plugins: func() []model.PluginStatus {
			return []model.PluginStatus{{Manifest: model.Manifest{ID: "mock", Name: "Mock"}}}
		},
	}
	opts.GetPluginSettings = func(id string) (any, error) {
		if id != "mock" {
			return nil, fmt.Errorf("%w: %s", ErrPluginNotFound, id)
		}
		payload := map[string]any{
			"plugin_id":    id,
			"enabled":      (*stored)["enabled"],
			"mode":         (*stored)["mode"],
			"concurrency":  (*stored)["concurrency"],
			"checkin_time": (*stored)["checkin_time"],
			"strict":       (*stored)["strict"],
		}
		return payload, nil
	}
	opts.SavePluginSettings = func(id string, update PluginSettingsUpdate) (any, error) {
		if id != "mock" {
			return nil, fmt.Errorf("%w: %s", ErrPluginNotFound, id)
		}
		*saved = update
		(*stored)["enabled"] = update.Enabled != nil && *update.Enabled
		(*stored)["mode"] = update.Mode
		(*stored)["concurrency"] = update.Concurrency
		(*stored)["checkin_time"] = update.CheckinTime
		(*stored)["strict"] = update.Strict
		return map[string]any{"ok": true, "plugin_id": id}, nil
	}
	return opts
}

func TestPluginSettingsMissingPluginIs404(t *testing.T) {
	saved := PluginSettingsUpdate{}
	stored := map[string]any{}
	handler := New(pluginSettingsFixture(&saved, &stored))

	recorder := doRequest(t, handler, http.MethodGet, "/api/plugins/ghost/settings", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("GET missing plugin = %d, want 404 (%s)", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Error struct{ Type string } `json:"error"`
	}
	decodeJSONBody(t, recorder, &payload)
	if payload.Error.Type != "not_found" {
		t.Fatalf("error type = %q, want not_found", payload.Error.Type)
	}

	recorder = doRequest(t, handler, http.MethodPut, "/api/plugins/ghost/settings", `{"mode":"always","concurrency":0,"strict":"default"}`)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("PUT missing plugin = %d, want 404 (%s)", recorder.Code, recorder.Body.String())
	}
}

func TestPluginSettingsValidationRejects(t *testing.T) {
	saved := PluginSettingsUpdate{}
	stored := map[string]any{}
	handler := New(pluginSettingsFixture(&saved, &stored))

	cases := []struct {
		name string
		body string
	}{
		{"bad mode", `{"mode":"sometimes","concurrency":2,"strict":"default"}`},
		{"negative concurrency", `{"mode":"always","concurrency":-1,"strict":"default"}`},
		{"over max concurrency", `{"mode":"always","concurrency":33,"strict":"default"}`},
		{"bad checkin", `{"mode":"always","concurrency":2,"checkin_time":"25:99","strict":"default"}`},
		{"bad strict", `{"mode":"always","concurrency":2,"strict":"maybe"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			saved = PluginSettingsUpdate{}
			recorder := doRequest(t, handler, http.MethodPut, "/api/plugins/mock/settings", tc.body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("PUT %s = %d, want 400 (%s)", tc.name, recorder.Code, recorder.Body.String())
			}
			var payload struct {
				Error struct{ Type string } `json:"error"`
			}
			decodeJSONBody(t, recorder, &payload)
			if payload.Error.Type != "bad_request" {
				t.Fatalf("error type = %q, want bad_request", payload.Error.Type)
			}
			if saved.Mode != "" || saved.Concurrency != 0 {
				t.Fatalf("callback ran for an invalid request: %+v", saved)
			}
		})
	}
}

func TestPluginSettingsPutThenGetRoundTrip(t *testing.T) {
	saved := PluginSettingsUpdate{}
	stored := map[string]any{"enabled": false, "mode": "", "concurrency": 0, "checkin_time": "", "strict": "default"}
	handler := New(pluginSettingsFixture(&saved, &stored))

	body := `{"enabled":true,"mode":"on_demand","concurrency":4,"checkin_time":"08:30","strict":"on"}`
	recorder := doRequest(t, handler, http.MethodPut, "/api/plugins/mock/settings", body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	if saved.Concurrency != 4 || saved.Mode != "on_demand" || saved.CheckinTime != "08:30" || saved.Strict != "on" {
		t.Fatalf("saved update = %+v", saved)
	}
	if saved.Enabled == nil || !*saved.Enabled {
		t.Fatalf("enabled not forwarded: %+v", saved.Enabled)
	}

	recorder = doRequest(t, handler, http.MethodGet, "/api/plugins/mock/settings", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	var got struct {
		Concurrency int    `json:"concurrency"`
		Mode        string `json:"mode"`
		CheckinTime string `json:"checkin_time"`
		Strict      string `json:"strict"`
	}
	decodeJSONBody(t, recorder, &got)
	if got.Concurrency != 4 || got.Mode != "on_demand" || got.CheckinTime != "08:30" || got.Strict != "on" {
		t.Fatalf("round-trip = %+v", got)
	}
}

func TestPluginSettingsNilCallbacksReturn501(t *testing.T) {
	handler := New(Options{
		Plugins: func() []model.PluginStatus {
			return []model.PluginStatus{{Manifest: model.Manifest{ID: "mock"}}}
		},
	})
	recorder := doRequest(t, handler, http.MethodGet, "/api/plugins/mock/settings", "")
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("GET without callbacks = %d, want 501 (%s)", recorder.Code, recorder.Body.String())
	}
}
