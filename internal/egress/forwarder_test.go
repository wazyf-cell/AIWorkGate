package egress

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"aiworkgate/internal/model"
)

func TestForwarderTunnelsThroughHTTPProxy(t *testing.T) {
	echo := newEchoServer(t)
	expectedAuth := basicAuth("alice", "s3cret")
	upstream := newFakeProxy(t, expectedAuth)

	fw, local := startForwarder(t, model.Proxy{
		ID:       "px_1",
		Name:     "test proxy",
		Type:     "http",
		Host:     "127.0.0.1",
		Port:     upstream.Port(),
		Username: "alice",
		Password: "s3cret",
		Enabled:  true,
	})

	if addr, _ := proxyURLParts(t, local); !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Fatalf("local url must bind loopback, got %q", local)
	}
	if got := fw.LocalURL(); got != local {
		t.Fatalf("LocalURL() = %q, want %q", got, local)
	}

	resp, err := newTunnelClient(t, local).Get(echo.URL() + "/hello")
	if err != nil {
		t.Fatalf("GET through forwarder: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "echo-ok" {
		t.Fatalf("body = %q, want echo-ok", body)
	}

	if got := echo.ConnCount(); got != 1 {
		t.Fatalf("echo saw %d connections, want exactly 1 (through proxy)", got)
	}
	targets := upstream.Targets()
	if len(targets) != 1 || targets[0] != echo.Addr() {
		t.Fatalf("upstream CONNECT targets = %v, want [%s]", targets, echo.Addr())
	}
	auths := upstream.AuthHeaders()
	if len(auths) != 1 || auths[0] != expectedAuth {
		t.Fatalf("upstream Proxy-Authorization = %v, want [%s]", auths, expectedAuth)
	}

	// Non-CONNECT requests must be rejected with 405 and must not be proxied.
	rawAddr, _ := proxyURLParts(t, local)
	raw, err := net.DialTimeout("tcp", rawAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial forwarder: %v", err)
	}
	defer func() { _ = raw.Close() }()
	_ = raw.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(raw, "GET /v1/models HTTP/1.1\r\nHost: example.com\r\n\r\n"); err != nil {
		t.Fatalf("write plain GET: %v", err)
	}
	gotResp, err := http.ReadResponse(bufio.NewReader(raw), nil)
	if err != nil {
		t.Fatalf("read 405 response: %v", err)
	}
	_, _ = io.Copy(io.Discard, gotResp.Body)
	_ = gotResp.Body.Close()
	if gotResp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("non-CONNECT status = %d, want 405", gotResp.StatusCode)
	}
	if got := echo.ConnCount(); got != 1 {
		t.Fatalf("echo saw %d connections after rejected request, want 1", got)
	}
}

// TestForwarderAcceptsProxyURLTransport is the integration shape every plugin
// uses: http.ProxyURL(egress.json url) with the token in the userinfo. It must
// authenticate without any plugin-side code, so this test goes through the real
// transport rather than the test dialer.
func TestForwarderAcceptsProxyURLTransport(t *testing.T) {
	upstream := newFakeProxy(t, "")
	_, local := startForwarder(t, model.Proxy{
		ID:      "px_url",
		Type:    "http",
		Host:    "127.0.0.1",
		Port:    upstream.Port(),
		Enabled: true,
	})
	proxyURL, err := url.Parse(local)
	if err != nil {
		t.Fatalf("parse listener url: %v", err)
	}

	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "through-proxy")
	}))
	defer origin.Close()

	client := &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
	defer client.CloseIdleConnections()

	resp, err := client.Get(origin.URL)
	if err != nil {
		t.Fatalf("GET through http.ProxyURL: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "through-proxy" {
		t.Fatalf("status = %d body = %q", resp.StatusCode, body)
	}
	for _, h := range upstream.AuthHeaders() {
		if h != "" {
			t.Fatalf("upstream proxy received credentials it was never configured with: %q", h)
		}
	}
}

