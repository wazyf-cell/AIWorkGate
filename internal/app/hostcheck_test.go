package app

import "testing"

func TestAllowedRequestHostBlocksRebinding(t *testing.T) {
	allowed := []string{"localhost", "localhost:7865", "127.0.0.1", "127.0.0.1:7865", "[::1]:7865", "::1"}
	for _, host := range allowed {
		if !allowedRequestHost(host, false, nil) {
			t.Errorf("local mode rejected loopback host %q", host)
		}
	}
	rejected := []string{"evil.example:7865", "evil.example", "192.168.1.10:7865", ""}
	for _, host := range rejected {
		if allowedRequestHost(host, false, nil) {
			t.Errorf("local mode accepted non-loopback host %q", host)
		}
	}
}

func TestAllowedRequestHostLANAcceptsOnlyIPLiterals(t *testing.T) {
	if !allowedRequestHost("192.168.1.10:7865", true, nil) {
		t.Error("LAN mode rejected a LAN IP literal")
	}
	if !allowedRequestHost("[fd00::5]:7865", true, nil) {
		t.Error("LAN mode rejected an IPv6 literal")
	}
	if allowedRequestHost("evil.example:7865", true, nil) {
		t.Error("LAN mode accepted a DNS name; rebinding is still possible")
	}
}

func TestAllowedRequestHostPublicHosts(t *testing.T) {
	hosts := []string{"ai.example.com"}
	if !allowedRequestHost("ai.example.com", false, hosts) {
		t.Fatalf("configured public host must pass")
	}
	if !allowedRequestHost("ai.example.com:443", false, hosts) {
		t.Fatalf("port must be ignored")
	}
	if allowedRequestHost("evil.example.com", false, hosts) {
		t.Fatalf("unlisted host must still be rejected")
	}
	if allowedRequestHost("ai.example.com", false, nil) {
		t.Fatalf("without the allowlist a public host stays rejected")
	}
}
