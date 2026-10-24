// Package egress implements account-level egress for AIWorkGate.
//
// Every enabled upstream proxy (HTTP CONNECT or SOCKS5) gets exactly one local
// HTTP CONNECT listener bound to 127.0.0.1:<ephemeral port>. Plugins only ever
// learn the local "http://<listener-token>@127.0.0.1:<port>" URL (see
// EgressFile): the token authenticates the plugin to its own listener, and
// upstream proxy credentials never leave this package.
//
// The proxy listener is deliberately fail-closed: if the upstream proxy cannot
// be reached, the client connection is closed. There is no direct fallback.
//
// A listener of type ProxyTypeDirect ("直连") binds the same loopback CONNECT
// surface but dials the CONNECT target itself instead of an upstream proxy, so
// an account without a proxy can still be accounted for in proxy_logs. That
// one is fail-open by construction: see direct.go.
package egress

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	xproxy "golang.org/x/net/proxy"

	"aiworkgate/internal/model"
)

const (
	// ProxyTypeHTTP is an upstream HTTP proxy that supports CONNECT.
	ProxyTypeHTTP = "http"
	// ProxyTypeSOCKS5 is an upstream SOCKS5 proxy. Targets are always passed
	// as domain names so DNS resolution happens remotely.
	ProxyTypeSOCKS5 = "socks5"

	defaultDialTimeout      = 10 * time.Second
	defaultHandshakeTimeout = 10 * time.Second

	maxProxyHeaderLines = 128
	maxProxyHeaderLine  = 8 << 10
)

var (
	// ErrForwarderClosed is returned when Start is called after Stop.
	ErrForwarderClosed = errors.New("egress: forwarder is closed")
	// ErrForwarderStarted is returned when Start is called twice.
	ErrForwarderStarted = errors.New("egress: forwarder already started")
	// ErrUnsupportedProxyType is returned for a proxy type other than
	// "http" or "socks5".
	ErrUnsupportedProxyType = errors.New("egress: unsupported proxy type")
)

// forwardConfig is the immutable dialing configuration captured at Start.
type forwardConfig struct {
	typ        string
	addr       string
	authHeader string // full Proxy-Authorization value, empty when no auth

	// clientAuthHeader is the Proxy-Authorization value a local client must
	// present. The token is minted per listener and embedded in the URL the
	// manager publishes, so another process on the machine cannot borrow the
	// listener just by finding its port in netstat.
	clientAuthHeader string
}

// Identity names the plugin and account a local listener serves. One listener
// per (plugin, account, proxy) is what makes a tunnel attributable: a shared
// listener would see the target but not who dialled it.
type Identity struct {
	PluginID  string
	AccountID string
}

// LogSink receives one row per tunnelled CONNECT. It must not block for long:
// it runs on the connection goroutine after the tunnel closes.
type LogSink func(model.ProxyLog)

// Forwarder exposes one local HTTP CONNECT listener that tunnels every
// connection through a single upstream proxy. The zero value is ready to use.
type Forwarder struct {
	mu      sync.Mutex
	cfg     forwardConfig
	proxy   model.Proxy
	ln      net.Listener
	url     string
	started bool
	stopped bool

	identity Identity
	sink     LogSink

	conns     map[net.Conn]struct{}
	wg        sync.WaitGroup
	serveDone chan struct{}

	dialTimeout      time.Duration
	handshakeTimeout time.Duration
}

// Start binds a loopback listener and starts accepting HTTP CONNECT requests.
// It returns the local URL plugins must use, e.g. "http://127.0.0.1:7901".
// The upstream proxy is not contacted until a client actually connects, so a
// dead upstream is (correctly) reported per request rather than at Start.
func (f *Forwarder) Start(p model.Proxy) (string, error) {
	return f.StartFor(p, Identity{}, nil)
}

