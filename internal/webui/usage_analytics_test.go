package webui

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"aiworkgate/internal/store"
)

func TestUsageAnalyticsDefaultsAndParams(t *testing.T) {
	var latencyDays, keyDays []int
	handler := New(Options{
		UsageLatency: func(days int) ([]store.DayLatency, error) {
			latencyDays = append(latencyDays, days)
			return nil, nil // nil must serialise as []
		},
		UsageKeyUsage: func(days int) ([]store.KeyUsageRow, error) {
			keyDays = append(keyDays, days)
			return nil, nil
		},
	})

	recorder := doRequest(t, handler, http.MethodGet, "/api/usage/latency", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("latency status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if got := strings.TrimSpace(recorder.Body.String()); got != "[]" {
		t.Fatalf("empty latency body = %q, want []", got)
	}
	if len(latencyDays) != 1 || latencyDays[0] != 14 {
		t.Fatalf("default latency days = %v, want [14]", latencyDays)
	}

	recorder = doRequest(t, handler, http.MethodGet, "/api/usage/latency?days=7", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("latency days=7 status = %d", recorder.Code)
	}
	if latencyDays[len(latencyDays)-1] != 7 {
		t.Fatalf("latency days = %v, want 7 last", latencyDays)
	}

	recorder = doRequest(t, handler, http.MethodGet, "/api/usage/keys", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("keys status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if got := strings.TrimSpace(recorder.Body.String()); got != "[]" {
		t.Fatalf("empty keys body = %q, want []", got)
	}
	if len(keyDays) != 1 || keyDays[0] != 30 {
		t.Fatalf("default key days = %v, want [30]", keyDays)
	}

	// Out-of-range and malformed windows are rejected, not clamped silently.
	for _, target := range []string{
		"/api/usage/latency?days=0",
		"/api/usage/latency?days=91",
		"/api/usage/latency?days=abc",
		"/api/usage/keys?days=-1",
		"/api/usage/keys?days=1.5",
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

func TestUsageAnalyticsPayloadShape(t *testing.T) {
	p50, p95 := int64(12), int64(80)
	credits := 0.25
	handler := New(Options{
		UsageLatency: func(int) ([]store.DayLatency, error) {
			return []store.DayLatency{{
				Day: "2026-10-21", Requests: 2, P50MS: &p50, P95MS: &p95,
			}}, nil
		},
		UsageKeyUsage: func(int) ([]store.KeyUsageRow, error) {
			return []store.KeyUsageRow{{
				KeyID: "k1", Name: "laptop", Requests: 2, PromptTokens: 10,
				CompletionTokens: 4, CachedTokens: 3, Credits: &credits,
			}}, nil
		},
	})

	recorder := doRequest(t, handler, http.MethodGet, "/api/usage/latency?days=30", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("latency status = %d", recorder.Code)
	}
	var latency []store.DayLatency
	decodeJSONBody(t, recorder, &latency)
	if len(latency) != 1 || latency[0].Day != "2026-10-21" ||
		latency[0].P50MS == nil || *latency[0].P50MS != 12 ||
		latency[0].P50TTFBMS != nil {
		t.Fatalf("latency payload = %+v", latency)
	}

	recorder = doRequest(t, handler, http.MethodGet, "/api/usage/keys?days=30", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("keys status = %d", recorder.Code)
	}
	var keys []store.KeyUsageRow
	decodeJSONBody(t, recorder, &keys)
	if len(keys) != 1 || keys[0].KeyID != "k1" || keys[0].CachedTokens != 3 ||
		keys[0].Credits == nil || *keys[0].Credits != 0.25 {
		t.Fatalf("keys payload = %+v", keys)
	}
}
