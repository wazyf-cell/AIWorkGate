package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestModelsLiveAPIFiltersDisabledAndNonText(t *testing.T) {
	s := defaultSession("maas.example.test")
	root := makeInstall(t, s)
	local := newFakeLocal(t)
	local.models = `{"success":true,"list":[
	  {"id":"model_config:acct:glm-5.3","model":"glm-5.3","displayName":"GLM 5.3","enabled":true,"toolCallingSupported":true,"contextWindow":128000,"baseUrl":"https://modelgw.example.com/v2","capabilities":{"vision":true}},
	  {"id":"model_config:acct:deepseek-v4-pro","model":"deepseek-v4-pro","displayName":"DeepSeek V4 Pro","enabled":true,"toolCallingSupported":false},
	  {"id":"model_config:acct:old-model","model":"old-model","enabled":false},
	  {"id":"model_config:acct:text-embedding-3","model":"text-embedding-3","enabled":true}
	]}`
	p, h, _ := newTestPlugin(t, func(o *Options) {
		o.ForcedOfficeAceHome = installHome(t, root)
		o.LocalAPIURL = local.URL()
	})

	rec := doReq(t, h, http.MethodGet, "/v1/models", reqOpts{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	resp := decodeAs[modelsResp](t, rec)
	if resp.Source != "merged" {
		t.Fatalf("source = %q", resp.Source)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("models = %+v", resp.Data)
	}
	if resp.Data[0].ID != "deepseek-v4-pro" || resp.Data[1].ID != "glm-5.3" {
		t.Fatalf("models = %+v", resp.Data)
	}
	glm := resp.Data[1]
	if glm.Tools == nil || !*glm.Tools || glm.Vision == nil || !*glm.Vision || glm.ContextLength != 128000 {
		t.Fatalf("glm = %+v", glm)
	}
	if resp.Data[0].Tools == nil || *resp.Data[0].Tools {
		t.Fatalf("deepseek tools = %+v", resp.Data[0].Tools)
	}
	if resp.Data[0].Vision != nil {
		t.Fatalf("vision must stay unknown, not invented: %+v", resp.Data[0].Vision)
	}
	if _, err := os.Stat(filepath.Join(p.DataDir, modelsCacheFileName)); err != nil {
		t.Fatalf("models cache not written: %v", err)
	}
}

func TestModelsFallbackToUserModelJSON(t *testing.T) {
	s := defaultSession("maas.example.test")
	root := makeInstall(t, s)
	claw := t.TempDir()
	userDir := filepath.Join(claw, "users", strings.ReplaceAll(s.userKey(), ":", "%3A"))
	writeTestFile(t, filepath.Join(userDir, "model.json"), `{"huawei-maas":[
	  {"id":"glm-5.3","baseUrl":"https://modelgw.example.com/v2"},
	  {"id":"kimi-k3-1019","baseUrl":"https://modelgw.example.com/v2"},
	  {"id":"text-embedding-3"}
	]}`)
	_, h, _ := newTestPlugin(t, func(o *Options) {
		o.ForcedOfficeAceHome = installHome(t, root)
		o.UserClawHome = claw
	})

	rec := doReq(t, h, http.MethodGet, "/v1/models", reqOpts{})
	resp := decodeAs[modelsResp](t, rec)
	if resp.Source != "model_json" {
		t.Fatalf("source = %q body %s", resp.Source, rec.Body.String())
	}
	if len(resp.Data) != 2 || resp.Data[0].ID != "glm-5.3" || resp.Data[1].ID != "kimi-k3-1019" {
		t.Fatalf("models = %+v", resp.Data)
	}
}

func TestModelsServesCacheWhenEverythingIsDown(t *testing.T) {
	s := defaultSession("maas.example.test")
	root := makeInstall(t, s)
	dataDir := t.TempDir()
	cache := modelCache{
		FetchedAt: fixedNow.Add(-7 * time.Hour).Unix(),
		Source:    "local_api",
		Models:    []Model{{ID: "cached-model", Name: "Cached"}, {ID: "other-model"}},
	}
	b, _ := json.Marshal(cache)
	writeTestFile(t, filepath.Join(dataDir, modelsCacheFileName), string(b))

	_, h, log := newTestPlugin(t, func(o *Options) {
		o.DataDir = dataDir
		o.ForcedOfficeAceHome = installHome(t, root)
	})
	rec := doReq(t, h, http.MethodGet, "/v1/models", reqOpts{})
	resp := decodeAs[modelsResp](t, rec)
	if resp.Source != "cache" || len(resp.Data) != 2 || resp.Data[0].ID != "cached-model" {
		t.Fatalf("resp = %+v body %s", resp, rec.Body.String())
	}
	if !strings.Contains(log.String(), "serving stale model cache") {
		t.Fatalf("missing stale cache log: %s", log.String())
	}
}

func TestModelsMultiplierFromTokenRates(t *testing.T) {
	s := defaultSession("maas.example.test")
	root := makeInstall(t, s)
	local := newFakeLocal(t)
	local.models = `{"success":true,"list":[
	  {"id":"model_config:acct:glm-5.2","model":"glm-5.2","displayName":"GLM-5.2","enabled":true},
	  {"id":"model_config:acct:no-rate","model":"no-rate","enabled":true}
	]}`
	local.rates = `{"models":[{"modelName":"glm-5.2","costCoefficient":0.7}]}`
	_, h, _ := newTestPlugin(t, func(o *Options) {
		o.ForcedOfficeAceHome = installHome(t, root)
		o.LocalAPIURL = local.URL()
	})
	rec := doReq(t, h, http.MethodGet, "/v1/models", reqOpts{})
	resp := decodeAs[modelsResp](t, rec)
	if len(resp.Data) != 2 {
		t.Fatalf("models = %+v", resp.Data)
	}
	if resp.Data[0].ID != "glm-5.2" || resp.Data[0].Multiplier != 0.7 {
		t.Fatalf("glm = %+v", resp.Data[0])
	}
	if resp.Data[1].ID != "no-rate" || resp.Data[1].Multiplier != 0 {
		t.Fatalf("no-rate = %+v", resp.Data[1])
	}
}

func TestModelsReadsRealThinkingTiersFromAPI(t *testing.T) {
	s := defaultSession("maas.example.test")
	root := makeInstall(t, s)
	local := newFakeLocal(t)
	local.models = `{"success":true,"list":[
	  {"id":"model_config:acct:glm-5.3","model":"glm-5.3","displayName":"GLM-5.3","enabled":true,
	   "thinking_config":{"disabled":{},"enabled":{"efforts":{"high":{},"max":{},"low":{}}}},
	   "priceFactor":0,"isFree":true,"modelSeries":"GLM"}
	]}`
	_, h, _ := newTestPlugin(t, func(o *Options) {
		o.ForcedOfficeAceHome = installHome(t, root)
		o.LocalAPIURL = local.URL()
	})
	rec := doReq(t, h, http.MethodGet, "/v1/models", reqOpts{})
	resp := decodeAs[modelsResp](t, rec)
	if len(resp.Data) != 1 {
		t.Fatalf("models = %+v", resp.Data)
	}
	m := resp.Data[0]
	want := []string{"off", "low", "high", "max"}
	if len(m.Thinking) != len(want) {
		t.Fatalf("thinking tiers = %v, want %v", m.Thinking, want)
	}
	for i, tier := range want {
		if m.Thinking[i] != tier {
			t.Fatalf("thinking tiers = %v, want %v", m.Thinking, want)
		}
	}
	if !m.Free || m.Vendor != "GLM" {
		t.Fatalf("free/vendor = %+v", m)
	}
}

func TestModelsProfileFallback(t *testing.T) {
	s := defaultSession("maas.example.test")
	root := makeInstall(t, s)
	local := newFakeLocal(t)
	local.models = `{"success":true,"models":[]}`
	local.profiles = `{"maasApiKey":"k","providers":[{"id":"acct","displayName":"Huawei MaaS","protocol":"huawei_maas","models":["kimi-k3-1019","text-embedding-3"],"modelBaseUrls":{"kimi-k3-1019":"https://modelgw.example.com/v2"},"modelDisplayNames":{"kimi-k3-1019":"Kimi K3"}}]}`
	_, h, _ := newTestPlugin(t, func(o *Options) {
		o.ForcedOfficeAceHome = installHome(t, root)
		o.LocalAPIURL = local.URL()
	})
	rec := doReq(t, h, http.MethodGet, "/v1/models", reqOpts{})
	resp := decodeAs[modelsResp](t, rec)
	if resp.Source != "local_profiles" || len(resp.Data) != 1 {
		t.Fatalf("resp = %+v body %s", resp, rec.Body.String())
	}
	if resp.Data[0].ID != "kimi-k3-1019" || resp.Data[0].Name != "Kimi K3" {
		t.Fatalf("model = %+v", resp.Data[0])
	}
}

func TestModelsEmptyWhenNothingAvailable(t *testing.T) {
	_, h, _ := newTestPlugin(t, nil)
	rec := doReq(t, h, http.MethodGet, "/v1/models", reqOpts{})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body %s", rec.Code, rec.Body.String())
	}
	resp := decodeAs[modelsResp](t, rec)
	if len(resp.Data) != 0 {
		t.Fatalf("models = %+v", resp.Data)
	}
	var raw map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	if _, ok := raw["data"]; ok {
		t.Fatalf("unavailable catalog must not return a data list: %s", rec.Body.String())
	}
	if env := decodeAs[errorEnvelope](t, rec); env.Error.Code != "models_unavailable" {
		t.Fatalf("error = %+v body %s", env.Error, rec.Body.String())
	}
}

// TestHandleModelsErrorResponses pins the contract that only a non-empty
// catalog may answer 200; a failed refresh with nothing cached must be a 503
// so the host never caches an empty "known" model list.
func TestHandleModelsErrorResponses(t *testing.T) {
	cached := []Model{{ID: "cached-model", Name: "Cached"}, {ID: "other-model"}}
	cases := []struct {
		name      string
		models    []Model
		source    string
		err       error
		wantCode  int
		wantData  int
		wantError bool
	}{
		{
			name:      "error with empty catalog",
			err:       errors.New("模型目录不可用：本机模型列表为空"),
			wantCode:  http.StatusServiceUnavailable,
			wantError: true,
		},
		{
			name:      "error with stale cache",
			models:    cached,
			source:    "cache",
			err:       errors.New("本机模型列表为空"),
			wantCode:  http.StatusOK,
			wantData:  2,
			wantError: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, h, _ := newTestPlugin(t, nil)
			orig := modelsSnapshotFunc
			modelsSnapshotFunc = func(*Plugin, context.Context, bool) ([]Model, string, error) {
				return tc.models, tc.source, tc.err
			}
			t.Cleanup(func() { modelsSnapshotFunc = orig })

			rec := doReq(t, h, http.MethodGet, "/v1/models", reqOpts{})
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d, body %s", rec.Code, tc.wantCode, rec.Body.String())
			}
			var raw map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
				t.Fatalf("decode %q: %v", rec.Body.String(), err)
			}
			data, hasData := raw["data"].([]any)
			if tc.wantError {
				if hasData {
					t.Fatalf("error response must not carry a data list: %s", rec.Body.String())
				}
				if env := decodeAs[errorEnvelope](t, rec); env.Error.Code != "models_unavailable" || env.Error.Message == "" {
					t.Fatalf("error = %+v body %s", env.Error, rec.Body.String())
				}
				return
			}
			if !hasData || len(data) != tc.wantData {
				t.Fatalf("data = %v, want %d entries: %s", raw["data"], tc.wantData, rec.Body.String())
			}
			resp := decodeAs[modelsResp](t, rec)
			if resp.Source != "cache" || resp.Data[0].ID != "cached-model" {
				t.Fatalf("resp = %+v", resp)
			}
			warn, _ := raw["officeace_error"].(string)
			if warn != tc.err.Error() {
				t.Fatalf("officeace_error = %q, want %q", warn, tc.err.Error())
			}
		})
	}
}