// StartFor is Start with the identity and log sink that attribute each tunnel.
// A p of type ProxyTypeDirect binds a direct listener: same loopback CONNECT
// surface, but the dialled endpoint is the CONNECT target itself, so no host,
// port or credentials are required (and any that are set are ignored).
func (f *Forwarder) StartFor(p model.Proxy, id Identity, sink LogSink) (string, error) {
	typ, err := normalizeProxyType(p.Type)
	if err != nil {
		return "", err
	}
	// addr is empty for a direct listener by design: there is no upstream.
	var addr string
	if typ != ProxyTypeDirect {
		if addr, err = proxyHostPort(p); err != nil {
			return "", err
		}
	}
	var authHeader string
	if typ != ProxyTypeDirect && (p.Username != "" || p.Password != "") {
		raw := p.Username + ":" + p.Password
		authHeader = "Basic " + base64.StdEncoding.EncodeToString([]byte(raw))
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopped {
		return "", ErrForwarderClosed
	}
	if f.started {
		return "", ErrForwarderStarted
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("egress: listen on loopback: %w", err)
	}
	rawToken := make([]byte, 32)
	if _, err := rand.Read(rawToken); err != nil {
		_ = ln.Close()
		return "", fmt.Errorf("egress: mint listener token: %w", err)
	}
	token := hex.EncodeToString(rawToken)
	if f.dialTimeout <= 0 {
		f.dialTimeout = defaultDialTimeout
	}
	if f.handshakeTimeout <= 0 {
		f.handshakeTimeout = defaultHandshakeTimeout
	}
	f.proxy = p
	f.identity = id
	f.sink = sink
	f.cfg = forwardConfig{
		typ:              typ,
		addr:             addr,
		authHeader:       authHeader,
		clientAuthHeader: "Basic " + base64.StdEncoding.EncodeToString([]byte(token+":")),
	}
	f.ln = ln
	// The token rides in the URL userinfo: http.ProxyURL then supplies
	// Proxy-Authorization on every CONNECT without any plugin-side change.
	f.url = "http://" + token + "@" + ln.Addr().String()
	f.conns = make(map[net.Conn]struct{})
	f.serveDone = make(chan struct{})
	f.started = true

	go f.serve(ln, f.serveDone)
	return f.url, nil
}

// LocalURL reports the loopback URL, or "" when the forwarder is not running.
func (f *Forwarder) LocalURL() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.started || f.stopped {
		return ""
	}
	return f.url
}

// Stop closes the listener and every active tunnel, then waits for all
// handler goroutines to exit. It is idempotent.
func (f *Forwarder) Stop() error {
	f.mu.Lock()
	if f.stopped || !f.started {
		f.stopped = true
		f.mu.Unlock()
		return nil
	}
	f.stopped = true
	f.url = ""
	ln := f.ln
	done := f.serveDone
	f.mu.Unlock()

	var err error
	if ln != nil {
		err = ln.Close()
	}
	if done != nil {
		<-done
	}

	f.mu.Lock()
	for c := range f.conns {
		_ = c.Close()
	}
	f.conns = nil
	f.mu.Unlock()

	f.wg.Wait()
	return err
}

// Close is an alias for Stop.
func (f *Forwarder) Close() error { return f.Stop() }

func (f *Forwarder) serve(ln net.Listener, done chan struct{}) {
	defer close(done)
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		if f.conns == nil {
			f.mu.Unlock()
			_ = conn.Close()
			return
		}
		f.conns[conn] = struct{}{}
		f.wg.Add(1)
		f.mu.Unlock()
		go f.handle(conn)
	}
}

