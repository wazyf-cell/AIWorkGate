package egress

import (
	"bufio"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aiworkgate/internal/model"
)

// fakeProxy is a minimal upstream HTTP CONNECT proxy used by the tests. It
// tunnels CONNECT requests to the requested target and records what it saw.
type fakeProxy struct {
	ln   net.Listener
	auth string // expected Proxy-Authorization header; "" disables auth

	mu          sync.Mutex
	targets     []string
	authHeaders []string
	conns       map[net.Conn]struct{}
	closed      bool
	wg          sync.WaitGroup
}

func newFakeProxy(t *testing.T, auth string) *fakeProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fake proxy listen: %v", err)
	}
	fp := &fakeProxy{ln: ln, auth: auth, conns: map[net.Conn]struct{}{}}
	fp.wg.Add(1)
	go fp.serve()
	t.Cleanup(fp.Close)
	return fp
}

func (fp *fakeProxy) Port() int {
	return fp.ln.Addr().(*net.TCPAddr).Port
}

func (fp *fakeProxy) Addr() string { return fp.ln.Addr().String() }

func (fp *fakeProxy) serve() {
	defer fp.wg.Done()
	for {
		conn, err := fp.ln.Accept()
		if err != nil {
			return
		}
		fp.mu.Lock()
		fp.conns[conn] = struct{}{}
		fp.mu.Unlock()
		fp.wg.Add(1)
		go func(c net.Conn) {
			defer fp.wg.Done()
			fp.handle(c)
		}(conn)
	}
}

func (fp *fakeProxy) handle(c net.Conn) {
	defer func() {
		fp.mu.Lock()
		delete(fp.conns, c)
		fp.mu.Unlock()
		_ = c.Close()
	}()

	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	if req.Body != nil {
		_ = req.Body.Close()
	}

	fp.mu.Lock()
	fp.targets = append(fp.targets, req.Host)
	fp.authHeaders = append(fp.authHeaders, req.Header.Get("Proxy-Authorization"))
	fp.mu.Unlock()

	if req.Method != http.MethodConnect {
		writeHTTPError(c, http.StatusMethodNotAllowed, "CONNECT only")
		return
	}
	if fp.auth != "" && req.Header.Get("Proxy-Authorization") != fp.auth {
		writeHTTPError(c, http.StatusProxyAuthRequired, "proxy auth required")
		return
	}
	target := req.Host
	if _, _, err := net.SplitHostPort(target); err != nil {
		writeHTTPError(c, http.StatusBadRequest, "bad target")
		return
	}
	up, err := net.DialTimeout("tcp", target, 2*time.Second)
	if err != nil {
		writeHTTPError(c, http.StatusBadGateway, "upstream dial failed")
		return
	}
	defer func() { _ = up.Close() }()

	_ = c.SetDeadline(time.Time{})
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(up, br)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(c, up)
		done <- struct{}{}
	}()
	<-done
	_ = c.Close()
	_ = up.Close()
	<-done
}

func (fp *fakeProxy) Close() {
	fp.mu.Lock()
	if fp.closed {
		fp.mu.Unlock()
		return
	}
	fp.closed = true
	ln := fp.ln
	conns := make([]net.Conn, 0, len(fp.conns))
	for c := range fp.conns {
		conns = append(conns, c)
	}
	fp.mu.Unlock()

	_ = ln.Close()
	for _, c := range conns {
		_ = c.Close()
	}
	fp.wg.Wait()
}

func (fp *fakeProxy) Targets() []string {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	out := make([]string, len(fp.targets))
	copy(out, fp.targets)
	return out
}

func (fp *fakeProxy) AuthHeaders() []string {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	out := make([]string, len(fp.authHeaders))
	copy(out, fp.authHeaders)
	return out
}

// echoServer is a tiny HTTP origin that counts TCP connections, so tests can
// prove traffic did (or did not) go direct.
type echoServer struct {
	srv   *httptest.Server
	conns int64

	mu    sync.Mutex
	paths []string
}

func newEchoServer(t *testing.T) *echoServer {
	t.Helper()
	es := &echoServer{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		es.mu.Lock()
		es.paths = append(es.paths, r.URL.Path)
		es.mu.Unlock()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "echo-ok")
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			atomic.AddInt64(&es.conns, 1)
		}
	}
	srv.Start()
	es.srv = srv
	t.Cleanup(srv.Close)
	return es
}

func (es *echoServer) URL() string { return es.srv.URL }

func (es *echoServer) Addr() string { return strings.TrimPrefix(es.srv.URL, "http://") }

func (es *echoServer) ConnCount() int64 { return atomic.LoadInt64(&es.conns) }

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe free port: %v", err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}

func startForwarder(t *testing.T, p model.Proxy) (*Forwarder, string) {
	t.Helper()
	fw := &Forwarder{dialTimeout: 3 * time.Second, handshakeTimeout: 3 * time.Second}
	local, err := fw.Start(p)
	if err != nil {
		t.Fatalf("forwarder start: %v", err)
	}
	t.Cleanup(func() { _ = fw.Stop() })
	return fw, local
}

// newTunnelClient returns an HTTP client that reaches every target through the
// forwarder's CONNECT listener (no environment proxy, no direct dialing).
func newTunnelClient(t *testing.T, localURL string) *http.Client {
	t.Helper()
	tr := &http.Transport{
		Proxy:             nil,
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialViaHTTPProxy(ctx, localURL, addr)
		},
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 5 * time.Second}
}

func basicAuth(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// proxyURLParts splits a listener URL into the dialable host:port and the
// Proxy-Authorization value the listener demands. Listener URLs carry the
// per-listener token in the userinfo, so tests that speak raw CONNECT must
// forward it or they are (correctly) answered 407.
func proxyURLParts(t *testing.T, proxyURL string) (addr, auth string) {
	t.Helper()
	u, err := url.Parse(proxyURL)
	if err != nil || u.Host == "" {
		t.Fatalf("parse listener url %q: %v", proxyURL, err)
	}
	if u.User == nil {
		t.Fatalf("listener url %q carries no token", proxyURL)
	}
	pass, _ := u.User.Password()
	return u.Host, basicAuth(u.User.Username(), pass)
}
