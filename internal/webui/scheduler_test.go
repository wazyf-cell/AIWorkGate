package webui

import (
	"net/http"
	"testing"
)

// TestSchedulerEndpointsUnavailable verifies every new route answers 501 when
// its host callback is missing.
func TestSchedulerEndpointsUnavailable(t *testing.T) {
	handler := New(Options{})
	cases := []struct {
		method string
		target string
		body   string
	}{
		{http.MethodGet, "/api/scheduler", ""},
		{http.MethodPut, "/api/scheduler/global", `{"inter_pool":"smart"}`},
		{http.MethodPut, "/api/scheduler/pools/mock", `{"strategy":"fixed"}`},
		{http.MethodPost, "/api/scheduler/presets/balanced", ""},
		{http.MethodGet, "/api/scheduler/models", ""},
		{http.MethodPut, "/api/scheduler/models/logical", `{"items":[]}`},
		{http.MethodDelete, "/api/scheduler/models/logical", ""},
		{http.MethodPost, "/api/scheduler/explain", `{"model":"x"}`},
		{http.MethodGet, "/api/scheduler/sticky", ""},
		{http.MethodDelete, "/api/scheduler/sticky", ""},
		{http.MethodGet, "/api/pools/mock", ""},
		{http.MethodPut, "/api/pools/mock", `{"hedge_ms":0}`},
		{http.MethodPut, "/api/pools/mock/members", `{"items":[]}`},
		{http.MethodPost, "/api/pools/mock/sync", ""},
		{http.MethodGet, "/api/pools/mock/catalog", ""},
	}
	for _, tc := range cases {
		recorder := doRequest(t, handler, tc.method, tc.target, tc.body)
		if recorder.Code != http.StatusNotImplemented {
			t.Fatalf("%s %s = %d, want 501 (%s)", tc.method, tc.target, recorder.Code, recorder.Body.String())
		}
	}
}

// schedulerOptions returns callbacks that record calls and answer {"ok":true}.
func schedulerOptions(calls *int) Options {
	payload := func() (any, error) {
		*calls++
		return map[string]any{"ok": true}, nil
	}
	return Options{
		SchedulerOverview: func() (any, error) { return payload() },
		SaveSchedulerGlobal: func(SchedulerGlobalUpdate) (any, error) {
			return payload()
		},
		SaveSchedulerPool: func(string, SchedulerPoolUpdate) (any, error) {
			return payload()
		},
		ApplySchedulerPreset: func(string) (any, error) { return payload() },
		SchedulerModels:      func() (any, error) { return payload() },
		SaveSchedulerModel: func(string, SchedulerModelUpdate) (any, error) {
			return payload()
		},
		DeleteSchedulerModel: func(string) error {
			*calls++
			return nil
		},
		SchedulerExplain: func(string, string, string) (any, error) { return payload() },
		SchedulerSticky:  func() (any, error) { return payload() },
		DeleteSchedulerSticky: func(string, string) (int64, error) {
			*calls++
			return 1, nil
		},
		PoolDetail: func(string) (any, error) { return payload() },
		SavePoolSettings: func(string, PoolSettingsUpdate) (any, error) {
			return payload()
		},
		SavePoolMembers: func(string, []string) (any, error) { return payload() },
		SyncPoolCatalog: func(string) (any, error) { return payload() },
		PoolCatalog:     func(string) (any, error) { return payload() },
	}
}

