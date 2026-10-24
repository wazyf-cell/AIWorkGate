package webui

import (
	"encoding/json"
	"net/http"
	"testing"

	"aiworkgate/internal/store"
)

// diagnosticsFixture is the host side of the connectivity page.
type diagnosticsFixture struct {
	requests []ProbeRequest
	result   ProbeResult
	err      error

	history []store.ConnectivityProbe

	calledLimit  int
	calledKind   string
	calledTarget string
}

func (f *diagnosticsFixture) options() Options {
	return Options{
		ProbeConnectivity: func(req ProbeRequest) (ProbeResult, error) {
			f.requests = append(f.requests, req)
			return f.result, f.err
		},
		ProbeHistory: func(limit int, kind, targetID string) ([]store.ConnectivityProbe, error) {
			f.calledLimit, f.calledKind, f.calledTarget = limit, kind, targetID
			return f.history, nil
		},
	}
}

func TestDiagnosticsProbeValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"missing kind", `{}`},
		{"unknown kind", `{"kind":"banana"}`},
		{"plugin without plugin_id", `{"kind":"plugin","model":"m"}`},
		{"plugin without model", `{"kind":"plugin","plugin_id":"officeace"}`},
		{"custom without id", `{"kind":"custom_model"}`},
		{"malformed json", `{"kind":`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := &diagnosticsFixture{}
			recorder := doRequest(t, New(fixture.options()), http.MethodPost, "/api/diagnostics/probe", tc.body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
			}
			var payload struct {
				Error struct {
					Type string `json:"type"`
				} `json:"error"`
			}
			decodeJSONBody(t, recorder, &payload)
			if payload.Error.Type != "invalid_request" && payload.Error.Type != "bad_request" {
				t.Fatalf("error type = %q, want invalid_request/bad_request", payload.Error.Type)
			}
			if len(fixture.requests) != 0 {
				t.Fatalf("host was called for invalid input: %+v", fixture.requests)
			}
		})
	}

	if code := doRequest(t, New(Options{}), http.MethodGet, "/api/diagnostics/probe", "").Code; code != http.StatusMethodNotAllowed {
		t.Fatalf("GET probe = %d, want 405", code)
	}
}

