package gateway

import (
	"net/http"
	"strings"
	"testing"
)

// D-11: paused rejects every /v1 call before authentication; whitelist admits
// only keys marked as the host owner.
func TestServiceModes(t *testing.T) {
	paused := newTestGateway(t, Options{ServiceMode: "paused"})
	rr := serve(paused, newGetRequest(t, "/v1/models", ""))
	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), "service_paused") {
		t.Fatalf("paused = %d (%s), want 503 service_paused", rr.Code, rr.Body.String())
	}

	whitelist := newTestGateway(t, Options{
		ServiceMode: "whitelist",
		KeyRecord:   staticKeyRecord(KeyRecord{Enabled: true, Owner: false}),
	})
	rr = serve(whitelist, newGetRequest(t, "/v1/models", clientKey))
	if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "service_whitelist") {
		t.Fatalf("whitelist non-owner = %d (%s), want 403 service_whitelist", rr.Code, rr.Body.String())
	}

	owner := newTestGateway(t, Options{
		ServiceMode: "whitelist",
		KeyRecord:   staticKeyRecord(KeyRecord{Enabled: true, Owner: true}),
	})
	if rr = serve(owner, newGetRequest(t, "/v1/models", clientKey)); rr.Code != http.StatusOK {
		t.Fatalf("whitelist owner = %d (%s), want 200", rr.Code, rr.Body.String())
	}

	// Owner flag is inert in open mode.
	open := newTestGateway(t, Options{
		ServiceMode: "open",
		KeyRecord:   staticKeyRecord(KeyRecord{Enabled: true, Owner: false}),
	})
	if rr = serve(open, newGetRequest(t, "/v1/models", clientKey)); rr.Code != http.StatusOK {
		t.Fatalf("open mode = %d (%s), want 200", rr.Code, rr.Body.String())
	}
}

func TestSetServiceModeLive(t *testing.T) {
	g := newTestGateway(t, Options{ServiceMode: "open"})
	req := func() int { return serve(g, newGetRequest(t, "/v1/models", clientKey)).Code }
	if req() != http.StatusOK {
		t.Fatalf("open mode did not pass")
	}
	g.SetServiceMode("paused")
	if got := req(); got != http.StatusServiceUnavailable {
		t.Fatalf("after pausing = %d, want 503", got)
	}
	g.SetServiceMode("bogus")
	if got := req(); got != http.StatusOK {
		t.Fatalf("unknown mode should fold to open, got %d", got)
	}
}

func TestKeyTotalRequestCap(t *testing.T) {
	atCap := newTestGateway(t, Options{
		KeyRecord:     staticKeyRecord(KeyRecord{Enabled: true, MaxRequestsTotal: 5}),
		KeyUsageTotal: func(string) (int, error) { return 5, nil },
	})
	rr := serve(atCap, newGetRequest(t, "/v1/models", clientKey))
	if rr.Code != http.StatusTooManyRequests || !strings.Contains(rr.Body.String(), "key_quota_exceeded") {
		t.Fatalf("at cap = %d (%s), want 429", rr.Code, rr.Body.String())
	}

	under := newTestGateway(t, Options{
		KeyRecord:     staticKeyRecord(KeyRecord{Enabled: true, MaxRequestsTotal: 5}),
		KeyUsageTotal: func(string) (int, error) { return 4, nil },
	})
	if rr = serve(under, newGetRequest(t, "/v1/models", clientKey)); rr.Code != http.StatusOK {
		t.Fatalf("under cap = %d (%s), want 200", rr.Code, rr.Body.String())
	}

	unlimited := newTestGateway(t, Options{
		KeyRecord:     staticKeyRecord(KeyRecord{Enabled: true}),
		KeyUsageTotal: func(string) (int, error) { return 1 << 30, nil },
	})
	if rr = serve(unlimited, newGetRequest(t, "/v1/models", clientKey)); rr.Code != http.StatusOK {
		t.Fatalf("unlimited = %d (%s), want 200", rr.Code, rr.Body.String())
	}
}