// TestForwarderRequiresListenerCredentials pins the loopback boundary: the
// listener is reachable by any local process that finds the port, so the token
// embedded in the URL is the only thing separating a plugin from whoever else
// runs on the machine.
func TestForwarderRequiresListenerCredentials(t *testing.T) {
	echo := newEchoServer(t)
	upstream := newFakeProxy(t, "")
	_, local := startForwarder(t, model.Proxy{
		ID:      "px_auth",
		Type:    "http",
		Host:    "127.0.0.1",
		Port:    upstream.Port(),
		Enabled: true,
	})
	addr, _ := proxyURLParts(t, local)

	for name, header := range map[string]string{
		"missing":  "",
		"wrong":    basicAuth("not-the-token", ""),
		"upstream": basicAuth("alice", "s3cret"),
	} {
		t.Run(name, func(t *testing.T) {
			conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
			if err != nil {
				t.Fatalf("dial listener: %v", err)
			}
			defer func() { _ = conn.Close() }()
			_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
			request := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", echo.Addr(), echo.Addr())
			if header != "" {
				request += "Proxy-Authorization: " + header + "\r\n"
			}
			request += "\r\n"
			if _, err := io.WriteString(conn, request); err != nil {
				t.Fatalf("write CONNECT: %v", err)
			}
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatalf("read response: %v", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusProxyAuthRequired {
				t.Fatalf("status = %d, want 407", resp.StatusCode)
			}
		})
	}

	if n := echo.ConnCount(); n != 0 {
		t.Fatalf("origin saw %d connections from an unauthorized client, want 0", n)
	}
	if n := len(upstream.Targets()); n != 0 {
		t.Fatalf("upstream proxy saw %d CONNECTs, want 0", n)
	}
}

func TestForwarderFailsClosedOnDeadUpstream(t *testing.T) {
	echo := newEchoServer(t)
	deadPort := freePort(t)

	_, local := startForwarder(t, model.Proxy{
		ID:      "px_dead",
		Type:    "http",
		Host:    "127.0.0.1",
		Port:    deadPort,
		Enabled: true,
	})

	_, err := newTunnelClient(t, local).Get(echo.URL() + "/never")
	if err == nil {
		t.Fatal("expected request to fail when upstream proxy is dead")
	}
	if !strings.Contains(err.Error(), "502") && !strings.Contains(err.Error(), "CONNECT") {
		t.Logf("unexpected error text (still a failure, which is what matters): %v", err)
	}
	if n := echo.ConnCount(); n != 0 {
		t.Fatalf("echo saw %d direct connections; fail-closed violated", n)
	}
}

func TestForwarderNoFallbackWhenUpstreamDies(t *testing.T) {
	echo := newEchoServer(t)
	upstream := newFakeProxy(t, "")

	_, local := startForwarder(t, model.Proxy{
		ID:      "px_live",
		Type:    "http",
		Host:    "127.0.0.1",
		Port:    upstream.Port(),
		Enabled: true,
	})
	client := newTunnelClient(t, local)

	resp, err := client.Get(echo.URL() + "/first")
	if err != nil {
		t.Fatalf("first request through live upstream: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if n := echo.ConnCount(); n != 1 {
		t.Fatalf("echo connections after first request = %d, want 1", n)
	}

	upstream.Close()

	if _, err := client.Get(echo.URL() + "/second"); err == nil {
		t.Fatal("expected request to fail after upstream proxy stopped")
	}
	if n := echo.ConnCount(); n != 1 {
		t.Fatalf("echo saw %d connections after upstream died; direct fallback occurred", n)
	}
}

func TestForwarderStopClosesActiveTunnels(t *testing.T) {
	echo := newEchoServer(t)
	upstream := newFakeProxy(t, "")

	fw, local := startForwarder(t, model.Proxy{
		ID:      "px_stop",
		Type:    "http",
		Host:    "127.0.0.1",
		Port:    upstream.Port(),
		Enabled: true,
	})

	rawAddr, rawAuth := proxyURLParts(t, local)
	conn, err := net.DialTimeout("tcp", rawAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial forwarder: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: %s\r\n\r\n",
		echo.Addr(), echo.Addr(), rawAuth); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	br := bufio.NewReader(conn)
	code, statusLine, err := readConnectResponse(br)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if code != http.StatusOK {
		t.Fatalf("CONNECT status %q, want 200", statusLine)
	}

	stopDone := make(chan error, 1)
	go func() { stopDone <- fw.Stop() }()
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stop hung waiting for an active tunnel")
	}

	if got := fw.LocalURL(); got != "" {
		t.Fatalf("LocalURL() after Stop = %q, want empty", got)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := br.ReadByte(); err == nil {
		t.Fatal("tunnel still readable after Stop")
	}
	if err := fw.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func TestForwarderStartValidation(t *testing.T) {
	upstream := newFakeProxy(t, "")

	var fw Forwarder
	if _, err := fw.Start(model.Proxy{Type: "ftp", Host: "127.0.0.1", Port: upstream.Port(), Enabled: true}); err == nil {
		t.Fatal("expected unsupported proxy type error")
	}
	if _, err := fw.Start(model.Proxy{Type: "http", Host: "", Port: upstream.Port(), Enabled: true}); err == nil {
		t.Fatal("expected empty host error")
	}
	if _, err := fw.Start(model.Proxy{Type: "http", Host: "127.0.0.1", Port: 0, Enabled: true}); err == nil {
		t.Fatal("expected port range error")
	}
	if _, err := fw.Start(model.Proxy{Type: "http", Host: "127.0.0.1", Port: upstream.Port(), Enabled: true}); err != nil {
		t.Fatalf("valid start: %v", err)
	}
	if _, err := fw.Start(model.Proxy{Type: "http", Host: "127.0.0.1", Port: upstream.Port(), Enabled: true}); err == nil {
		t.Fatal("expected already-started error")
	}
	if err := fw.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := fw.Start(model.Proxy{Type: "http", Host: "127.0.0.1", Port: upstream.Port(), Enabled: true}); err == nil {
		t.Fatal("expected start-after-stop error")
	}
}

func TestForwarderContextDoesNotFallBackDirect(t *testing.T) {
	// A proxy address that blackholes connections would normally hang; the
	// dial timeout must bound the failure instead of trying the target.
	echo := newEchoServer(t)
	deadPort := freePort(t)
	fw := &Forwarder{dialTimeout: 500 * time.Millisecond, handshakeTimeout: time.Second}
	local, err := fw.Start(model.Proxy{ID: "px_slow", Type: "http", Host: "127.0.0.1", Port: deadPort, Enabled: true})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = fw.Stop() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = dialViaHTTPProxy(ctx, local, echo.Addr())
	if err == nil {
		t.Fatal("expected CONNECT to fail")
	}
	if n := echo.ConnCount(); n != 0 {
		t.Fatalf("echo saw %d direct connections", n)
	}
}
