package gateway

import (
	"net/http"
	"testing"
)

func TestIPRateLimit(t *testing.T) {
	g := newTestGateway(t, Options{RateLimitPerIPPerMinute: 2})
	do := func() *int {
		code := serve(g, newGetRequest(t, "/v1/models", clientKey)).Code
		return &code
	}
	if c := do(); *c != http.StatusOK {
		t.Fatalf("first = %d, want 200", *c)
	}
	if c := do(); *c != http.StatusOK {
		t.Fatalf("second = %d, want 200", *c)
	}
	rr := serve(g, newGetRequest(t, "/v1/models", clientKey))
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("third = %d, want 429", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatalf("429 must carry Retry-After")
	}
}

func TestKeyRateLimit(t *testing.T) {
	g := newTestGateway(t, Options{RateLimitPerMinute: 1})
	if rr := serve(g, newGetRequest(t, "/v1/models", clientKey)); rr.Code != http.StatusOK {
		t.Fatalf("first = %d, want 200", rr.Code)
	}
	if rr := serve(g, newGetRequest(t, "/v1/models", clientKey)); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("second = %d, want 429", rr.Code)
	}
}

func TestRateLimitsOffByDefault(t *testing.T) {
	g := newTestGateway(t, Options{})
	for i := 0; i < 3; i++ {
		if rr := serve(g, newGetRequest(t, "/v1/models", clientKey)); rr.Code != http.StatusOK {
			t.Fatalf("request %d = %d, want 200", i, rr.Code)
		}
	}
}

func TestSetRateLimitsLive(t *testing.T) {
	g := newTestGateway(t, Options{})
	do := func() int { return serve(g, newGetRequest(t, "/v1/models", clientKey)).Code }
	if do() != http.StatusOK {
		t.Fatalf("off by default")
	}
	g.SetRateLimits(1, 0)
	if do() != http.StatusOK {
		t.Fatalf("first call after enabling the key limit must pass")
	}
	if do() != http.StatusTooManyRequests {
		t.Fatalf("second call after enabling the key limit must be 429")
	}
}
