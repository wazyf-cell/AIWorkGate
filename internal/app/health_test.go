package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// /health mirrors A's liveness probe and must answer before any app state is
// initialised, so launchers can wait for the port to open.
func TestHealthEndpointIsPublicJSON(t *testing.T) {
	a := &App{}
	rec := httptest.NewRecorder()
	a.routeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Status  string `json:"status"`
		Running bool   `json:"running"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body %q)", err, rec.Body.String())
	}
	if body.Status != "ok" || !body.Running {
		t.Fatalf("body = %+v, want ok/running", body)
	}
}

func TestHealthEndpointRejectsWrites(t *testing.T) {
	a := &App{}
	rec := httptest.NewRecorder()
	a.routeHTTP(rec, httptest.NewRequest(http.MethodPost, "/health", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}
