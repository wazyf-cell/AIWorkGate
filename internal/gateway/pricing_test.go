package gateway

import (
	"math"
	"net/http"
	"testing"
)

func TestLogComputesConfiguredCost(t *testing.T) {
	srv, _ := newUpstreamServer(t, jsonResponder(http.StatusOK,
		`{"model":"gpt-served","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":1000,"completion_tokens":500,"total_tokens":1500}}`))
	logs := &fakeLogSink{}
	g := newTestGateway(t, Options{
		A:   &fakeA{base: srv.URL, key: "sk-a"},
		Log: logs,
		PriceFor: func(model string) (Price, string, bool) {
			if model == "gpt-served" {
				return Price{Input: 2, Output: 8, CacheRead: 1, Multiplier: 0.8}, "gpt-served", true
			}
			return Price{}, "", false
		},
	})
	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rr.Code, rr.Body.String())
	}
	row := logs.wait(t, 1)[0]
	if row.PricingModel != "gpt-served" || row.CostUSD == nil {
		t.Fatalf("cost = %v pricing = %q", row.CostUSD, row.PricingModel)
	}
	want := (1000*2 + 500*8) / 1_000_000.0 * 0.8
	if math.Abs(*row.CostUSD-want) > 1e-9 {
		t.Fatalf("cost = %v, want %v", *row.CostUSD, want)
	}
}

func TestLogLeavesCostEmptyWithoutPrice(t *testing.T) {
	srv, _ := newUpstreamServer(t, jsonResponder(http.StatusOK,
		`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	logs := &fakeLogSink{}
	g := newTestGateway(t, Options{A: &fakeA{base: srv.URL, key: "sk-a"}, Log: logs})
	serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	row := logs.wait(t, 1)[0]
	if row.CostUSD != nil || row.PricingModel != "" {
		t.Fatalf("cost = %v pricing = %q, want empty", row.CostUSD, row.PricingModel)
	}
}
