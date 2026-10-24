package webui

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"aiworkgate/internal/store"
)

// customModelFixture is the host side of the custom-model editor.
type customModelFixture struct {
	models      []store.CustomModel
	created     []CustomModelInput
	updated     []customModelUpdate
	deleted     []string
	conflict    bool
	testLatency int64
	testErr     error
}

type customModelUpdate struct {
	id    string
	patch CustomModelPatch
}

func (f *customModelFixture) options() Options {
	return Options{
		CustomModels: func() ([]store.CustomModel, error) { return f.models, nil },
		CreateCustomModel: func(input CustomModelInput) (store.CustomModel, error) {
			if f.conflict {
				return store.CustomModel{}, fmt.Errorf("%w: duplicate", store.ErrCustomModelConflict)
			}
			f.created = append(f.created, input)
			row := store.CustomModel{
				ID: "cm-new", Name: input.Name, BaseURL: input.BaseURL,
				UpstreamModel: input.UpstreamModel, Enabled: input.Enabled,
				CreatedAt: 100, UpdatedAt: 100, KeySet: input.APIKey != "",
			}
			f.models = append(f.models, row)
			return row, nil
		},
		UpdateCustomModel: func(id string, patch CustomModelPatch) (store.CustomModel, error) {
			f.updated = append(f.updated, customModelUpdate{id: id, patch: patch})
			for i := range f.models {
				if f.models[i].ID != id {
					continue
				}
				if patch.Name != nil {
					f.models[i].Name = *patch.Name
				}
				if patch.BaseURL != nil {
					f.models[i].BaseURL = *patch.BaseURL
				}
				if patch.UpstreamModel != nil {
					f.models[i].UpstreamModel = *patch.UpstreamModel
				}
				if patch.Enabled != nil {
					f.models[i].Enabled = *patch.Enabled
				}
				if patch.APIKey != nil {
					f.models[i].KeySet = *patch.APIKey != ""
				}
				return f.models[i], nil
			}
			return store.CustomModel{}, store.ErrNotFound
		},
		DeleteCustomModel: func(id string) (bool, error) {
			f.deleted = append(f.deleted, id)
			for i := range f.models {
				if f.models[i].ID == id {
					f.models = append(f.models[:i], f.models[i+1:]...)
					return true, nil
				}
			}
			return false, nil
		},
		TestCustomModel: func(id string) (int64, error) {
			for _, row := range f.models {
				if row.ID == id {
					return f.testLatency, f.testErr
				}
			}
			return 0, store.ErrNotFound
		},
	}
}

