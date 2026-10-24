package egress

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"aiworkgate/internal/model"
)

// collector gathers the rows one forwarder writes.
type collector struct {
	mu   sync.Mutex
	rows []model.ProxyLog
}

func (c *collector) sink(row model.ProxyLog) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rows = append(c.rows, row)
}

func (c *collector) wait(t *testing.T, n int) []model.ProxyLog {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		got := len(c.rows)
		c.mu.Unlock()
		if got >= n {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.rows) < n {
		t.Fatalf("rows = %d, want %d (%+v)", len(c.rows), n, c.rows)
	}
	out := make([]model.ProxyLog, len(c.rows))
	copy(out, c.rows)
	c.rows = nil
	return out
}

func startLoggingForwarder(t *testing.T, p model.Proxy, id Identity, c *collector) string {
	t.Helper()
	fw := &Forwarder{dialTimeout: 3 * time.Second, handshakeTimeout: 3 * time.Second}
	local, err := fw.StartFor(p, id, c.sink)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = fw.Stop() })
	return local
}

func httpProxyAt(port int) model.Proxy {
	return model.Proxy{ID: "px_log", Name: "记录代理", Type: "http", Host: "127.0.0.1", Port: port, Enabled: true}
}

func TestForwarderLogsSuccessfulTunnel(t *testing.T) {
	echo := newEchoServer(t)
	upstream := newFakeProxy(t, "")
	c := &collector{}
	local := startLoggingForwarder(t, httpProxyAt(upstream.Port()),
		Identity{PluginID: "trae", AccountID: "acc_7"}, c)

	resp, err := newTunnelClient(t, local).Get(echo.URL() + "/log-me")
	if err != nil {
		t.Fatalf("GET through forwarder: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "echo-ok" {
		t.Fatalf("body = %q", body)
	}

	rows := c.wait(t, 1)
	row := rows[0]
	if row.PluginID != "trae" || row.AccountID != "acc_7" {
		t.Fatalf("identity = %q/%q, want trae/acc_7", row.PluginID, row.AccountID)
	}
	if row.ProxyID != "px_log" || row.ProxyLabel != "记录代理" {
		t.Fatalf("proxy = %q/%q", row.ProxyID, row.ProxyLabel)
	}
	host, port, err := net.SplitHostPort(echo.Addr())
	if err != nil {
		t.Fatalf("echo addr: %v", err)
	}
	portNum, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("echo port: %v", err)
	}
	if row.TargetHost != host || row.TargetPort != portNum {
		t.Fatalf("target = %q:%d, want %s:%s", row.TargetHost, row.TargetPort, host, port)
	}
	if row.Result != model.ProxyResultOK || row.Error != "" {
		t.Fatalf("result = %q error = %q", row.Result, row.Error)
	}
	if row.BytesUp <= 0 || row.BytesDown <= 0 {
		t.Fatalf("bytes = up %d down %d, want both counted", row.BytesUp, row.BytesDown)
	}
	if row.TS == "" || row.At <= 0 || row.DurationMS < 0 {
		t.Fatalf("row lacks timestamp/duration: %+v", row)
	}
}

