package egress

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"aiworkgate/internal/model"
)

// startDirectListener binds a direct-egress listener with one identity and the
// rows it writes handed to c.
func startDirectListener(t *testing.T, id Identity, c *collector) string {
	t.Helper()
	fw := &Forwarder{dialTimeout: 3 * time.Second, handshakeTimeout: 3 * time.Second}
	local, err := fw.StartDirect(id, c.sink)
	if err != nil {
		t.Fatalf("start direct listener: %v", err)
	}
	t.Cleanup(func() { _ = fw.Stop() })
	return local
}

func directRowChecks(t *testing.T, row model.ProxyLog, wantResult string) {
	t.Helper()
	// Literal values, not the constants: proxy_id is what the log page filters
	// on and what a direct row is recognised by, so it is a wire format.
	if row.ProxyID != "direct" {
		t.Fatalf("proxy_id = %q, want the fixed %q", row.ProxyID, "direct")
	}
	if row.ProxyLabel != "直连" {
		t.Fatalf("proxy_label = %q, want %q", row.ProxyLabel, "直连")
	}
	if row.Result != wantResult {
		t.Fatalf("result = %q, want %q (row %+v)", row.Result, wantResult, row)
	}
}

// TestDirectListenerDialsTargetItself is the core shape guard: a direct listener
// is the CONNECT listener of the target, not of an upstream proxy.
func TestDirectListenerDialsTargetItself(t *testing.T) {
	echo := newEchoServer(t)
	// A live fake proxy that must never be touched.
	upstream := newFakeProxy(t, "")
	c := &collector{}

	local := startDirectListener(t, Identity{PluginID: "trae", AccountID: "acc_direct"}, c)
	if addr, _ := proxyURLParts(t, local); !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Fatalf("direct listener must bind loopback, got %q", local)
	}

	resp, err := newTunnelClient(t, local).Get(echo.URL() + "/direct")
	if err != nil {
		t.Fatalf("GET through direct listener: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "echo-ok" {
		t.Fatalf("body = %q, want echo-ok", body)
	}
	if n := echo.ConnCount(); n != 1 {
		t.Fatalf("origin saw %d connections, want 1 opened by the listener itself", n)
	}
	if targets := upstream.Targets(); len(targets) != 0 {
		t.Fatalf("direct listener dialled an upstream proxy: %v", targets)
	}

	row := c.wait(t, 1)[0]
	directRowChecks(t, row, model.ProxyResultOK)
}

func TestDirectListenerLogsSuccessfulTunnel(t *testing.T) {
	echo := newEchoServer(t)
	c := &collector{}
	local := startDirectListener(t, Identity{PluginID: "gemini", AccountID: "acc_9"}, c)

	resp, err := newTunnelClient(t, local).Get(echo.URL() + "/record-me")
	if err != nil {
		t.Fatalf("GET through direct listener: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	row := c.wait(t, 1)[0]
	directRowChecks(t, row, model.ProxyResultOK)
	if row.PluginID != "gemini" || row.AccountID != "acc_9" {
		t.Fatalf("row lost its identity: %+v", row)
	}
	host, portStr, err := net.SplitHostPort(echo.Addr())
	if err != nil {
		t.Fatalf("echo addr: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("echo port: %v", err)
	}
	if row.TargetHost != host || row.TargetPort != port {
		t.Fatalf("target = %q:%d, want %s:%s", row.TargetHost, row.TargetPort, host, portStr)
	}
	if row.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200", row.Status)
	}
	if row.BytesUp <= 0 || row.BytesDown <= 0 {
		t.Fatalf("bytes = up %d down %d, want both counted", row.BytesUp, row.BytesDown)
	}
	if row.TS == "" || row.At <= 0 || row.DurationMS < 0 || row.Error != "" {
		t.Fatalf("row lacks timestamp/duration or carries an error: %+v", row)
	}
}

// TestDirectListenerLogsFailedDial proves a failed direct dial is recorded as
// "failed" (never "denied": nobody refused, nothing answered).
func TestDirectListenerLogsFailedDial(t *testing.T) {
	dead := freePort(t)
	c := &collector{}
	local := startDirectListener(t, Identity{PluginID: "gemini", AccountID: "acc_dead"}, c)

	deadURL := "http://127.0.0.1:" + strconv.Itoa(dead)
	if _, err := newTunnelClient(t, local).Get(deadURL + "/never"); err == nil {
		t.Fatal("expected the direct dial of a closed port to fail")
	}

	row := c.wait(t, 1)[0]
	directRowChecks(t, row, model.ProxyResultFailed)
	if row.Status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", row.Status)
	}
	if row.PluginID != "gemini" || row.AccountID != "acc_dead" {
		t.Fatalf("failed row lost its identity: %+v", row)
	}
	if row.TargetHost != "127.0.0.1" || row.TargetPort != dead {
		t.Fatalf("target = %q:%d, want 127.0.0.1:%d", row.TargetHost, row.TargetPort, dead)
	}
	if !strings.Contains(row.Error, "direct dial") {
		t.Fatalf("error = %q, want it to name the direct dial", row.Error)
	}
	if row.BytesUp != 0 || row.BytesDown != 0 {
		t.Fatalf("a failed dial must not count bytes, got up %d down %d", row.BytesUp, row.BytesDown)
	}
}

// TestDirectListenerLogsDeniedTunnels keeps the refusal rows identical to the
// proxy listener's, so one log page explains both.
func TestDirectListenerLogsDeniedTunnels(t *testing.T) {
	t.Run("non CONNECT is denied", func(t *testing.T) {
		c := &collector{}
		local := startDirectListener(t, Identity{PluginID: "gemini", AccountID: "acc_x"}, c)
		rawAddr, _ := proxyURLParts(t, local)
		raw, err := net.DialTimeout("tcp", rawAddr, 2*time.Second)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer func() { _ = raw.Close() }()
		_ = raw.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := io.WriteString(raw, "GET /v1/models HTTP/1.1\r\nHost: example.com\r\n\r\n"); err != nil {
			t.Fatalf("write: %v", err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(raw), nil)
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", resp.StatusCode)
		}

		row := c.wait(t, 1)[0]
		directRowChecks(t, row, model.ProxyResultDenied)
		if row.Status != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", row.Status)
		}
		if row.PluginID != "gemini" || row.AccountID != "acc_x" {
			t.Fatalf("denied row lost its identity: %+v", row)
		}
	})

	t.Run("target without a port is denied", func(t *testing.T) {
		c := &collector{}
		local := startDirectListener(t, Identity{}, c)
		rawAddr, rawAuth := proxyURLParts(t, local)
		raw, err := net.DialTimeout("tcp", rawAddr, 2*time.Second)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer func() { _ = raw.Close() }()
		_ = raw.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := fmt.Fprintf(raw, "CONNECT example.com HTTP/1.1\r\nHost: example.com\r\nProxy-Authorization: %s\r\n\r\n", rawAuth); err != nil {
			t.Fatalf("write: %v", err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(raw), nil)
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
		row := c.wait(t, 1)[0]
		directRowChecks(t, row, model.ProxyResultDenied)
		if row.Status != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", row.Status)
		}
	})
}

