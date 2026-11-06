package webui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

func TestRoutingGroupsEndpoint(t *testing.T) {
	var saved json.RawMessage
	handler := New(Options{
		RoutingGroups: func() (any, error) {
			return map[string]any{"groups": []any{}, "universe": []string{"p/m"}}, nil
		},
		SaveRoutingGroups: func(groups json.RawMessage) (any, error) {
			saved = groups
			return map[string]any{"groups": []any{}}, nil
		},
	})

	recorder := doRequest(t, handler, http.MethodGet, "/api/routing-groups", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	var view map[string]any
	decodeJSONBody(t, recorder, &view)
	if _, ok := view["universe"]; !ok {
		t.Fatalf("GET payload = %v", view)
	}

	recorder = doRequest(t, handler, http.MethodPut, "/api/routing-groups",
		`{"groups":[{"id":"g","match":["*"]}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if string(saved) != `[{"id":"g","match":["*"]}]` {
		t.Fatalf("saved = %s", saved)
	}

	if recorder = doRequest(t, handler, http.MethodPut, "/api/routing-groups", `{}`); recorder.Code != http.StatusBadRequest {
		t.Fatalf("missing groups status = %d, want 400", recorder.Code)
	}
	if recorder = doRequest(t, handler, http.MethodDelete, "/api/routing-groups", ""); recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE status = %d, want 405", recorder.Code)
	}
	if recorder = doRequest(t, New(Options{}), http.MethodGet, "/api/routing-groups", ""); recorder.Code != http.StatusNotImplemented {
		t.Fatalf("unwired status = %d, want 501", recorder.Code)
	}
}

func TestRoutingGroupsValidationMapsTo400(t *testing.T) {
	handler := New(Options{
		SaveRoutingGroups: func(json.RawMessage) (any, error) {
			return nil, fmt.Errorf("%w: bad group", ErrInvalidRequest)
		},
	})
	recorder := doRequest(t, handler, http.MethodPut, "/api/routing-groups", `{"groups":[]}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", recorder.Code, recorder.Body.String())
	}
}
