package webui

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"aiworkgate/internal/store"
)

func TestUsageModelEndpointsDefaultsAndParams(t *testing.T) {
	var modelDays, latencyDays []int
	handler := New(Options{
		UsageModels: func(days int) ([]store.ModelUsageRow, error) {
			modelDays = append(modelDays, days)
			return nil, nil // nil must serialise as []
		},
		UsageModelLatency: func(days int) ([]store.ModelLatencyRow, error) {
			latencyDays = append(latencyDays, days)
			return nil, nil
		},
	})

	recorder := doRequest(t, handler, http.MethodGet, "/api/usage/models", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("models status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if got := strings.TrimSpace(recorder.Body.String()); got != "[]" {
		t.Fatalf("empty models body = %q, want []", got)
	}
	if len(modelDays) != 1 || modelDays[0] != 30 {
		t.Fatalf("default model days = %v, want [30]", modelDays)
	}

	recorder = doRequest(t, handler, http.MethodGet, "/api/usage/models?days=7", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("models days=7 status = %d", recorder.Code)
	}
	if modelDays[len(modelDays)-1] != 7 {
		t.Fatalf("model days = %v, want 7 last", modelDays)
	}

	recorder = doRequest(t, handler, http.MethodGet, "/api/usage/latency/models", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("model latency status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if got := strings.TrimSpace(recorder.Body.String()); got != "[]" {
		t.Fatalf("empty model latency body = %q, want []", got)
	}
	if len(latencyDays) != 1 || latencyDays[0] != 14 {
		t.Fatalf("default model latency days = %v, want [14]", latencyDays)
	}

	recorder = doRequest(t, handler, http.MethodGet, "/api/usage/latency/models?days=90", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("model latency days=90 status = %d", recorder.Code)
	}
	if latencyDays[len(latencyDays)-1] != 90 {
		t.Fatalf("model latency days = %v, want 90 last", latencyDays)
	}

	// Same validation as the older analytics endpoints: 400 invalid_query.
	for _, target := range []string{
		"/api/usage/models?days=0",
		"/api/usage/models?days=91",
		"/api/usage/models?days=abc",
		"/api/usage/latency/models?days=-3",
		"/api/usage/latency/models?days=1.5",
	} {
		recorder = doRequest(t, handler, http.MethodGet, target, "")
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400", target, recorder.Code)
		}
		var payload struct {
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
			t.Fatalf("%s body %q: %v", target, recorder.Body.String(), err)
		}
		if payload.Error.Type != "invalid_query" {
			t.Fatalf("%s error type = %q", target, payload.Error.Type)
		}
	}
}

func TestUsageModelEndpointPayloadShape(t *testing.T) {
	p50, p95 := int64(120), int64(900)
	credits := 1.25
	handler := New(Options{
		UsageModels: func(int) ([]store.ModelUsageRow, error) {
			return []store.ModelUsageRow{
				{Model: "kimi-k2", Requests: 9, PromptTokens: 100, CompletionTokens: 40, Credits: &credits},
				{Model: "gpt-4o", Requests: 2, PromptTokens: 5, CompletionTokens: 1},
			}, nil
		},
		UsageModelLatency: func(int) ([]store.ModelLatencyRow, error) {
			return []store.ModelLatencyRow{{
				Model: "kimi-k2", Requests: 9, P50MS: &p50, P95MS: &p95,
			}}, nil
		},
	})

	recorder := doRequest(t, handler, http.MethodGet, "/api/usage/models?days=30", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("models status = %d", recorder.Code)
	}
	var models []store.ModelUsageRow
	decodeJSONBody(t, recorder, &models)
	if len(models) != 2 || models[0].Model != "kimi-k2" ||
		models[0].Credits == nil || *models[0].Credits != 1.25 {
		t.Fatalf("models payload = %+v", models)
	}
	if models[1].Credits != nil {
		t.Fatalf("unreported credits = %v, want null", *models[1].Credits)
	}
	// The JSON field must be the literal null so the UI can tell it apart
	// from a real 0.
	if !strings.Contains(recorder.Body.String(), `"credits":null`) {
		t.Fatalf("body %q must keep credits null", recorder.Body.String())
	}

	recorder = doRequest(t, handler, http.MethodGet, "/api/usage/latency/models", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("model latency status = %d", recorder.Code)
	}
	var latency []store.ModelLatencyRow
	decodeJSONBody(t, recorder, &latency)
	if len(latency) != 1 || latency[0].Model != "kimi-k2" ||
		latency[0].P50MS == nil || *latency[0].P50MS != 120 ||
		latency[0].P50TTFBMS != nil {
		t.Fatalf("model latency payload = %+v", latency)
	}
}