func (f *Forwarder) handle(client net.Conn) {
	defer f.wg.Done()
	defer func() {
		f.mu.Lock()
		if f.conns != nil {
			delete(f.conns, client)
		}
		f.mu.Unlock()
		_ = client.Close()
	}()

	started := time.Now()
	target := ""
	var targetHost string
	var targetPort int
	// logRow records one attempt. status is the HTTP status the local client
	// saw (405/400/502) or the upstream proxy's refusal code.
	logRow := func(result string, status int, errText string, up, down int64) {
		f.logProxy(model.ProxyLog{
			TS:         started.Format(time.RFC3339),
			At:         started.Unix(),
			PluginID:   f.identity.PluginID,
			AccountID:  f.identity.AccountID,
			ProxyID:    f.proxy.ID,
			ProxyLabel: f.proxy.Name,
			TargetHost: targetHost,
			TargetPort: targetPort,
			Result:     result,
			Status:     status,
			DurationMS: time.Since(started).Milliseconds(),
			BytesUp:    up,
			BytesDown:  down,
			Error:      errText,
		})
	}

	// A panic in one tunnel must not take down the whole host. logProxy is the
	// only place in this file that already recovers, so funnel the report there.
	defer func() {
		if r := recover(); r != nil {
			logRow(model.ProxyResultFailed, http.StatusInternalServerError,
				fmt.Sprintf("panic: %v", r), 0, 0)
		}
	}()

	_ = client.SetDeadline(time.Now().Add(f.handshakeTimeout))
	clientR := bufio.NewReader(client)
	req, err := http.ReadRequest(clientR)
	if err != nil {
		logRow(model.ProxyResultFailed, 0, "read CONNECT: "+err.Error(), 0, 0)
		return
	}
	if req.Body != nil {
		_ = req.Body.Close()
	}
	if req.Method != http.MethodConnect {
		writeHTTPError(client, http.StatusMethodNotAllowed, "only HTTP CONNECT is supported")
		logRow(model.ProxyResultDenied, http.StatusMethodNotAllowed, "not a CONNECT request", 0, 0)
		return
	}
	if !f.clientAuthorized(req) {
		writeProxyAuthRequired(client)
		logRow(model.ProxyResultDenied, http.StatusProxyAuthRequired,
			"missing or invalid listener credentials", 0, 0)
		return
	}
	target = req.Host
	if target == "" && req.URL != nil {
		target = req.URL.Host
	}
	if host, port, err := net.SplitHostPort(target); err != nil {
		writeHTTPError(client, http.StatusBadRequest, "CONNECT target must be host:port")
		logRow(model.ProxyResultDenied, http.StatusBadRequest, "bad CONNECT target", 0, 0)
		return
	} else {
		targetHost = host
		if n, err := strconv.Atoi(port); err == nil {
			targetPort = n
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), f.dialTimeout+f.handshakeTimeout)
	defer cancel()
	upstream, upstreamR, err := f.dialUpstream(ctx, target)
	if err != nil {
		// A proxied listener fails closed here: it never falls back to a direct
		// connection. A direct listener has no fallback question to answer -
		// the dial that failed is the dial the plugin would have made itself,
		// so closing is exactly what the plugin would have seen.
		var refused *connectRefusedError
		result := model.ProxyResultFailed
		status := http.StatusBadGateway
		if errors.As(err, &refused) {
			result = model.ProxyResultDenied
			if refused.status > 0 {
				status = refused.status
			}
		}
		writeHTTPError(client, http.StatusBadGateway, f.dialFailureText())
		logRow(result, status, err.Error(), 0, 0)
		return
	}

	_ = client.SetDeadline(time.Time{})
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		_ = upstream.Close()
		logRow(model.ProxyResultFailed, 0, "write 200 to client: "+err.Error(), 0, 0)
		return
	}
	up, down, tunnelErr := f.tunnel(client, clientR, upstream, upstreamR)
	if tunnelErr != nil {
		logRow(model.ProxyResultFailed, http.StatusInternalServerError,
			"tunnel: "+tunnelErr.Error(), up, down)
		return
	}
	logRow(model.ProxyResultOK, http.StatusOK, "", up, down)
}

// logProxy hands the row to the sink without letting a sink failure affect the
// connection that already finished.
func (f *Forwarder) logProxy(row model.ProxyLog) {
	sink := f.sink
	if sink == nil {
		return
	}
	defer func() { _ = recover() }()
	sink(row)
}

// tunnel copies both directions and returns (client→upstream, upstream→client)
// byte counts. Either side ending tears down both directions. A panic inside a
// pump is recovered there and returned here so the caller logs one failure row.
func (f *Forwarder) tunnel(client net.Conn, clientR io.Reader, upstream net.Conn, upstreamR io.Reader) (int64, int64, error) {
	defer func() { _ = upstream.Close() }()

	done := make(chan pumpResult, 2)
	go f.pumpTunnel(upstream, clientR, 0, done)
	go f.pumpTunnel(client, upstreamR, 1, done)

	first := <-done
	_ = client.Close()
	_ = upstream.Close()
	second := <-done

	var up, down int64
	var panicErr error
	for _, r := range []pumpResult{first, second} {
		if r.panicErr != nil && panicErr == nil {
			panicErr = fmt.Errorf("pump dir %d: %v", r.dir, r.panicErr)
		}
		if r.dir == 0 {
			up = r.n
		} else {
			down = r.n
		}
	}
	return up, down, panicErr
}

