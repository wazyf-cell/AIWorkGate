package egress

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"aiworkgate/internal/model"
)

func TestCheckParsesExitIPAndUsesConnect(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "json", body: `{"ip":"198.51.100.7"}`, want: "198.51.100.7"},
		{name: "plain", body: "203.0.113.9\n", want: "203.0.113.9"},
		{name: "quoted", body: `"203.0.113.10"`, want: "203.0.113.10"},
		{name: "ipv6 json", body: `{"ip":"2001:db8::1"}`, want: "2001:db8::1"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				_, _ = io.WriteString(w, tc.body)
			}))
			defer echo.Close()
			upstream := newFakeProxy(t, "")

			proxy := model.Proxy{
				ID:      "px_check",
				Type:    "http",
				Host:    "127.0.0.1",
				Port:    upstream.Port(),
				Enabled: true,
			}
			ip, latency, err := Check(context.Background(), proxy, echo.URL, 5*time.Second)
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if ip != tc.want {
				t.Fatalf("exit ip = %q, want %q", ip, tc.want)
			}
			if latency <= 0 {
				t.Fatalf("latency = %v, want > 0", latency)
			}
			targets := upstream.Targets()
			if len(targets) != 1 {
				t.Fatalf("upstream CONNECT count = %d, want 1", len(targets))
			}
		})
	}
}

func TestCheckFailsOnDeadProxy(t *testing.T) {
	deadPort := freePort(t)
	start := time.Now()
	_, _, err := Check(context.Background(), model.Proxy{
		ID:      "px_dead",
		Type:    "http",
		Host:    "127.0.0.1",
		Port:    deadPort,
		Enabled: true,
	}, "http://127.0.0.1:1/", 3*time.Second)
	if err == nil {
		t.Fatal("expected Check to fail with a dead upstream proxy")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Check took %v, timeout not enforced", elapsed)
	}
}

func TestCheckFailsOnUpstreamAuthRequired(t *testing.T) {
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ip":"198.51.100.7"}`)
	}))
	defer echo.Close()
	upstream := newFakeProxy(t, basicAuth("u", "p"))

	// No credentials configured: the upstream must reject CONNECT.
	_, _, err := Check(context.Background(), model.Proxy{
		ID:      "px_noauth",
		Type:    "http",
		Host:    "127.0.0.1",
		Port:    upstream.Port(),
		Enabled: true,
	}, echo.URL, 3*time.Second)
	if err == nil {
		t.Fatal("expected Check to fail when upstream requires auth")
	}
}

func TestCheckUsesDefaultIPEchoURLConstant(t *testing.T) {
	if DefaultIPEchoURL != "https://api.ipify.org?format=json" {
		t.Fatalf("DefaultIPEchoURL = %q", DefaultIPEchoURL)
	}
}

func TestParseExitIPRejectsGarbage(t *testing.T) {
	for _, body := range []string{"", "   ", "<html>oops</html>", `{"origin":"1.2.3.4"}`, `not json {`} {
		if ip, err := parseExitIP([]byte(body)); err == nil {
			t.Fatalf("parseExitIP(%q) = %q, want error", body, ip)
		}
	}
}
