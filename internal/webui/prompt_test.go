package webui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

func promptAnalyzeHandler(t *testing.T, calls *int) http.Handler {
	t.Helper()
	return New(Options{
		PromptAnalyze: func(model string, body json.RawMessage) (any, error) {
			*calls++
			return map[string]any{
				"model":        model,
				"body_len":     len(body),
				"total_tokens": 12,
			}, nil
		},
	})
}

func TestPromptAnalyzeEndpoint(t *testing.T) {
	calls := 0
	handler := promptAnalyzeHandler(t, &calls)
	recorder := doRequest(t, handler, http.MethodPost, "/api/prompt/analyze",
		`{"model":"codearts/gpt-4o","body":{"messages":[{"role":"user","content":"hi"}]}}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	var payload map[string]any
	decodeJSONBody(t, recorder, &payload)
	if payload["model"] != "codearts/gpt-4o" {
		t.Fatalf("model = %v", payload["model"])
	}
	if payload["total_tokens"] != float64(12) {
		t.Fatalf("total_tokens = %v", payload["total_tokens"])
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestPromptAnalyzeRequiresBody(t *testing.T) {
	calls := 0
	handler := promptAnalyzeHandler(t, &calls)
	for _, body := range []string{`{"model":"m"}`, `{"model":"m","body":null}`, ``} {
		recorder := doRequest(t, handler, http.MethodPost, "/api/prompt/analyze", body)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("body %q: status = %d, want 400 (%s)", body, recorder.Code, recorder.Body.String())
		}
	}
	if calls != 0 {
		t.Fatalf("calls = %d, want 0", calls)
	}
}

func TestPromptAnalyzeInvalidRequestMapsTo400(t *testing.T) {
	handler := New(Options{
		PromptAnalyze: func(string, json.RawMessage) (any, error) {
			return nil, fmt.Errorf("%w: not an object", ErrInvalidRequest)
		},
	})
	recorder := doRequest(t, handler, http.MethodPost, "/api/prompt/analyze",
		`{"body":{"messages":[]}}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", recorder.Code, recorder.Body.String())
	}
}

func TestPromptAnalyzeMethodAndAvailability(t *testing.T) {
	calls := 0
	if recorder := doRequest(t, promptAnalyzeHandler(t, &calls), http.MethodGet, "/api/prompt/analyze", ""); recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", recorder.Code)
	}
	if recorder := doRequest(t, New(Options{}), http.MethodPost, "/api/prompt/analyze", `{"body":{}}`); recorder.Code != http.StatusNotImplemented {
		t.Fatalf("unwired status = %d, want 501", recorder.Code)
	}
}
