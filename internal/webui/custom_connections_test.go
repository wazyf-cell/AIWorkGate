package webui

import (
	"errors"
	"net/http"
	"testing"

	"aiworkgate/internal/store"
)

type connectionFixture struct {
	created   []CustomConnectionInput
	imported  [][]ImportModelInput
	remoteErr error
	duplicate bool
}

func (f *connectionFixture) options() Options {
	return Options{
		CustomConnections: func() ([]store.CustomConnection, error) {
			return []store.CustomConnection{{ID: "cc-1", Label: "主接入", Protocol: "openai",
				BaseURL: "https://a.example.com", Enabled: true, KeySet: true, ModelCount: 2}}, nil
		},
		CreateCustomConnection: func(input CustomConnectionInput) (store.CustomConnection, bool, error) {
			f.created = append(f.created, input)
			return store.CustomConnection{ID: "cc-new", Label: input.Label, Protocol: input.Protocol,
				BaseURL: input.BaseURL, Enabled: input.Enabled, KeySet: input.APIKey != ""}, !f.duplicate, nil
		},
		FetchCustomConnectionModels: func(id string) ([]RemoteModel, error) {
			if f.remoteErr != nil {
				return nil, f.remoteErr
			}
			return []RemoteModel{{ID: "gpt-4o", Name: "GPT-4o", ContextWindow: 128000}}, nil
		},
		ImportCustomConnectionModels: func(id string, models []ImportModelInput) (ImportModelsResult, error) {
			f.imported = append(f.imported, models)
			return ImportModelsResult{Created: []store.CustomModel{{ID: "cm-1", Name: "gpt-4o"}}}, nil
		},
	}
}

func TestCustomConnectionCreateValidation(t *testing.T) {
	cases := []struct{ name, body string }{
		{"unknown protocol", `{"protocol":"grpc","base_url":"https://a.example.com","api_key":"sk"}`},
		{"missing url", `{"protocol":"openai","api_key":"sk"}`},
		{"remote http", `{"protocol":"openai","base_url":"http://a.example.com","api_key":"sk"}`},
		{"missing key", `{"protocol":"openai","base_url":"https://a.example.com"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := &connectionFixture{}
			recorder := doRequest(t, New(fixture.options()), http.MethodPost, "/api/custom-connections", tc.body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("create = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
			}
			if len(fixture.created) != 0 {
				t.Fatalf("host called for invalid input: %+v", fixture.created)
			}
		})
	}
}

func TestCustomConnectionCreateDuplicateReturnsExisting(t *testing.T) {
	fixture := &connectionFixture{duplicate: true}
	recorder := doRequest(t, New(fixture.options()), http.MethodPost, "/api/custom-connections",
		`{"label":"主","protocol":"anthropic","base_url":"https://api.anthropic.com","api_key":"sk"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("duplicate create = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	if len(fixture.created) != 1 || fixture.created[0].Protocol != "anthropic" {
		t.Fatalf("created = %+v", fixture.created)
	}
}

func TestCustomConnectionRemoteModelsErrorIs502(t *testing.T) {
	fixture := &connectionFixture{remoteErr: errors.New("上游返回状态 401")}
	recorder := doRequest(t, New(fixture.options()), http.MethodGet,
		"/api/custom-connections/cc-1/remote-models", "")
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("remote models = %d, want 502 (body %s)", recorder.Code, recorder.Body.String())
	}
}

func TestCustomConnectionImportPassesSelection(t *testing.T) {
	fixture := &connectionFixture{}
	handler := New(fixture.options())
	empty := doRequest(t, handler, http.MethodPost, "/api/custom-connections/cc-1/models/import", `{"models":[]}`)
	if empty.Code != http.StatusBadRequest {
		t.Fatalf("empty import = %d, want 400", empty.Code)
	}
	recorder := doRequest(t, handler, http.MethodPost, "/api/custom-connections/cc-1/models/import",
		`{"models":[{"upstream_model":"gpt-4o","name":"fast","context_window":128000,"input_types":["text","image"],"reasoning_tiers":["low","high"],"default_reasoning":"low"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("import = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	if len(fixture.imported) != 1 || len(fixture.imported[0]) != 1 {
		t.Fatalf("imported = %+v", fixture.imported)
	}
	got := fixture.imported[0][0]
	if got.UpstreamModel != "gpt-4o" || got.ContextWindow != 128000 || got.DefaultReasoning != "low" {
		t.Fatalf("import input = %+v", got)
	}
}

func TestCustomModelCreateWithConnectionDoesNotRequireKey(t *testing.T) {
	fixture := &customModelFixture{}
	handler := New(fixture.options())
	recorder := doRequest(t, handler, http.MethodPost, "/api/custom-models",
		`{"connection_id":"cc-1","name":"fast","upstream_model":"gpt-4o","max_output_tokens":4096,"input_types":["text"]}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201 (body %s)", recorder.Code, recorder.Body.String())
	}
	if len(fixture.created) != 1 {
		t.Fatalf("created = %+v", fixture.created)
	}
	input := fixture.created[0]
	if input.ConnectionID != "cc-1" || input.MaxOutputTokens != 4096 || len(input.InputTypes) != 1 {
		t.Fatalf("input = %+v", input)
	}
}