func TestDiagnosticsProbeResponseShape(t *testing.T) {
	fixture := &diagnosticsFixture{result: ProbeResult{
		OK: true, Kind: ProbeKindPlugin, Target: "officeace", TargetID: "officeace/glm-5.3",
		Status: 200, LatencyMS: 42, Detail: "模型存在 · 9 个模型", At: 1792521234, ProbeID: 12,
	}}
	handler := New(fixture.options())

	recorder := doRequest(t, handler, http.MethodPost, "/api/diagnostics/probe",
		`{"kind":" Plugin ","plugin_id":" officeace ","model":" officeace/glm-5.3 "}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("probe = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	if len(fixture.requests) != 1 {
		t.Fatalf("ProbeConnectivity calls = %d", len(fixture.requests))
	}
	req := fixture.requests[0]
	if req.Kind != ProbeKindPlugin || req.PluginID != "officeace" || req.Model != "officeace/glm-5.3" {
		t.Fatalf("request = %+v, want trimmed values and a lower-case kind", req)
	}

	var payload map[string]any
	decodeJSONBody(t, recorder, &payload)
	for _, key := range []string{"ok", "kind", "target", "target_id", "status", "latency_ms", "detail", "at", "probe_id"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("response lacks %q: %v", key, payload)
		}
	}
	if payload["ok"] != true || payload["probe_id"].(float64) != 12 || payload["latency_ms"].(float64) != 42 {
		t.Fatalf("response payload = %v", payload)
	}
	if _, ok := payload["error_code"]; ok {
		t.Fatalf("success response must omit error_code: %v", payload)
	}

	// A failed probe is still HTTP 200 and carries the error code.
	fixture.result = ProbeResult{
		OK: false, Kind: ProbeKindPlugin, Target: "officeace", TargetID: "officeace/missing",
		Status: 200, LatencyMS: 7, Detail: "模型不存在 · 9 个模型", ErrorCode: "model_missing",
		At: 1792521300, ProbeID: 13,
	}
	recorder = doRequest(t, handler, http.MethodPost, "/api/diagnostics/probe",
		`{"kind":"plugin","plugin_id":"officeace","model":"missing"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("failed probe = %d, want 200", recorder.Code)
	}
	payload = map[string]any{}
	decodeJSONBody(t, recorder, &payload)
	if payload["ok"] != false || payload["error_code"] != "model_missing" {
		t.Fatalf("failure payload = %v", payload)
	}

	// A custom model request reaches the host with its id.
	recorder = doRequest(t, handler, http.MethodPost, "/api/diagnostics/probe",
		`{"kind":"custom_model","custom_model_id":"cm-abc"}`)
	if recorder.Code != http.StatusOK || fixture.requests[2].CustomModelID != "cm-abc" {
		t.Fatalf("custom probe = %d, req %+v", recorder.Code, fixture.requests[2])
	}

	// kind "a" takes no parameters.
	recorder = doRequest(t, handler, http.MethodPost, "/api/diagnostics/probe", `{"kind":"a"}`)
	if recorder.Code != http.StatusOK || fixture.requests[3].Kind != ProbeKindA {
		t.Fatalf("a probe = %d, req %+v", recorder.Code, fixture.requests[3])
	}
}

func TestDiagnosticsProbeHistory(t *testing.T) {
	fixture := &diagnosticsFixture{history: []store.ConnectivityProbe{
		{ID: 3, Kind: "plugin", Target: "officeace", TargetID: "officeace/glm-5.3",
			OK: true, Status: 200, LatencyMS: 42, Detail: "模型存在 · 9 个模型", CreatedAt: 1792521234},
		{ID: 2, Kind: "a", Target: "a", TargetID: "127.0.0.1:7864",
			OK: false, Status: 0, LatencyMS: 8000, Detail: "A 未运行", CreatedAt: 1792521200},
	}}
	handler := New(fixture.options())

	recorder := doRequest(t, handler, http.MethodGet, "/api/diagnostics/probes", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("history = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	if fixture.calledLimit != diagnosticsProbeDefaultLimit || fixture.calledKind != "" || fixture.calledTarget != "" {
		t.Fatalf("host call = %d/%q/%q, want defaults", fixture.calledLimit, fixture.calledKind, fixture.calledTarget)
	}
	var payload struct {
		Probes []map[string]any `json:"probes"`
	}
	decodeJSONBody(t, recorder, &payload)
	if len(payload.Probes) != 2 {
		t.Fatalf("probes = %v", payload.Probes)
	}
	first := payload.Probes[0]
	if first["id"].(float64) != 3 || first["kind"] != "plugin" || first["target_id"] != "officeace/glm-5.3" ||
		first["ok"] != true || first["latency_ms"].(float64) != 42 || first["at"].(float64) != 1792521234 {
		t.Fatalf("first probe = %v", first)
	}
	if _, ok := first["error_code"]; ok {
		t.Fatalf("history rows must follow the documented shape: %v", first)
	}

	// limit clamps to 100 and filters pass through.
	recorder = doRequest(t, handler, http.MethodGet,
		"/api/diagnostics/probes?limit=500&kind=PLUGIN&target_id=officeace%2Fglm-5.3", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("filtered history = %d", recorder.Code)
	}
	if fixture.calledLimit != diagnosticsProbeMaxLimit || fixture.calledKind != "plugin" ||
		fixture.calledTarget != "officeace/glm-5.3" {
		t.Fatalf("host call = %d/%q/%q", fixture.calledLimit, fixture.calledKind, fixture.calledTarget)
	}

	// No rows serializes as an empty array, never null.
	fixture.history = nil
	recorder = doRequest(t, handler, http.MethodGet, "/api/diagnostics/probes?limit=1", "")
	if body := recorder.Body.String(); body != "{\"probes\":[]}\n" {
		t.Fatalf("empty history body = %q, want an empty probes array", body)
	}

	if code := doRequest(t, handler, http.MethodGet, "/api/diagnostics/probes?kind=banana", "").Code; code != http.StatusBadRequest {
		t.Fatalf("bad kind filter = %d, want 400", code)
	}
	if code := doRequest(t, handler, http.MethodPost, "/api/diagnostics/probes", "").Code; code != http.StatusMethodNotAllowed {
		t.Fatalf("POST history = %d, want 405", code)
	}
}

func TestDiagnosticsCallbacksOptional(t *testing.T) {
	handler := New(Options{})
	if code := doRequest(t, handler, http.MethodPost, "/api/diagnostics/probe", `{"kind":"a"}`).Code; code != http.StatusNotImplemented {
		t.Fatalf("probe without callback = %d, want 501", code)
	}
	if code := doRequest(t, handler, http.MethodGet, "/api/diagnostics/probes", "").Code; code != http.StatusNotImplemented {
		t.Fatalf("history without callback = %d, want 501", code)
	}
}

// TestDiagnosticsHostErrorIsInternal keeps an unexpected callback failure out
// of the probe contract: only probe outcomes are 200, transport failures are 500.
func TestDiagnosticsHostErrorIsInternal(t *testing.T) {
	fixture := &diagnosticsFixture{err: json.Unmarshal([]byte(`{`), &struct{}{})}
	recorder := doRequest(t, New(fixture.options()), http.MethodPost, "/api/diagnostics/probe", `{"kind":"a"}`)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("host error = %d, want 500 (body %s)", recorder.Code, recorder.Body.String())
	}
}
