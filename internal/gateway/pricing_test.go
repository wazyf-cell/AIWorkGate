package gateway

import (
	"math"
	"net/http"
	"testing"

	"aiworkgate/internal/model"
)

func TestLogComputesConfiguredCost(t *testing.T) {
	srv, _ := newUpstreamServer(t, jsonResponder(http.StatusOK,
		`{"model":"gpt-served","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":1000,"completion_tokens":500,"total_tokens":1500}}`))
	logs := &fakeLogSink{}
	reg := newFakeRegistry().add(pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "tok")
	g := newTestGateway(t, Options{
		Plugins: reg,
		Log:     logs,
		PriceFor: func(model string) (Price, string, bool) {
			if model == "gpt-served" {
				return Price{Input: 2, Output: 8, CacheRead: 1, Multiplier: 0.8}, "gpt-served", true
			}
			return Price{}, "", false
		},
	})
	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p/chat","messages":[{"role":"user","content":"hi"}]}`))
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
	reg := newFakeRegistry().add(pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "tok")
	g := newTestGateway(t, Options{Plugins: reg, Log: logs})
	serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p/chat","messages":[{"role":"user","content":"hi"}]}`))
	row := logs.wait(t, 1)[0]
	if row.CostUSD != nil || row.PricingModel != "" {
		t.Fatalf("cost = %v pricing = %q, want empty", row.CostUSD, row.PricingModel)
	}
}

// D-30③: a model with its own price is never covered by the pool rate.
func TestPoolRateDoesNotCoverModelPrice(t *testing.T) {
	srv, _ := newUpstreamServer(t, jsonResponder(http.StatusOK,
		`{"model":"m","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":1000000,"completion_tokens":1000000,"total_tokens":2000000}}`))
	logs := &fakeLogSink{}
	reg := newFakeRegistry().add(pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "tok")
	g := newTestGateway(t, Options{
		Plugins: reg,
		Log:     logs,
		PriceFor: func(string) (Price, string, bool) {
			// model price 2/8 with pool rate 2 and model multiplier 0.5:
			// the pool rate must NOT cover the model price.
			return Price{Input: 2, Output: 8, PoolRate: 2, Multiplier: 0.5}, "m", true
		},
	})
	serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p/chat","messages":[{"role":"user","content":"hi"}]}`))
	row := logs.wait(t, 1)[0]
	if row.CostUSD == nil {
		t.Fatal("no cost recorded")
	}
	want := (1000000*2 + 1000000*8) / 1000000.0 * 0.5
	if math.Abs(*row.CostUSD-want) > 1e-9 {
		t.Fatalf("cost = %v, want %v (pool rate 2 must not cover the model price)", *row.CostUSD, want)
	}
}

// D-30③: with no model-level price, the pool rate multiplies the (zero) base.
func TestPoolRateMultipliesUnpricedModel(t *testing.T) {
	srv, _ := newUpstreamServer(t, jsonResponder(http.StatusOK,
		`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":1000000,"completion_tokens":0,"total_tokens":1000000}}`))
	logs := &fakeLogSink{}
	reg := newFakeRegistry().add(pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "tok")
	g := newTestGateway(t, Options{
		Plugins: reg,
		Log:     logs,
		PriceFor: func(string) (Price, string, bool) {
			return Price{PoolRate: 2}, "m", true
		},
	})
	serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p/chat","messages":[{"role":"user","content":"hi"}]}`))
	row := logs.wait(t, 1)[0]
	if row.CostUSD == nil || *row.CostUSD != 0 {
		t.Fatalf("cost = %v, want 0 (no base price to multiply)", row.CostUSD)
	}
}
