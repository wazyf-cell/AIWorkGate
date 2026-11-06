package main

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Endpoints and fingerprints, all ported from the reference implementation's
// opencode-product.ts / opencode.ts.
const (
	// inferenceBase is the free-tier entry. /zen/v1 is NOT used: the reference
	// records that it answers FreeTierError for anonymous free models.
	inferenceBase = "https://opencode.ai/inference"
	// chatPath and modelsPath keep the v1 suffix split so the base stays reusable.
	chatPath   = "/v1/chat/completions"
	modelsPath = "/v1/models"

	// anonymousKey is the literal `public`. The official CLI sends exactly this
	// when it has no key (`options: ok ? {} : { apiKey: 'public' }`), which is
	// why the free tier is usable with no login at all.
	anonymousKey = "public"

	// defaultUserAgent must keep the v2 shape
	// `opencode/<channel>/<version>/<client>` with major >= 2. The old
	// `opencode/1.18.22` shape has been rejected with 403 FreeTierError since
	// 2026-10-14. Measured against the live endpoint on 2026-10-21: this value
	// is accepted.
	defaultUserAgent = "opencode/latest/2.0.26/cli"

	// Gateway timeouts. These mirror the reference's chatHeaderTimeoutMs (60s)
	// and chatChunkTimeoutMs (120s); they bound the wait for response headers and
	// the idle gap between SSE chunks respectively.
	chatHeaderTimeoutMs = 60 * time.Second
	chatChunkTimeoutMs  = 120 * time.Second

	// catalogTimeout bounds a models list call. The reference adds an explicit
	// 8s timeout because the desktop client's fetch to opencode.ai can hang
	// forever (2026-10-02 incident); without it the model list stays blank.
	catalogTimeout = 8 * time.Second

	// base62 is the alphabet the session id tail uses.
	base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

	// sessionTailLength is 12 hex + 14 base62 characters, matching the
	// `ses_ + 12hex + 14base62` shape the reference derives.
	sessionTailLength   = 26
	sessionRandomLength = sessionTailLength - 12
)

// projectID derives the stable x-opencode-project fingerprint for one identity.
//
// Ported verbatim from opencode.ts deriveProjectId: the inner sha256 hashes
// "<identity> <generation>", and the outer sha1 hashes
// "git-remote:opencode/<inner hex>". The generation allows rotating the
// fingerprint without changing the credential, which is what the reference's
// nextFingerprintGeneration does.
func projectID(identity string, generation int) string {
	inner := sha256.Sum256([]byte(fmt.Sprintf("%s %d", identity, generation)))
	outer := sha1.Sum([]byte("git-remote:opencode/" + hex.EncodeToString(inner[:])))
	return hex.EncodeToString(outer[:])
}

var (
	lastSessionStamp int64
	sessionSequence  int
)

// sessionID derives a new `ses_...` id.
//
// Ported from opencode.ts deriveSessionId2: the first 12 hex characters encode
// the bitwise complement of (unix_ms * 4096 + sequence), and the remaining 14
// characters are random base62. The reference warns that the shape matters —
// a wrong session id shape produces FreeTierError.
func sessionID() string {
	now := time.Now().UnixMilli()
	if now != lastSessionStamp {
		lastSessionStamp = now
		sessionSequence = 0
	}
	sessionSequence++
	value := ^(now*4096 + int64(sessionSequence))
	var hexPart strings.Builder
	for i := 0; i < 6; i++ {
		b := (value >> uint(40-8*i)) & 0xff
		fmt.Fprintf(&hexPart, "%02x", b)
	}
	var tail strings.Builder
	buf := make([]byte, sessionRandomLength)
	if _, err := rand.Read(buf); err != nil {
		for i := range buf {
			buf[i] = byte(i * 7)
		}
	}
	for _, b := range buf {
		tail.WriteByte(base62[int(b)%len(base62)])
	}
	return "ses_" + hexPart.String() + tail.String()
}

// userAgentForVersion rewrites a detected CLI version into the v2 UA shape.
// A major below 2 yields "" so the caller keeps the safe default.
func userAgentForVersion(version string) string {
	parts := strings.Split(strings.TrimSpace(version), ".")
	if len(parts) != 3 {
		return ""
	}
	for _, p := range parts {
		if p == "" {
			return ""
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return ""
			}
		}
	}
	if parts[0] < "2" {
		return ""
	}
	return "opencode/latest/" + strings.TrimSpace(version) + "/cli"
}

