package webui

import (
	"net/http"
	"strings"
	"testing"
)

func TestUsageClearEndpoints(t *testing.T) {
	var note string
	calls := 0
	handler := New(Options{
		ClearUsage: func(value string) (any, error) {
			calls++
			note = value
			return map[string]any{"clear": map[string]any{"note": value}}, nil
		},
		UsageClears: func(limit int) (any, error) {
			return map[string]any{"limit": limit, "clears": []any{}}, nil
		},
	})

	recorder := doRequest(t, handler, http.MethodPost, "/api/usage/clear", `{"note":"  before upgrade  "}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("clear status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if calls != 1 || note != "before upgrade" {
		t.Fatalf("callback = %d/%q", calls, note)
	}
	var payload map[string]any
	decodeJSONBody(t, recorder, &payload)
	if _, ok := payload["clear"]; !ok {
		t.Fatalf("payload = %v", payload)
	}

	long := `{"note":"` + strings.Repeat("a", 201) + `"}`
	recorder = doRequest(t, handler, http.MethodPost, "/api/usage/clear", long)
	if recorder.Code != http.StatusBadRequest || calls != 1 {
		t.Fatalf("long note = %d calls=%d, want 400 without callback", recorder.Code, calls)
	}

	recorder = doRequest(t, handler, http.MethodGet, "/api/usage/clears?limit=999", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("clears status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	payload = map[string]any{}
	decodeJSONBody(t, recorder, &payload)
	if payload["limit"] != float64(500) {
		t.Fatalf("limit = %v, want the 500 cap", payload["limit"])
	}

	if recorder = doRequest(t, handler, http.MethodGet, "/api/usage/clear", ""); recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET clear = %d, want 405", recorder.Code)
	}
	if recorder = doRequest(t, New(Options{}), http.MethodPost, "/api/usage/clear", `{}`); recorder.Code != http.StatusNotImplemented {
		t.Fatalf("unwired clear = %d, want 501", recorder.Code)
	}
}