func TestSchedulerValidationRejectsBeforeCallback(t *testing.T) {
	calls := 0
	handler := New(schedulerOptions(&calls))

	cases := []struct {
		name   string
		method string
		target string
		body   string
		kind   string
	}{
		{"inter pool", http.MethodPut, "/api/scheduler/global", `{"inter_pool":"priority"}`, "invalid_inter_pool"},
		{"empty global", http.MethodPut, "/api/scheduler/global", `{}`, "bad_request"},
		{"defaults ttl", http.MethodPut, "/api/scheduler/global", `{"defaults":{"pool_sticky_ttl":86401}}`, "invalid_scheduler_defaults"},
		{"defaults concurrency", http.MethodPut, "/api/scheduler/global", `{"defaults":{"account_concurrency":33}}`, "invalid_scheduler_defaults"},
		{"defaults hedge", http.MethodPut, "/api/scheduler/global", `{"defaults":{"hedge_ms":900}}`, "invalid_scheduler_defaults"},
		{"defaults reasoning", http.MethodPut, "/api/scheduler/global", `{"defaults":{"default_reasoning":"max"}}`, "invalid_scheduler_defaults"},
		{"pool strategy", http.MethodPut, "/api/scheduler/pools/mock", `{"strategy":"smart"}`, "invalid_pool_settings"},
		{"pool ttl", http.MethodPut, "/api/scheduler/pools/mock", `{"conv_sticky_ttl":-1}`, "invalid_pool_settings"},
		{"pool concurrency", http.MethodPut, "/api/scheduler/pools/mock", `{"account_concurrency":40}`, "invalid_pool_settings"},
		{"pool hedge", http.MethodPut, "/api/scheduler/pools/mock", `{"hedge_ms":999}`, "invalid_pool_settings"},
		{"pool reasoning", http.MethodPut, "/api/scheduler/pools/mock", `{"default_reasoning":"ultra"}`, "invalid_pool_settings"},
		{"pool sync time", http.MethodPut, "/api/scheduler/pools/mock", `{"model_sync_time":"25:99"}`, "invalid_pool_settings"},
		{"preset", http.MethodPost, "/api/scheduler/presets/chaos", "", "invalid_preset"},
		{"model duplicate", http.MethodPut, "/api/scheduler/models/l",
			`{"items":[{"plugin_id":"p","model":"m"},{"plugin_id":"p","model":"m"}]}`, "invalid_model_binding"},
		{"model empty source", http.MethodPut, "/api/scheduler/models/l",
			`{"items":[{"plugin_id":"p","model":""}]}`, "invalid_model_binding"},
		{"members empty id", http.MethodPut, "/api/pools/mock/members", `{"items":[""]}`, "bad_request"},
		{"sync time", http.MethodPut, "/api/pools/mock", `{"model_sync_time":"25:99"}`, "invalid_pool_settings"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := calls
			recorder := doRequest(t, handler, tc.method, tc.target, tc.body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", recorder.Code, recorder.Body.String())
			}
			var payload struct {
				Error struct{ Type string } `json:"error"`
			}
			decodeJSONBody(t, recorder, &payload)
			if payload.Error.Type != tc.kind {
				t.Fatalf("error type = %q, want %q", payload.Error.Type, tc.kind)
			}
			if calls != before {
				t.Fatalf("callback ran for an invalid request")
			}
		})
	}
}