func TestForwarderLogsDeniedAndFailedTunnels(t *testing.T) {
	upstream := newFakeProxy(t, "")

	t.Run("non CONNECT is denied", func(t *testing.T) {
		c := &collector{}
		local := startLoggingForwarder(t, httpProxyAt(upstream.Port()),
			Identity{PluginID: "trae", AccountID: "acc_a"}, c)
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

		row := c.wait(t, 1)[0]
		if row.Result != model.ProxyResultDenied || row.Status != 405 {
			t.Fatalf("row = %+v, want denied/405", row)
		}
		if row.PluginID != "trae" || row.AccountID != "acc_a" {
			t.Fatalf("denied row lost its identity: %+v", row)
		}
	})

	t.Run("bad target is denied", func(t *testing.T) {
		c := &collector{}
		local := startLoggingForwarder(t, httpProxyAt(upstream.Port()), Identity{}, c)
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
		if resp.StatusCode != 400 {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
		row := c.wait(t, 1)[0]
		if row.Result != model.ProxyResultDenied || row.Status != 400 {
			t.Fatalf("row = %+v, want denied/400", row)
		}
	})

	t.Run("proxy refusal is denied with its status", func(t *testing.T) {
		authed := newFakeProxy(t, basicAuth("alice", "right"))
		c := &collector{}
		p := httpProxyAt(authed.Port())
		p.Username, p.Password = "alice", "wrong"
		local := startLoggingForwarder(t, p, Identity{PluginID: "trae", AccountID: "acc_b"}, c)

		if _, err := newTunnelClient(t, local).Get(newEchoServer(t).URL() + "/nope"); err == nil {
			t.Fatal("expected the request to fail on proxy auth")
		}
		row := c.wait(t, 1)[0]
		if row.Result != model.ProxyResultDenied {
			t.Fatalf("result = %q, want denied (row %+v)", row.Result, row)
		}
		if row.Status != 407 {
			t.Fatalf("status = %d, want the upstream 407", row.Status)
		}
		if row.Error == "" {
			t.Fatal("refusal lost its reason text")
		}
	})

	t.Run("dead upstream is failed", func(t *testing.T) {
		c := &collector{}
		local := startLoggingForwarder(t, httpProxyAt(freePort(t)),
			Identity{PluginID: "trae", AccountID: "acc_c"}, c)
		if _, err := newTunnelClient(t, local).Get(newEchoServer(t).URL() + "/never"); err == nil {
			t.Fatal("expected failure against a dead upstream proxy")
		}
		row := c.wait(t, 1)[0]
		if row.Result != model.ProxyResultFailed {
			t.Fatalf("result = %q, want failed (row %+v)", row.Result, row)
		}
		if row.Status != 502 || row.Error == "" {
			t.Fatalf("row = %+v, want status 502 and a reason", row)
		}
	})
}

func TestManagerAttributesTrafficPerAccount(t *testing.T) {
	echo := newEchoServer(t)
	upstream := newFakeProxy(t, "")
	st := &fakeStore{
		proxies: []model.Proxy{{ID: "px_m", Name: "M", Type: "http", Host: "127.0.0.1", Port: upstream.Port(), Enabled: true}},
		assignments: []model.AccountEgress{
			{PluginID: "plug", AccountID: "acc_1", Mode: model.EgressProxy, ProxyID: "px_m"},
			{PluginID: "plug", AccountID: "acc_2", Mode: model.EgressProxy, ProxyID: "px_m"},
		},
	}
	c := &collector{}
	m := NewManager(st)
	m.SetLogSink(c.sink)
	t.Cleanup(func() { _ = m.CloseAll() })
	if err := m.StartEnabled(); err != nil {
		t.Fatalf("StartEnabled: %v", err)
	}

	url1, ok := m.LocalURLFor("plug", "acc_1", "px_m")
	if !ok {
		t.Fatal("acc_1 has no listener")
	}
	// Asking twice reuses the same listener.
	if again, _ := m.LocalURLFor("plug", "acc_1", "px_m"); again != url1 {
		t.Fatalf("second ask returned %q, want the same url %q", again, url1)
	}
	url2, ok := m.LocalURLFor("plug", "acc_2", "px_m")
	if !ok {
		t.Fatal("acc_2 has no listener")
	}
	if url1 == url2 {
		t.Fatal("two accounts share one listener; traffic cannot be attributed")
	}

	for _, url := range []string{url1, url2} {
		resp, err := newTunnelClient(t, url).Get(echo.URL() + "/attributed")
		if err != nil {
			t.Fatalf("GET via %s: %v", url, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	rows := c.wait(t, 2)
	seen := map[string]string{}
	for _, row := range rows {
		if row.PluginID != "plug" {
			t.Fatalf("row lost its plugin: %+v", row)
		}
		seen[row.AccountID] = row.Result
	}
	if seen["acc_1"] != model.ProxyResultOK || seen["acc_2"] != model.ProxyResultOK {
		t.Fatalf("per-account rows = %+v", seen)
	}
}
