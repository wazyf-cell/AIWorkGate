package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// D-40: the allowlist runs before authentication, accepts exact IPs and CIDRs,
// fails safe to loopback when empty, and never trusts X-Forwarded-For.
func TestAllowIPsGateBeforeAuth(t *testing.T) {
	cases := []struct {
		name   string
		allow  []string
		remote string
		xff    string
		want   int
	}{
		{"nil means unrestricted", nil, "10.0.0.5:1000", "", http.StatusUnauthorized},
		{"loopback allowed", []string{"127.0.0.1"}, "127.0.0.1:1000", "", http.StatusUnauthorized},
		{"remote denied", []string{"127.0.0.1"}, "10.0.0.5:1000", "", http.StatusForbidden},
		{"cidr allowed", []string{"192.168.0.0/16"}, "192.168.1.7:1000", "", http.StatusUnauthorized},
		{"spoofed xff ignored", []string{"127.0.0.1"}, "10.0.0.5:1000", "127.0.0.1", http.StatusForbidden},
		{"empty fails safe", []string{}, "10.0.0.5:1000", "", http.StatusForbidden},
		{"invalid entries fail safe", []string{"not-an-ip"}, "10.0.0.5:1000", "", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newTestGateway(t, Options{AllowIPs: tc.allow})
			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			req.RemoteAddr = tc.remote
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			rec := serve(g, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d (%s), want %d", rec.Code, rec.Body.String(), tc.want)
			}
		})
	}
}

func TestTrustedProxyClientIP(t *testing.T) {
	cases := []struct {
		name    string
		allow   []string
		trusted []string
		remote  string
		xff     string
		want    int
	}{
		{
			name: "trusted peer xff wins", allow: []string{"10.0.0.5"}, trusted: []string{"127.0.0.1"},
			remote: "127.0.0.1:1000", xff: "10.0.0.5", want: http.StatusUnauthorized,
		},
		{
			name: "trusted peer xff denied", allow: []string{"127.0.0.1"}, trusted: []string{"127.0.0.1"},
			remote: "127.0.0.1:1000", xff: "10.0.0.5", want: http.StatusForbidden,
		},
		{
			name: "chain walks right to left", allow: []string{"1.2.3.4"}, trusted: []string{"127.0.0.1"},
			remote: "127.0.0.1:1000", xff: "9.9.9.9, 1.2.3.4, 127.0.0.1", want: http.StatusUnauthorized,
		},
		{
			name: "trusted cidr", allow: []string{"10.0.0.5"}, trusted: []string{"172.16.0.0/12"},
			remote: "172.17.0.2:1000", xff: "10.0.0.5", want: http.StatusUnauthorized,
		},
		{
			name: "untrusted peer ignores xff", allow: []string{"127.0.0.1"}, trusted: []string{"127.0.0.1"},
			remote: "10.0.0.5:1000", xff: "127.0.0.1", want: http.StatusForbidden,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newTestGateway(t, Options{AllowIPs: tc.allow, TrustedProxies: tc.trusted})
			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			req.RemoteAddr = tc.remote
			req.Header.Set("X-Forwarded-For", tc.xff)
			if rec := serve(g, req); rec.Code != tc.want {
				t.Fatalf("status = %d (%s), want %d", rec.Code, rec.Body.String(), tc.want)
			}
		})
	}
}

func TestSetAllowIPsTakesEffectLive(t *testing.T) {
	g := newTestGateway(t, Options{AllowIPs: []string{"127.0.0.1"}})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.RemoteAddr = "192.168.1.7:1000"
	if rec := serve(g, req); rec.Code != http.StatusForbidden {
		t.Fatalf("before update = %d, want 403", rec.Code)
	}
	g.SetAllowIPs([]string{"192.168.0.0/16"})
	if rec := serve(g, req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("after update = %d, want 401 (passed the gate)", rec.Code)
	}
}

func TestKeyAllowsIP(t *testing.T) {
	g := newTestGateway(t, Options{})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.RemoteAddr = "192.168.1.7:1000"
	if !g.keyAllowsIP(req, nil) {
		t.Fatalf("empty allowed_ips must not restrict")
	}
	if !g.keyAllowsIP(req, []string{"192.168.0.0/16"}) {
		t.Fatalf("cidr should admit the address")
	}
	if g.keyAllowsIP(req, []string{"10.0.0.0/8"}) {
		t.Fatalf("outside cidr must be refused")
	}
}
