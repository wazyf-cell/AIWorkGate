package webui

import (
	"net/http"
	"strings"
	"testing"
)

func TestModelTestRoute(t *testing.T) {
	var gotProvider, gotModel string
	h := New(marketOptions(func(o *Options) {
		o.ModelTest = func(provider, model string) (ModelTestResult, error) {
			gotProvider, gotModel = provider, model
			return ModelTestResult{OK: true, Status: 200, MS: 37}, nil
		}
	}))

	rec := doRequest(t, h, http.MethodPost, "/api/models/trae/claude-sonnet-4/test", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("test = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var payload map[string]any
	decodeJSONBody(t, rec, &payload)
	if payload["ok"] != true || payload["status"].(float64) != 200 || payload["ms"].(float64) != 37 {
		t.Fatalf("payload = %v", payload)
	}
	if payload["model"] != "trae/claude-sonnet-4" {
		t.Fatalf("model = %v, want the route echo", payload["model"])
	}
	if _, hasErr := payload["error"]; hasErr {
		t.Fatalf("success must omit error: %v", payload)
	}
	if gotProvider != "trae" || gotModel != "claude-sonnet-4" {
		t.Fatalf("callback saw %q/%q", gotProvider, gotModel)
	}

	if rec := doRequest(t, h, http.MethodGet, "/api/models/trae/trae-cn/test", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d, want 405", rec.Code)
	}
	nilH := New(marketOptions(nil))
	if rec := doRequest(t, nilH, http.MethodPost, "/api/models/trae/trae-cn/test", ""); rec.Code != http.StatusNotImplemented {
		t.Fatalf("nil callback = %d, want 501", rec.Code)
	}
	errH := New(marketOptions(func(o *Options) {
		o.ModelTest = func(string, string) (ModelTestResult, error) { return ModelTestResult{}, errSentinel }
	}))
	if rec := doRequest(t, errH, http.MethodPost, "/api/models/trae/trae-cn/test", ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("callback error = %d, want 500", rec.Code)
	}
}

var errSentinel = errorString("probe boom")

type errorString string

func (e errorString) Error() string { return string(e) }

func TestModelTestBatchRoute(t *testing.T) {
	h := New(marketOptions(func(o *Options) {
		o.ModelTestBatch = func(items []ModelTestItem) ([]ModelTestResult, error) {
			out := make([]ModelTestResult, 0, len(items))
			for i, item := range items {
				out = append(out, ModelTestResult{OK: i == 0, Status: 200, Model: item.Provider + "/" + item.Model})
			}
			return out, nil
		}
	}))

	rec := doRequest(t, h, http.MethodPost, "/api/models/test",
		`{"items":[{"provider":"trae","model":"claude-sonnet-4"},{"provider":"officeace","model":"glm-5.3"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("batch = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var payload struct {
		Results []ModelTestResult `json:"results"`
	}
	decodeJSONBody(t, rec, &payload)
	if len(payload.Results) != 2 || payload.Results[0].Model != "trae/claude-sonnet-4" || payload.Results[1].OK {
		t.Fatalf("results = %+v", payload.Results)
	}

	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"empty", `{"items":[]}`, http.StatusBadRequest},
		{"no items", `{}`, http.StatusBadRequest},
		{"too many", `{"items":[{"provider":"p","model":"m"}]}`, http.StatusBadRequest},
		{"bad json", `{`, http.StatusBadRequest},
		{"missing fields", `{"items":[{"provider":"trae","model":""}]}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body
			if tc.name == "too many" {
				var many []string
				for i := 0; i < 51; i++ {
					many = append(many, `{"provider":"p","model":"m"}`)
				}
				body = `{"items":[` + strings.Join(many, ",") + `]}`
			}
			rec := doRequest(t, h, http.MethodPost, "/api/models/test", body)
			if rec.Code != tc.want {
				t.Fatalf("%s = %d, want %d (body %s)", tc.name, rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestModelTestBatchUnavailable(t *testing.T) {
	h := New(marketOptions(nil))
	rec := doRequest(t, h, http.MethodPost, "/api/models/test", `{"items":[{"provider":"p","model":"m"}]}`)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("nil callback = %d, want 501", rec.Code)
	}
}
