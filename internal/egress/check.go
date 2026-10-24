package egress

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"aiworkgate/internal/model"
)

// DefaultIPEchoURL is the default exit-IP echo endpoint. Callers may override.
const DefaultIPEchoURL = "https://api.ipify.org?format=json"

// DefaultCheckTimeout bounds a single Check when the caller passes <= 0.
const DefaultCheckTimeout = 20 * time.Second

// Check verifies that a proxy works by starting a temporary forwarder, fetching
// ipEchoURL through it, and stopping the forwarder again. It returns the exit
// IP, the request latency and an error. The HTTP client is wired to the local
// CONNECT listener explicitly (no environment proxy, no direct fallback), so a
// broken upstream always fails Check.
func Check(ctx context.Context, p model.Proxy, ipEchoURL string, timeout time.Duration) (exitIP string, latency time.Duration, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(ipEchoURL) == "" {
		ipEchoURL = DefaultIPEchoURL
	}
	if timeout <= 0 {
		timeout = DefaultCheckTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var fw Forwarder
	local, err := fw.Start(p)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = fw.Stop() }()

	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(dctx context.Context, network, addr string) (net.Conn, error) {
			return dialViaHTTPProxy(dctx, local, addr)
		},
		DisableKeepAlives:     true,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		ForceAttemptHTTP2:     false,
	}
	defer transport.CloseIdleConnections()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ipEchoURL, nil)
	if err != nil {
		return "", 0, fmt.Errorf("egress: build ip echo request: %w", err)
	}
	req.Header.Set("User-Agent", "AIWorkGate-egress-check/1")

	start := time.Now()
	resp, err := (&http.Client{Transport: transport}).Do(req)
	latency = time.Since(start)
	if err != nil {
		return "", latency, fmt.Errorf("egress: ip echo via proxy %s: %w", p.ID, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", latency, fmt.Errorf("egress: read ip echo via proxy %s: %w", p.ID, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", latency, fmt.Errorf("egress: ip echo via proxy %s: HTTP %d", p.ID, resp.StatusCode)
	}
	ip, err := parseExitIP(body)
	if err != nil {
		return "", latency, fmt.Errorf("egress: ip echo via proxy %s: %w", p.ID, err)
	}
	return ip, latency, nil
}

// dialViaHTTPProxy opens a CONNECT tunnel through a local (loopback) forwarder
// and returns the resulting connection. Any bytes already buffered after the
// CONNECT response are preserved for the caller.
func dialViaHTTPProxy(ctx context.Context, proxyURL, target string) (net.Conn, error) {
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("egress: parse forwarder url %q: %w", proxyURL, err)
	}
	if u.Scheme != "http" {
		return nil, fmt.Errorf("egress: unsupported forwarder scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("egress: forwarder url %q has no host", proxyURL)
	}
	proxyAddr := u.Host
	if u.Port() == "" {
		proxyAddr = net.JoinHostPort(u.Hostname(), "80")
	}
	if _, _, err := net.SplitHostPort(target); err != nil {
		return nil, fmt.Errorf("egress: target %q must be host:port: %w", target, err)
	}

	d := &net.Dialer{Timeout: defaultDialTimeout, KeepAlive: 30 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("egress: dial forwarder %s: %w", proxyAddr, err)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	_ = conn.SetDeadline(handshakeDeadline(ctx, defaultHandshakeTimeout))

	var b strings.Builder
	fmt.Fprintf(&b, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	if u.User != nil {
		pass, _ := u.User.Password()
		raw := u.User.Username() + ":" + pass
		fmt.Fprintf(&b, "Proxy-Authorization: Basic %s\r\n", base64.StdEncoding.EncodeToString([]byte(raw)))
	}
	b.WriteString("\r\n")
	if _, err := io.WriteString(conn, b.String()); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("egress: write CONNECT to forwarder: %w", err)
	}

	br := bufio.NewReader(conn)
	code, statusLine, err := readConnectResponse(br)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("egress: CONNECT %s via forwarder: %w", target, err)
	}
	if code < 200 || code > 299 {
		_ = conn.Close()
		return nil, fmt.Errorf("egress: forwarder refused CONNECT %s: %s", target, statusLine)
	}

	_ = conn.SetDeadline(time.Time{})
	return &bufferedConn{Conn: conn, r: br}, nil
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// parseExitIP accepts plain-text echoes ("1.2.3.4"), quoted values and JSON
// objects such as {"ip":"1.2.3.4"}. Anything else is rejected so an error page
// with HTTP 200 is not mistaken for an exit IP.
func parseExitIP(body []byte) (string, error) {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return "", fmt.Errorf("empty ip echo response")
	}

	candidate := text
	if strings.HasPrefix(text, "{") {
		var payload struct {
			IP string `json:"ip"`
		}
		if err := json.Unmarshal([]byte(text), &payload); err != nil {
			return "", fmt.Errorf("invalid ip echo JSON: %w", err)
		}
		candidate = strings.TrimSpace(payload.IP)
		if candidate == "" {
			return "", fmt.Errorf("ip echo JSON has no \"ip\" field")
		}
	}

	candidate = strings.Trim(candidate, "\"")
	if i := strings.IndexAny(candidate, " \t\r\n,"); i >= 0 {
		candidate = candidate[:i]
	}
	if net.ParseIP(candidate) == nil {
		return "", fmt.Errorf("unexpected ip echo payload %q", truncateForError(text, 120))
	}
	return candidate, nil
}

func truncateForError(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
