package app

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"aiworkgate/internal/config"
)

// stubRoute records which surface a routed request landed on.
type stubRoute struct {
	kind string // "gateway" | "admin"
}

func (s *stubRoute) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"surface":"` + s.kind + `"}`))
}

func TestIsGatewayPath(t *testing.T) {
	for _, path := range []string{
		"/v1", "/v1/models", "/v1/chat/completions",
		"/v1beta/models", "/v1beta/models/gemini-pro:generateContent",
	} {
		if !isGatewayPath(path) {
			t.Fatalf("%q is not recognized as a gateway path", path)
		}
	}
	for _, path := range []string{"/", "/api/status", "/v1beta", "/v1beta2/models", "/v10/models"} {
		if isGatewayPath(path) {
			t.Fatalf("%q wrongly recognized as a gateway path", path)
		}
	}
}

// TestRouteHTTPMountsV1Beta pins the P6 Gemini mount: /v1beta/* must reach the
// gateway handler (mirroring its own path whitelist), never fall through to
// the SPA handler that answers non-GET with 405.
func TestRouteHTTPMountsV1Beta(t *testing.T) {
	gw, adm := &stubRoute{kind: "gateway"}, &stubRoute{kind: "admin"}
	a := &App{cfg: config.Default(), gatewayHandler: gw, adminHandler: adm}

	routes := map[string]string{
		"/v1/chat/completions":                  "gateway",
		"/v1/models":                            "gateway",
		"/v1beta/models/m:generateContent":      "gateway",
		"/v1beta/models/m:streamGenerateContent": "gateway",
		"/":                                     "admin",
		"/api/status":                           "admin",
		"/v1beta":                               "admin",
	}
	for path, want := range routes {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Host = "127.0.0.1"
		rec := httptest.NewRecorder()
		a.routeHTTP(rec, req)
		if rec.Code != http.StatusOK || rec.Body.String() != `{"surface":"`+want+`"}` {
			t.Fatalf("route %q: got %d %s, want surface %q", path, rec.Code, rec.Body.String(), want)
		}
	}
}

// TestRouteHTTPV1BetaSkipsLANPassword extends the /v1 exemption to /v1beta:
// a LAN listener must still serve gateway paths to client-key callers
// instead of demanding the admin LAN password.
func TestRouteHTTPV1BetaSkipsLANPassword(t *testing.T) {
	gw, adm := &stubRoute{kind: "gateway"}, &stubRoute{kind: "admin"}
	cfg := config.Default()
	cfg.ListenLAN = true
	a := &App{cfg: cfg, gatewayHandler: gw, adminHandler: adm}

	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/m:generateContent", nil)
	req.Host = "127.0.0.1"
	rec := httptest.NewRecorder()
	a.routeHTTP(rec, req) // httptest.NewRequest remote is non-loopback by default
	if rec.Code != http.StatusOK || rec.Body.String() != `{"surface":"gateway"}` {
		t.Fatalf("LAN /v1beta: got %d %s, want gateway surface without password", rec.Code, rec.Body.String())
	}
}