func TestCustomModelListReportsKeySet(t *testing.T) {
	fixture := &customModelFixture{models: []store.CustomModel{
		{ID: "cm-1", Name: "my-model", BaseURL: "https://a.example.com/v1", UpstreamModel: "m",
			Enabled: true, KeySet: true, CreatedAt: 1, UpdatedAt: 2},
		{ID: "cm-2", Name: "other", BaseURL: "https://b.example.com/v1", UpstreamModel: "m2", Enabled: false},
	}}
	handler := New(fixture.options())

	recorder := doRequest(t, handler, http.MethodGet, "/api/custom-models", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("list = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Models []store.CustomModel `json:"models"`
	}
	decodeJSONBody(t, recorder, &payload)
	if len(payload.Models) != 2 {
		t.Fatalf("models = %+v, want 2 rows", payload.Models)
	}
	if !payload.Models[0].KeySet || payload.Models[1].KeySet {
		t.Fatalf("key_set = %v/%v, want true/false", payload.Models[0].KeySet, payload.Models[1].KeySet)
	}
	if recorder.Body.String() == "" || containsKeyField(recorder.Body.String()) {
		t.Fatalf("list leaks a key field: %s", recorder.Body.String())
	}
}

// containsKeyField is a crude guard against ever serialising an api_key.
func containsKeyField(body string) bool {
	return strings.Contains(body, `"api_key"`) || strings.Contains(body, `"apikey"`)
}

func TestCustomModelCreateValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"missing name", `{"base_url":"https://a.example.com/v1","upstream_model":"m","api_key":"sk"}`},
		{"slash name", `{"name":"p/m","base_url":"https://a.example.com/v1","upstream_model":"m","api_key":"sk"}`},
		{"missing url", `{"name":"m","upstream_model":"m","api_key":"sk"}`},
		{"relative url", `{"name":"m","base_url":"/v1","upstream_model":"m","api_key":"sk"}`},
		{"remote http", `{"name":"m","base_url":"http://api.example.com/v1","upstream_model":"m","api_key":"sk"}`},
		{"query url", `{"name":"m","base_url":"https://a.example.com/v1?q=1","upstream_model":"m","api_key":"sk"}`},
		{"missing upstream", `{"name":"m","base_url":"https://a.example.com/v1","api_key":"sk"}`},
		{"missing key", `{"name":"m","base_url":"https://a.example.com/v1","upstream_model":"m"}`},
		{"empty key", `{"name":"m","base_url":"https://a.example.com/v1","upstream_model":"m","api_key":"  "}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := &customModelFixture{}
			recorder := doRequest(t, New(fixture.options()), http.MethodPost, "/api/custom-models", tc.body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("create = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
			}
			if len(fixture.created) != 0 {
				t.Fatalf("host was called for invalid input: %+v", fixture.created)
			}
		})
	}
}

func TestCustomModelCreateAndConflict(t *testing.T) {
	fixture := &customModelFixture{}
	handler := New(fixture.options())

	recorder := doRequest(t, handler, http.MethodPost, "/api/custom-models",
		`{"name":"  my-model  ","base_url":"https://api.example.com/v1","upstream_model":"vendor","api_key":"sk-1"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201 (body %s)", recorder.Code, recorder.Body.String())
	}
	if len(fixture.created) != 1 {
		t.Fatalf("CreateCustomModel calls = %d", len(fixture.created))
	}
	input := fixture.created[0]
	if input.Name != "  my-model  " || input.APIKey != "sk-1" || !input.Enabled {
		t.Fatalf("input = %+v, want trimmed-by-host name, key and enabled default", input)
	}
	var created struct {
		Model store.CustomModel `json:"model"`
	}
	decodeJSONBody(t, recorder, &created)
	if created.Model.ID != "cm-new" || !created.Model.KeySet {
		t.Fatalf("created = %+v, want id cm-new and key_set", created.Model)
	}

	fixture.conflict = true
	recorder = doRequest(t, handler, http.MethodPost, "/api/custom-models",
		`{"name":"my-model","base_url":"https://api.example.com/v1","upstream_model":"vendor","api_key":"sk-2"}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("conflict = %d, want 409 (body %s)", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	decodeJSONBody(t, recorder, &payload)
	if payload.Error.Type != "conflict" {
		t.Fatalf("error type = %q, want conflict", payload.Error.Type)
	}
}

func TestCustomModelUpdateKeepsOrClearsKey(t *testing.T) {
	fixture := &customModelFixture{models: []store.CustomModel{{
		ID: "cm-1", Name: "my-model", BaseURL: "https://a.example.com/v1",
		UpstreamModel: "m", Enabled: true, KeySet: true,
	}}}
	handler := New(fixture.options())

	// Absent api_key keeps the key.
	recorder := doRequest(t, handler, http.MethodPut, "/api/custom-models/cm-1",
		`{"name":"renamed","enabled":false}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("update = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	if len(fixture.updated) != 1 {
		t.Fatalf("UpdateCustomModel calls = %d", len(fixture.updated))
	}
	patch := fixture.updated[0].patch
	if patch.APIKey != nil {
		t.Fatalf("absent api_key must be nil, got %q", *patch.APIKey)
	}
	if patch.Name == nil || *patch.Name != "renamed" || patch.Enabled == nil || *patch.Enabled {
		t.Fatalf("patch = %+v", patch)
	}
	if !fixture.models[0].KeySet {
		t.Fatal("key_set must stay true when api_key is absent")
	}

	// "" clears the key.
	recorder = doRequest(t, handler, http.MethodPut, "/api/custom-models/cm-1", `{"api_key":""}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("clear key = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	if fixture.updated[1].patch.APIKey == nil || *fixture.updated[1].patch.APIKey != "" {
		t.Fatalf("patch api_key = %v, want pointer to empty string", fixture.updated[1].patch.APIKey)
	}
	if fixture.models[0].KeySet {
		t.Fatal("key_set must be false after clearing")
	}

	// Invalid field and unknown id.
	if code := doRequest(t, handler, http.MethodPut, "/api/custom-models/cm-1",
		`{"name":"bad/name"}`).Code; code != http.StatusBadRequest {
		t.Fatalf("invalid rename = %d, want 400", code)
	}
	if code := doRequest(t, handler, http.MethodPut, "/api/custom-models/cm-missing",
		`{"enabled":true}`).Code; code != http.StatusNotFound {
		t.Fatalf("missing update = %d, want 404", code)
	}
}

func TestCustomModelDeleteAndTest(t *testing.T) {
	fixture := &customModelFixture{
		models: []store.CustomModel{{
			ID: "cm-1", Name: "my-model", BaseURL: "https://a.example.com/v1", UpstreamModel: "m", Enabled: true,
		}},
		testLatency: 42,
	}
	handler := New(fixture.options())

	recorder := doRequest(t, handler, http.MethodPost, "/api/custom-models/cm-1/test", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("test = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	var tested struct {
		OK        bool  `json:"ok"`
		LatencyMS int64 `json:"latency_ms"`
	}
	decodeJSONBody(t, recorder, &tested)
	if !tested.OK || tested.LatencyMS != 42 {
		t.Fatalf("test payload = %+v, want ok/42", tested)
	}

	fixture.testErr = errors.New("上游返回状态 401：invalid key")
	recorder = doRequest(t, handler, http.MethodPost, "/api/custom-models/cm-1/test", "")
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("failing test = %d, want 502 (body %s)", recorder.Code, recorder.Body.String())
	}
	var failed struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	decodeJSONBody(t, recorder, &failed)
	if failed.OK || failed.Error == "" {
		t.Fatalf("failing payload = %+v", failed)
	}

	if code := doRequest(t, handler, http.MethodPost, "/api/custom-models/cm-missing/test", "").Code; code != http.StatusNotFound {
		t.Fatalf("test unknown = %d, want 404", code)
	}

	recorder = doRequest(t, handler, http.MethodDelete, "/api/custom-models/cm-1", "")
	if recorder.Code != http.StatusOK || len(fixture.deleted) != 1 {
		t.Fatalf("delete = %d (deleted %v), want 200", recorder.Code, fixture.deleted)
	}
	if code := doRequest(t, handler, http.MethodDelete, "/api/custom-models/cm-1", "").Code; code != http.StatusNotFound {
		t.Fatalf("second delete = %d, want 404", code)
	}
}

func TestCustomModelCallbacksOptional(t *testing.T) {
	handler := New(Options{})
	if code := doRequest(t, handler, http.MethodGet, "/api/custom-models", "").Code; code != http.StatusNotImplemented {
		t.Fatalf("list without callbacks = %d, want 501", code)
	}
	if code := doRequest(t, handler, http.MethodPost, "/api/custom-models/cm-1/test", "").Code; code != http.StatusNotImplemented {
		t.Fatalf("test without callbacks = %d, want 501", code)
	}
}
