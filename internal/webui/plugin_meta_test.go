package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"aiworkgate/internal/model"
)

// pluginMetaFixture wires one running "mock", one stopped "idle" and a nil
// PluginAction: if GET /api/plugins/mock/meta ever fell through to the
// {action} handler the reply would be 501 instead of the plugin payload.
func pluginMetaFixture(callErr error) Options {
	return Options{
		Plugins: func() []model.PluginStatus {
			return []model.PluginStatus{
				{Manifest: model.Manifest{ID: "mock"}, Running: true, Healthy: true},
				{Manifest: model.Manifest{ID: "idle"}},
			}
		},
		PluginMeta: func(id string) (any, error) {
			if callErr != nil {
				return nil, callErr
			}
			return map[string]any{"id": id, "uptime_s": 42, "plugin_field": "verbatim"}, nil
		},
	}
}

func pluginMetaErrorType(t *testing.T, body []byte) string {
	t.Helper()
	var payload struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}
	return payload.Error.Type
}

func TestPluginMetaReturnsPluginPayload(t *testing.T) {
	handler := New(pluginMetaFixture(nil))
	recorder := doRequest(t, handler, http.MethodGet, "/api/plugins/mock/meta", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET meta = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	var payload map[string]any
	decodeJSONBody(t, recorder, &payload)
	if payload["id"] != "mock" || payload["plugin_field"] != "verbatim" || payload["uptime_s"] != float64(42) {
		t.Fatalf("payload = %v, want the plugin's own JSON", payload)
	}
}

func TestPluginMetaMethodsAndStatuses(t *testing.T) {
	handler := New(pluginMetaFixture(nil))

	recorder := doRequest(t, handler, http.MethodPost, "/api/plugins/mock/meta", "")
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST meta = %d, want 405 (body %s)", recorder.Code, recorder.Body.String())
	}

	recorder = doRequest(t, handler, http.MethodGet, "/api/plugins/ghost/meta", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown plugin = %d, want 404 (body %s)", recorder.Code, recorder.Body.String())
	}
	if kind := pluginMetaErrorType(t, recorder.Body.Bytes()); kind != "plugin_not_found" {
		t.Fatalf("error type = %q, want plugin_not_found", kind)
	}

	recorder = doRequest(t, handler, http.MethodGet, "/api/plugins/idle/meta", "")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("stopped plugin = %d, want 503 (body %s)", recorder.Code, recorder.Body.String())
	}
	if kind := pluginMetaErrorType(t, recorder.Body.Bytes()); kind != "plugin_not_running" {
		t.Fatalf("error type = %q, want plugin_not_running", kind)
	}
}

func TestPluginMetaCallbackFailure(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		want     int
		wantKind string
	}{
		{"plugin refuses", errors.New("plugins: mock /meta: http 500: boom"), http.StatusBadGateway, "plugin_meta_failed"},
		{"plugin never answers", fmt.Errorf("plugins: mock /meta: %w", context.DeadlineExceeded), http.StatusGatewayTimeout, "plugin_meta_failed"},
		{"plugin stopped meanwhile", fmt.Errorf("%w: mock", ErrPluginNotRunning), http.StatusServiceUnavailable, "plugin_not_running"},
		{"plugin uninstalled meanwhile", fmt.Errorf("%w: mock", ErrPluginNotFound), http.StatusNotFound, "plugin_not_found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := New(pluginMetaFixture(tc.err))
			recorder := doRequest(t, handler, http.MethodGet, "/api/plugins/mock/meta", "")
			if recorder.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", recorder.Code, tc.want, recorder.Body.String())
			}
			if kind := pluginMetaErrorType(t, recorder.Body.Bytes()); kind != tc.wantKind {
				t.Fatalf("error type = %q, want %q", kind, tc.wantKind)
			}
		})
	}
}

func TestPluginMetaNilCallbackIs501(t *testing.T) {
	handler := New(Options{
		Plugins: func() []model.PluginStatus {
			return []model.PluginStatus{{Manifest: model.Manifest{ID: "mock"}, Running: true}}
		},
	})
	recorder := doRequest(t, handler, http.MethodGet, "/api/plugins/mock/meta", "")
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("nil callback = %d, want 501 (body %s)", recorder.Code, recorder.Body.String())
	}
}