// chatHeaders is the identity header set for one request.
//
// These four same-value session headers are load-bearing: the reference says
// "v2: these four headers must all carry the same value, missing any one is 403".
func chatHeaders(project, session, userAgent string) [][2]string {
	if strings.TrimSpace(userAgent) == "" {
		userAgent = defaultUserAgent
	}
	return [][2]string{
		{"x-opencode-project", project},
		{"x-opencode-session", session},
		{"x-opencode-session-id", session},
		{"x-session-affinity", session},
		{"x-session-id", session},
		{"x-opencode-client", "cli"},
		{"user-agent", userAgent},
	}
}

// upstreamClient talks to the inference host.
type upstreamClient struct {
	http *http.Client
	// base is a field so a test can point it at a mock server.
	base string

	// Egress state: when the host binds this account to a local forwarder, every
	// upstream call goes through it. A blocked binding fails closed instead of
	// silently falling back to the direct connection — the free tier's regional
	// decision is made from the egress address, so an unnoticed fallback would
	// look like a random upstream failure rather than a routing problem.
	mu          sync.RWMutex
	proxyURL    string
	proxyClient *http.Client
	egressErr   error
}

func newUpstreamClient() *upstreamClient {
	return &upstreamClient{
		base: inferenceBase,
		http: &http.Client{
			// No total timeout: a streamed answer may run for minutes. The
			// header and chunk timeouts below bound it instead.
			Transport: &http.Transport{
				Proxy:                 nil,
				MaxIdleConnsPerHost:   8,
				IdleConnTimeout:       90 * time.Second,
				ResponseHeaderTimeout: chatHeaderTimeoutMs,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// setEgress installs (or clears) the account's proxy route. It is called once at
// startup and again on /reload, so a binding change takes effect without a
// restart. proxyURL=="" and a nil error mean "connect directly".
func (c *upstreamClient) setEgress(proxyURL string, blocked error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.proxyURL = strings.TrimSpace(proxyURL)
	c.proxyClient = nil
	c.egressErr = blocked
	if c.proxyURL == "" {
		return
	}
	u, err := url.Parse(c.proxyURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		c.egressErr = fmt.Errorf("egress: invalid proxy url %q", c.proxyURL)
		return
	}
	base, ok := c.http.Transport.(*http.Transport)
	if !ok {
		c.egressErr = fmt.Errorf("egress: base transport is not configurable")
		return
	}
	tr := base.Clone()
	tr.Proxy = http.ProxyURL(u)
	c.proxyClient = &http.Client{
		Transport:     tr,
		CheckRedirect: c.http.CheckRedirect,
	}
}

// client returns the HTTP client to use. A blocked egress binding returns an
// error rather than an untunnelled client.
func (c *upstreamClient) client() (*http.Client, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.egressErr != nil {
		return nil, c.egressErr
	}
	if c.proxyClient != nil {
		return c.proxyClient, nil
	}
	return c.http, nil
}

// chat POSTs an already-shaped body and returns the unread response. The caller
// owns closing the body.
func (c *upstreamClient) chat(ctx context.Context, project, session, userAgent, apiKey string, body []byte) (*http.Response, error) {
	return c.chatTo(ctx, c.base, project, session, userAgent, apiKey, body)
}

// chatTo is chat with an explicit base, so tests use a mock server.
func (c *upstreamClient) chatTo(ctx context.Context, base, project, session, userAgent, apiKey string, body []byte) (*http.Response, error) {
	client, err := c.client()
	if err != nil {
		return nil, err
	}
	target := strings.TrimRight(base, "/") + chatPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	for _, kv := range chatHeaders(project, session, userAgent) {
		req.Header.Set(kv[0], kv[1])
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "text/event-stream")
	req.Header.Set("authorization", "Bearer "+apiKey)
	return client.Do(req)
}

// catalog GETs /v1/models. The response is returned raw: it carries only
// id/object/created/owned_by, so callers treat it as a name list, not metadata.
func (c *upstreamClient) catalog(ctx context.Context, apiKey string) ([]byte, error) {
	client, err := c.client()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, catalogTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.base, "/")+modelsPath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("authorization", "Bearer "+apiKey)
	req.Header.Set("accept", "application/json")
	req.Header.Set("user-agent", defaultUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("opencode: models HTTP %d: %s", resp.StatusCode, truncate(string(data), 256))
	}
	return data, nil
}

// readFileLimited reads at most limit bytes from path.
func readFileLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, limit))
}

// stringPathSeparator is os.PathSeparator as a string, kept local so imports
// stay minimal in files that only need path joining.
func stringPathSeparator() string { return string(os.PathSeparator) }

// truncate shortens s for an error message.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