// pumpResult is one direction's byte count: dir 0 is client→upstream.
type pumpResult struct {
	dir      int
	n        int64
	panicErr any
}

func (f *Forwarder) pumpTunnel(dst net.Conn, src io.Reader, dir int, done chan<- pumpResult) {
	var n int64
	var panicErr any
	defer func() {
		if r := recover(); r != nil {
			panicErr = r
		}
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- pumpResult{dir: dir, n: n, panicErr: panicErr}
	}()
	n, _ = io.Copy(dst, src)
}

func (f *Forwarder) dialUpstream(ctx context.Context, target string) (net.Conn, io.Reader, error) {
	switch f.cfg.typ {
	case ProxyTypeDirect:
		// The CONNECT target is the destination, not a hint for an upstream.
		conn, err := dialDirect(ctx, target, f.dialTimeout)
		if err != nil {
			return nil, nil, err
		}
		return conn, conn, nil
	case ProxyTypeHTTP:
		return dialHTTPConnect(ctx, f.cfg.addr, f.cfg.authHeader, target, f.dialTimeout, f.handshakeTimeout)
	case ProxyTypeSOCKS5:
		conn, err := dialSOCKS5(ctx, f.cfg.addr, f.proxy.Username, f.proxy.Password, target, f.dialTimeout)
		if err != nil {
			return nil, nil, err
		}
		return conn, conn, nil
	default:
		return nil, nil, ErrUnsupportedProxyType
	}
}

// dialFailureText is the reason a local client is given when the dial behind
// its CONNECT failed. The wording distinguishes the two listener kinds: a
// direct listener has no upstream proxy to blame.
func (f *Forwarder) dialFailureText() string {
	if f.cfg.typ == ProxyTypeDirect {
		return "direct egress unavailable"
	}
	return "upstream proxy unavailable"
}

// connectRefusedError marks a CONNECT the upstream proxy answered with a
// non-2xx status. It is separate from transport failures so the traffic log
// can label the attempt "denied" instead of "failed".
type connectRefusedError struct {
	status int
	line   string
}

func (e *connectRefusedError) Error() string {
	return fmt.Sprintf("egress: upstream proxy refused CONNECT: %s", e.line)
}

// dialHTTPConnect performs an upstream CONNECT handshake and returns the raw
// tunnel. The returned reader may hold buffered tunnel bytes.
func dialHTTPConnect(ctx context.Context, proxyAddr, authHeader, target string, dialTimeout, handshakeTimeout time.Duration) (net.Conn, io.Reader, error) {
	d := &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("egress: dial upstream proxy %s: %w", proxyAddr, err)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	_ = conn.SetDeadline(handshakeDeadline(ctx, handshakeTimeout))

	var b strings.Builder
	fmt.Fprintf(&b, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	if authHeader != "" {
		fmt.Fprintf(&b, "Proxy-Authorization: %s\r\n", authHeader)
	}
	b.WriteString("Proxy-Connection: keep-alive\r\n\r\n")
	if _, err := io.WriteString(conn, b.String()); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("egress: write CONNECT to %s: %w", proxyAddr, err)
	}

	br := bufio.NewReader(conn)
	code, statusLine, err := readConnectResponse(br)
	if err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("egress: upstream CONNECT %s: %w", target, err)
	}
	if code < 200 || code > 299 {
		_ = conn.Close()
		return nil, nil, &connectRefusedError{status: code, line: statusLine}
	}

	_ = conn.SetDeadline(time.Time{})
	return conn, br, nil
}