// TestDirectListenerSinkFailureKeepsTunnel is the fail-open guard required of
// direct mode: a recording failure must never change what the client experiences,
// and it must not take the listener down with it.
func TestDirectListenerSinkFailureKeepsTunnel(t *testing.T) {
	echo := newEchoServer(t)
	fw := &Forwarder{dialTimeout: 3 * time.Second, handshakeTimeout: 3 * time.Second}
	local, err := fw.StartDirect(Identity{PluginID: "gemini", AccountID: "acc_panic"}, func(model.ProxyLog) {
		panic("sink exploded while writing proxy_logs")
	})
	if err != nil {
		t.Fatalf("start direct listener: %v", err)
	}
	t.Cleanup(func() { _ = fw.Stop() })

	client := newTunnelClient(t, local)
	for i := 0; i < 2; i++ {
		resp, err := client.Get(echo.URL() + "/still-works")
		if err != nil {
			t.Fatalf("request %d through the direct listener: %v", i+1, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if string(body) != "echo-ok" {
			t.Fatalf("request %d body = %q, want echo-ok", i+1, body)
		}
	}
	if n := echo.ConnCount(); n != 2 {
		t.Fatalf("origin saw %d connections, want 2", n)
	}
}

// TestManagerDirectListenersPerAccount covers the attribution requirement: one
// listener per (plugin, account, direct), rows carrying that account and the
// fixed "direct" identity, and nothing bound while the switch is off.
func TestManagerDirectListenersPerAccount(t *testing.T) {
	echo := newEchoServer(t)
	st := &fakeStore{
		assignments: []model.AccountEgress{
			{PluginID: "plug", AccountID: "acc_1", Mode: model.EgressDirect},
			{PluginID: "plug", AccountID: "acc_2", Mode: model.EgressDirect},
		},
	}
	c := &collector{}
	m := NewManager(st)
	m.SetLogSink(c.sink)
	t.Cleanup(func() { _ = m.CloseAll() })

	if err := m.StartEnabled(); err != nil {
		t.Fatalf("StartEnabled: %v", err)
	}
	// Off is the shipped default: no listener, no ports, no rows.
	if u, ok := m.DirectURLFor("plug", "acc_1"); ok {
		t.Fatalf("direct listener created while direct logging is off: %q", u)
	}

	if err := m.SetDirectLogging(true); err != nil {
		t.Fatalf("SetDirectLogging: %v", err)
	}
	url1, ok := m.DirectURLFor("plug", "acc_1")
	if !ok || url1 == "" {
		t.Fatalf("acc_1 has no direct listener (url %q, ok %v)", url1, ok)
	}
	if again, _ := m.DirectURLFor("plug", "acc_1"); again != url1 {
		t.Fatalf("second ask returned %q, want the same listener %q", again, url1)
	}
	url2, ok := m.DirectURLFor("plug", "acc_2")
	if !ok || url2 == "" {
		t.Fatalf("acc_2 has no direct listener")
	}
	if url1 == url2 {
		t.Fatal("two accounts share one direct listener; traffic cannot be attributed")
	}
	// The plugin-wide listener is a third one, keyed by an empty account.
	urlDefault, ok := m.DirectURLFor("plug", "")
	if !ok || urlDefault == url1 || urlDefault == url2 {
		t.Fatalf("plugin-wide direct listener = %q (ok %v), want its own port", urlDefault, ok)
	}

	for _, url := range []string{url1, url2} {
		resp, err := newTunnelClient(t, url).Get(echo.URL() + "/direct-row")
		if err != nil {
			t.Fatalf("GET via %s: %v", url, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	rows := c.wait(t, 2)
	seen := map[string]string{}
	for _, row := range rows {
		directRowChecks(t, row, model.ProxyResultOK)
		if row.PluginID != "plug" {
			t.Fatalf("row lost its plugin: %+v", row)
		}
		seen[row.AccountID] = row.ProxyID
	}
	if seen["acc_1"] != DirectProxyID || seen["acc_2"] != DirectProxyID {
		t.Fatalf("per-account direct rows = %+v", seen)
	}

	// StartEnabled re-reads the proxy table; it must not sweep the synthetic
	// direct listeners, which are not rows in that table.
	if err := m.StartEnabled(); err != nil {
		t.Fatalf("second StartEnabled: %v", err)
	}
	if again, ok := m.DirectURLFor("plug", "acc_1"); !ok || again != url1 {
		t.Fatalf("StartEnabled dropped the direct listener: %q (ok %v), want %q", again, ok, url1)
	}
}

// TestManagerSetDirectLoggingOffStopsListeners keeps the switch honest: turning
// accounting off must actually close the ports plugins were dialling.
func TestManagerSetDirectLoggingOffStopsListeners(t *testing.T) {
	st := &fakeStore{}
	m := NewManager(st)
	t.Cleanup(func() { _ = m.CloseAll() })

	if err := m.SetDirectLogging(true); err != nil {
		t.Fatalf("SetDirectLogging(on): %v", err)
	}
	url, ok := m.DirectURLFor("plug", "acc_1")
	if !ok {
		t.Fatal("no direct listener after enabling logging")
	}
	if !m.DirectLoggingEnabled() {
		t.Fatal("DirectLoggingEnabled = false after enabling")
	}

	if err := m.SetDirectLogging(false); err != nil {
		t.Fatalf("SetDirectLogging(off): %v", err)
	}
	if m.DirectLoggingEnabled() {
		t.Fatal("DirectLoggingEnabled = true after disabling")
	}
	if again, ok := m.DirectURLFor("plug", "acc_1"); ok {
		t.Fatalf("a stopped switch still hands out %q", again)
	}
	rawAddr, _ := proxyURLParts(t, url)
	raw, err := net.DialTimeout("tcp", rawAddr, time.Second)
	if err == nil {
		_ = raw.Close()
		t.Fatalf("direct listener %s is still accepting connections after switching off", url)
	}
}

// TestManagerStartProxyRejectsReservedDirectID keeps a user-created proxy from
// stealing the identity of the synthetic slot.
func TestManagerStartProxyRejectsReservedDirectID(t *testing.T) {
	m := NewManager(&fakeStore{})
	t.Cleanup(func() { _ = m.CloseAll() })

	err := m.StartProxy(model.Proxy{ID: DirectProxyID, Name: "冒充直连", Type: "http", Host: "127.0.0.1", Port: 8080, Enabled: true})
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("StartProxy(direct) error = %v, want it rejected as reserved", err)
	}
	if _, ok := m.LocalURLFor("plug", "acc_1", DirectProxyID); ok {
		t.Fatal("a proxy claiming the direct id must not get a listener")
	}
}

// TestDirectListenerRejectsInconsistentProxyConfig documents that a direct
// listener needs no host/port, and that a bogus one is not silently used.
func TestDirectListenerNeedsNoUpstreamConfig(t *testing.T) {
	c := &collector{}
	fw := &Forwarder{dialTimeout: 3 * time.Second, handshakeTimeout: 3 * time.Second}
	local, err := fw.StartFor(model.Proxy{ID: DirectProxyID, Name: DirectProxyLabel, Type: ProxyTypeDirect},
		Identity{PluginID: "plug", AccountID: "acc_1"}, c.sink)
	if err != nil {
		t.Fatalf("a direct listener must start without host/port: %v", err)
	}
	t.Cleanup(func() { _ = fw.Stop() })

	echo := newEchoServer(t)
	if _, err := newTunnelClient(t, local).Get(echo.URL() + "/no-upstream"); err != nil {
		t.Fatalf("GET through direct listener: %v", err)
	}
	row := c.wait(t, 1)[0]
	directRowChecks(t, row, model.ProxyResultOK)
}
