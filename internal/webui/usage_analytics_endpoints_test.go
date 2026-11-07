package webui

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"aiworkgate/internal/store"
)

func TestUsageHeatmapDefaultsAndParams(t *testing.T) {
	var heatmapDays []int
	handler := New(Options{
		UsageHeatmap: func(days int) ([]store.DailyCount, error) {
			heatmapDays = append(heatmapDays, days)
			return nil, nil // nil must serialise as []
		},
	})

	recorder := doRequest(t, handler, http.MethodGet, "/api/usage/heatmap", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("heatmap status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if got := strings.TrimSpace(recorder.Body.String()); got != "[]" {
		t.Fatalf("empty heatmap body = %q, want []", got)
	}
	if len(heatmapDays) != 1 || heatmapDays[0] != 26*7 {
		t.Fatalf("default heatmap days = %v, want [%d]", heatmapDays, 26*7)
	}

	recorder = doRequest(t, handler, http.MethodGet, "/api/usage/heatmap?days=30", "")
	if recorder.Code != http.StatusOK || heatmapDays[len(heatmapDays)-1] != 30 {
		t.Fatalf("heatmap days=30: status=%d days=%v", recorder.Code, heatmapDays)
	}

	// Same window validation as every other usage observability endpoint.
	for _, target := range []string{
		"/api/usage/heatmap?days=0",
		"/api/usage/heatmap?days=91",
		"/api/usage/heatmap?days=abc",
	} {
		recorder = doRequest(t, handler, http.MethodGet, target, "")
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400", target, recorder.Code)
		}
	}
}

func TestUsageHeatmapAndReasoningPayloadShape(t *testing.T) {
	handler := New(Options{
		UsageHeatmap: func(int) ([]store.DailyCount, error) {
			return []store.DailyCount{{Day: "2026-11-07", Requests: 12, Errors: 1}}, nil
		},
		UsageReasoning: func(int) ([]store.ReasoningSplitRow, error) {
			return []store.ReasoningSplitRow{
				{Model: "m1", WithThinking: true, Requests: 2, PromptTokens: 21, CompletionTokens: 42, ReasoningTokens: 50},
				{Model: "m1", WithThinking: false, Requests: 1, PromptTokens: 12, CompletionTokens: 24},
			}, nil
		},
	})

	recorder := doRequest(t, handler, http.MethodGet, "/api/usage/heatmap?days=7", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("heatmap status = %d", recorder.Code)
	}
	var days []store.DailyCount
	decodeJSONBody(t, recorder, &days)
	if len(days) != 1 || days[0].Day != "2026-11-07" || days[0].Requests != 12 || days[0].Errors != 1 {
		t.Fatalf("heatmap payload = %+v", days)
	}

	recorder = doRequest(t, handler, http.MethodGet, "/api/usage/reasoning?days=7", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("reasoning status = %d", recorder.Code)
	}
	var rows []store.ReasoningSplitRow
	decodeJSONBody(t, recorder, &rows)
	if len(rows) != 2 || rows[0].Model != "m1" || !rows[0].WithThinking ||
		rows[0].ReasoningTokens != 50 || rows[1].WithThinking {
		t.Fatalf("reasoning payload = %+v", rows)
	}
	var probe map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(recorder.Body.String())), &probe); err != nil {
		var list []map[string]any
		if err2 := json.Unmarshal([]byte(strings.TrimSpace(recorder.Body.String())), &list); err2 != nil {
			t.Fatalf("reasoning body not JSON: %v / %v", err, err2)
		}
	}
}

func TestUsageReasoningDefaultsAndUnavailable(t *testing.T) {
	var reasoningDays []int
	handler := New(Options{
		UsageReasoning: func(days int) ([]store.ReasoningSplitRow, error) {
			reasoningDays = append(reasoningDays, days)
			return []store.ReasoningSplitRow{}, nil
		},
	})

	recorder := doRequest(t, handler, http.MethodGet, "/api/usage/reasoning", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("reasoning status = %d", recorder.Code)
	}
	if len(reasoningDays) != 1 || reasoningDays[0] != 30 {
		t.Fatalf("default reasoning days = %v, want [30]", reasoningDays)
	}

	// A nil callback answers 501, like every other observability endpoint.
	bare := New(Options{})
	recorder = doRequest(t, bare, http.MethodGet, "/api/usage/heatmap", "")
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("nil heatmap callback status = %d, want 501", recorder.Code)
	}
	recorder = doRequest(t, bare, http.MethodGet, "/api/usage/reasoning", "")
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("nil reasoning callback status = %d, want 501", recorder.Code)
	}
}

func TestUsageRetentionEndpoint(t *testing.T) {
	handler := New(Options{
		Retention: func() (RetentionInfo, error) {
			return RetentionInfo{APIRequestDays: 30, BodyPreviewDays: 3, HostLogDays: 30, ProxyLogDays: 7}, nil
		},
	})

	recorder := doRequest(t, handler, http.MethodGet, "/api/usage/retention", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("retention status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	var info RetentionInfo
	decodeJSONBody(t, recorder, &info)
	if info.APIRequestDays != 30 || info.BodyPreviewDays != 3 ||
		info.HostLogDays != 30 || info.ProxyLogDays != 7 {
		t.Fatalf("retention payload = %+v", info)
	}

	// POST is rejected and a nil callback answers 501.
	recorder = doRequest(t, handler, http.MethodPost, "/api/usage/retention", "")
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST retention status = %d, want 405", recorder.Code)
	}
	bare := New(Options{})
	recorder = doRequest(t, bare, http.MethodGet, "/api/usage/retention", "")
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("nil retention callback status = %d, want 501", recorder.Code)
	}
}