func TestSchedulerSuccessPayloads(t *testing.T) {
	var (
		calls     int
		global    SchedulerGlobalUpdate
		pool      SchedulerPoolUpdate
		preset    string
		modelName string
		modelBody SchedulerModelUpdate
		explain   [3]string
		sticky    [2]string
		members   []string
	)
	handler := New(Options{
		SchedulerOverview: func() (any, error) {
			return map[string]any{"version": 3}, nil
		},
		SaveSchedulerGlobal: func(update SchedulerGlobalUpdate) (any, error) {
			global = update
			return map[string]any{"ok": true}, nil
		},
		SaveSchedulerPool: func(pluginID string, update SchedulerPoolUpdate) (any, error) {
			if pluginID != "mock" {
				t.Fatalf("plugin = %q", pluginID)
			}
			pool = update
			return map[string]any{"ok": true}, nil
		},
		ApplySchedulerPreset: func(name string) (any, error) {
			preset = name
			return map[string]any{"ok": true}, nil
		},
		SchedulerModels: func() (any, error) {
			return map[string]any{"models": []any{}}, nil
		},
		SaveSchedulerModel: func(logical string, update SchedulerModelUpdate) (any, error) {
			modelName, modelBody = logical, update
			return map[string]any{"ok": true}, nil
		},
		DeleteSchedulerModel: func(string) error { calls++; return nil },
		SchedulerExplain: func(model, conv, client string) (any, error) {
			explain = [3]string{model, conv, client}
			return map[string]any{"decision": map[string]any{"account_id": "a"}}, nil
		},
		SchedulerSticky: func() (any, error) {
			return map[string]any{"bindings": []any{}, "count": 0}, nil
		},
		DeleteSchedulerSticky: func(scope, key string) (int64, error) {
			sticky = [2]string{scope, key}
			return 2, nil
		},
		PoolDetail: func(string) (any, error) {
			return map[string]any{"plugin_id": "mock"}, nil
		},
		SavePoolSettings: func(string, PoolSettingsUpdate) (any, error) {
			return map[string]any{"ok": true}, nil
		},
		SavePoolMembers: func(_ string, items []string) (any, error) {
			members = items
			return map[string]any{"ok": true}, nil
		},
		SyncPoolCatalog: func(string) (any, error) {
			return map[string]any{"ok": true, "synced": 2}, nil
		},
		PoolCatalog: func(string) (any, error) {
			return map[string]any{"catalog": []any{}}, nil
		},
	})

	recorder := doRequest(t, handler, http.MethodGet, "/api/scheduler", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("overview = %d", recorder.Code)
	}
	var overview struct{ Version int }
	decodeJSONBody(t, recorder, &overview)
	if overview.Version != 3 {
		t.Fatalf("version = %d", overview.Version)
	}

	recorder = doRequest(t, handler, http.MethodPut, "/api/scheduler/global",
		`{"inter_pool":"FIXED","defaults":{"strategy":"weighted3","pool_sticky_ttl":600}}`)
	if recorder.Code != http.StatusOK || global.InterPool == nil || *global.InterPool != "fixed" {
		t.Fatalf("global = %+v (%d)", global, recorder.Code)
	}
	if global.Defaults == nil || global.Defaults.Strategy == nil || *global.Defaults.Strategy != "weighted3" {
		t.Fatalf("defaults = %+v", global.Defaults)
	}

	recorder = doRequest(t, handler, http.MethodPut, "/api/scheduler/pools/mock",
		`{"upstream_enabled":false,"strategy":"p2c","account_concurrency":2,"hedge_ms":3000,"default_reasoning":"high"}`)
	if recorder.Code != http.StatusOK || pool.Strategy == nil || *pool.Strategy != "p2c" {
		t.Fatalf("pool = %+v (%d)", pool, recorder.Code)
	}
	if pool.UpstreamEnabled == nil || *pool.UpstreamEnabled {
		t.Fatalf("upstream = %v", pool.UpstreamEnabled)
	}

	recorder = doRequest(t, handler, http.MethodPost, "/api/scheduler/presets/latency", "")
	if recorder.Code != http.StatusOK || preset != "latency" {
		t.Fatalf("preset = %q (%d)", preset, recorder.Code)
	}

	recorder = doRequest(t, handler, http.MethodPut, "/api/scheduler/models/deepseek",
		`{"display_name":"DeepSeek","items":[{"plugin_id":"mock","model":"glm"}]}`)
	if recorder.Code != http.StatusOK || modelName != "deepseek" || len(modelBody.Items) != 1 ||
		modelBody.Items[0].PluginID != "mock" || modelBody.Items[0].Model != "glm" {
		t.Fatalf("model = %q %+v (%d)", modelName, modelBody, recorder.Code)
	}

	recorder = doRequest(t, handler, http.MethodDelete, "/api/scheduler/models/deepseek", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("delete model = %d", recorder.Code)
	}

	recorder = doRequest(t, handler, http.MethodPost, "/api/scheduler/explain",
		`{"model":"deepseek","conversation_id":"c1","client_key":"ck"}`)
	if recorder.Code != http.StatusOK || explain != [3]string{"deepseek", "c1", "ck"} {
		t.Fatalf("explain = %+v (%d)", explain, recorder.Code)
	}

	recorder = doRequest(t, handler, http.MethodDelete, "/api/scheduler/sticky?scope=conv&key=c1", "")
	if recorder.Code != http.StatusOK || sticky != [2]string{"conv", "c1"} {
		t.Fatalf("sticky = %+v (%d)", sticky, recorder.Code)
	}
	var deleted struct{ Removed int64 }
	decodeJSONBody(t, recorder, &deleted)
	if deleted.Removed != 2 {
		t.Fatalf("removed = %d", deleted.Removed)
	}

	recorder = doRequest(t, handler, http.MethodDelete, "/api/scheduler/sticky?key=c1", "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("key without scope = %d, want 400", recorder.Code)
	}

	recorder = doRequest(t, handler, http.MethodPut, "/api/pools/mock/members",
		`{"items":["acc_1","acc_2","acc_1"," "]} `)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("blank member accepted (%d)", recorder.Code)
	}
	recorder = doRequest(t, handler, http.MethodPut, "/api/pools/mock/members", `{"items":["acc_1","acc_2","acc_1"]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("members = %d", recorder.Code)
	}
	if len(members) != 2 || members[0] != "acc_1" || members[1] != "acc_2" {
		t.Fatalf("members = %+v, want deduplicated order", members)
	}

	recorder = doRequest(t, handler, http.MethodGet, "/api/pools/mock", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("pool detail = %d", recorder.Code)
	}
	recorder = doRequest(t, handler, http.MethodPost, "/api/pools/mock/sync", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("sync = %d", recorder.Code)
	}
	recorder = doRequest(t, handler, http.MethodGet, "/api/pools/mock/catalog", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("catalog = %d", recorder.Code)
	}
}

func TestSchedulerErrorMapping(t *testing.T) {
	handler := New(Options{
		SaveSchedulerPool: func(string, SchedulerPoolUpdate) (any, error) {
			return nil, ErrPluginNotFound
		},
		SchedulerExplain: func(string, string, string) (any, error) {
			return nil, ErrUnknownModel
		},
	})
	recorder := doRequest(t, handler, http.MethodPut, "/api/scheduler/pools/ghost", `{}`)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("pool = %d, want 404", recorder.Code)
	}
	recorder = doRequest(t, handler, http.MethodPost, "/api/scheduler/explain", `{"model":"nope"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("explain = %d, want 400", recorder.Code)
	}
	var payload struct {
		Error struct{ Type string } `json:"error"`
	}
	decodeJSONBody(t, recorder, &payload)
	if payload.Error.Type != "unknown_model" {
		t.Fatalf("type = %q", payload.Error.Type)
	}
}