// dialSOCKS5 dials target through a SOCKS5 proxy. The target is passed as a
// domain name (when it is one), so the remote proxy resolves DNS.
func dialSOCKS5(ctx context.Context, proxyAddr, user, pass, target string, timeout time.Duration) (net.Conn, error) {
	var auth *xproxy.Auth
	if user != "" || pass != "" {
		auth = &xproxy.Auth{User: user, Password: pass}
	}
	dialer, err := xproxy.SOCKS5("tcp", proxyAddr, auth, &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("egress: create socks5 dialer for %s: %w", proxyAddr, err)
	}

	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := dialer.Dial("tcp", target)
		ch <- result{conn: c, err: err}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("egress: socks5 dial %s via %s: %w", target, proxyAddr, r.err)
		}
		return r.conn, nil
	case <-ctx.Done():
		go func() {
			if r := <-ch; r.conn != nil {
				_ = r.conn.Close()
			}
		}()
		return nil, fmt.Errorf("egress: socks5 dial %s via %s: %w", target, proxyAddr, ctx.Err())
	}
}

// readConnectResponse parses a proxy CONNECT response status line and headers.
// It intentionally ignores any response body: a successful CONNECT switches
// the connection to a raw tunnel, and failures are followed by a close.
func readConnectResponse(br *bufio.Reader) (int, string, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return 0, "", fmt.Errorf("read status line: %w", err)
	}
	statusLine := strings.TrimRight(line, "\r\n")
	fields := strings.SplitN(statusLine, " ", 3)
	if len(fields) < 2 || !strings.HasPrefix(fields[0], "HTTP/") {
		return 0, statusLine, fmt.Errorf("malformed status line %q", statusLine)
	}
	code, err := strconv.Atoi(fields[1])
	if err != nil || code < 100 || code > 599 {
		return 0, statusLine, fmt.Errorf("malformed status code in %q", statusLine)
	}
	for i := 0; i < maxProxyHeaderLines; i++ {
		h, err := br.ReadString('\n')
		if err != nil {
			return 0, statusLine, fmt.Errorf("read response headers: %w", err)
		}
		if len(h) > maxProxyHeaderLine {
			return 0, statusLine, errors.New("proxy response header line too long")
		}
		if h == "\r\n" || h == "\n" {
			return code, statusLine, nil
		}
	}
	return 0, statusLine, errors.New("too many proxy response headers")
}

func handshakeDeadline(ctx context.Context, fallback time.Duration) time.Time {
	deadline := time.Now().Add(fallback)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		return dl
	}
	return deadline
}

func writeHTTPError(w io.Writer, status int, msg string) {
	body := msg + "\n"
	_, _ = fmt.Fprintf(w,
		"HTTP/1.1 %d %s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		status, http.StatusText(status), len(body), body)
}

func writeProxyAuthRequired(w io.Writer) {
	body := "proxy credentials required\n"
	_, _ = fmt.Fprintf(w,
		"HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic realm=\"aiworkgate\"\r\n"+
			"Content-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		len(body), body)
}

// clientAuthorized checks the per-listener credential. Constant-time compare
// keeps the token out of reach of a timing oracle even though it is loopback.
func (f *Forwarder) clientAuthorized(req *http.Request) bool {
	got := strings.TrimSpace(req.Header.Get("Proxy-Authorization"))
	want := f.cfg.clientAuthHeader
	return want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func normalizeProxyType(t string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case ProxyTypeHTTP:
		return ProxyTypeHTTP, nil
	case ProxyTypeSOCKS5, "socks5h":
		return ProxyTypeSOCKS5, nil
	case ProxyTypeDirect:
		return ProxyTypeDirect, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnsupportedProxyType, t)
	}
}

// proxyHostPort normalizes the configured proxy endpoint. A port embedded in
// the host string wins over p.Port.
func proxyHostPort(p model.Proxy) (string, error) {
	host := strings.TrimSpace(p.Host)
	if host == "" {
		return "", errors.New("egress: proxy host is empty")
	}
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if h, portStr, err := net.SplitHostPort(host); err == nil {
		if port, convErr := strconv.Atoi(portStr); convErr == nil {
			host, p.Port = h, port
		}
	}
	host = strings.Trim(host, "[]")
	if p.Port < 1 || p.Port > 65535 {
		return "", fmt.Errorf("egress: proxy port out of range: %d", p.Port)
	}
	return net.JoinHostPort(host, strconv.Itoa(p.Port)), nil
}
