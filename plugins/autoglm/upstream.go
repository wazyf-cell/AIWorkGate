package main

// Upstream HTTP client.
//
// Ported from workbuddy2api-panel/internal/autoclaw/upstream.go. Endpoints,
// envelope handling and error classification are the reference's, one for one.
// The relay base rewriting (relayBase / relayProxyBase / relayBaseFromProxy) is
// reproduced because the sandbox API returns a bare host while the event stream
// lives under /autoclaw-cloud/proxy/<sandbox_id>.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const upstreamTimeout = 2 * time.Minute

// UpstreamError carries the reference's error classification so the host can
// tell "this account is dead" from "try again later".
type UpstreamError struct {
	Kind   string
	Status int
	Code   int64
	Msg    string
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("upstream %s (http %d code %d): %s", e.Kind, e.Status, e.Code, e.Msg)
}

// The marker lists below are the reference's (upstream.go). They are matched
// case-insensitively against the response body; the Chinese entries matter
// because the cn region answers in Chinese.
var (
	hardMarkers = []string{
		"insufficient credit", "no credit", "credit exhausted", "out of credit",
		"quota exceeded", "quota exhaust", "payment required", "not enough credit",
		"insufficient balance", "balance not enough", "limit reached", "usage limit",
		"余额不足", "额度不足", "积分不足", "积分用完", "额度用尽", "没有积分", "配额不足", "可用额度",
	}
	deadMarkers = []string{
		"not logged in", "unauthorized", "invalid token", "token expired",
		"login required", "session expired", "offline user session", "auth.inject failed",
		"未登录", "登录已过期", "请先登录", "token 失效",
	}
)

// classify maps an upstream failure onto the reference's kinds.
func classify(status int, code int64, body string) string {
	if code == 410000 {
		return "session_dead"
	}
	lower := strings.ToLower(body)
	for _, m := range hardMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return "hard_credit"
		}
	}
	for _, m := range deadMarkers {
		if strings.Contains(lower, strings.ToLower(m)) {
			return "session_dead"
		}
	}
	switch {
	case status == 402:
		return "hard_credit"
	case status == 429:
		return "soft_rate"
	case status == 404:
		return "not_found"
	case status >= 500:
		return "server"
	case status >= 400:
		return "client"
	}
	return "none"
}

func upstreamErrf(kind string, status int, code int64, format string, args ...any) *UpstreamError {
	return &UpstreamError{Kind: kind, Status: status, Code: code, Msg: fmt.Sprintf(format, args...)}
}

// ---- relay base rewriting ----

// relayBase normalises a sandbox endpoint to <host>/autoclaw-cloud, accepting a
// ws:// or plain host form just like the reference.
func relayBase(endpoint string) string {
	t := strings.TrimSpace(endpoint)
	low := strings.ToLower(t)
	if strings.HasPrefix(low, "ws://") {
		t = "http://" + t[5:]
	} else if strings.HasPrefix(low, "wss://") {
		t = "https://" + t[6:]
	}
	if !strings.Contains(t, "://") {
		t = strings.TrimRight(t, "/")
		if strings.HasSuffix(t, relaySuffix) {
			return t
		}
		return t + relaySuffix
	}
	sp, err := url.Parse(t)
	if err != nil {
		t = strings.TrimRight(t, "/")
		if strings.HasSuffix(t, relaySuffix) {
			return t
		}
		return t + relaySuffix
	}
	path := sp.Path
	if i := strings.Index(path, relaySuffix); i >= 0 {
		path = path[:i+len(relaySuffix)]
	} else {
		path = strings.TrimRight(path, "/") + relaySuffix
	}
	sp.Path = path
	sp.RawQuery, sp.Fragment = "", ""
	return strings.TrimRight(sp.String(), "/")
}

// relayProxyBase appends /proxy/<sandbox id> to the relay base.
func relayProxyBase(endpoint, sandboxID string) string {
	base := relayBase(endpoint)
	if sandboxID == "" {
		return base
	}
	return strings.TrimRight(base, "/") + "/proxy/" + strings.TrimSpace(sandboxID)
}

// relayBaseFromProxy strips the /proxy/<id> suffix again.
func relayBaseFromProxy(proxyBase string) string {
	p := strings.TrimRight(proxyBase, "/")
	if i := strings.LastIndex(p, "/proxy/"); i >= 0 {
		return p[:i]
	}
	return p
}

// ---- client ----

type upstream struct {
	http            *http.Client
	egress          *egressRouter
	userAPIOverride string
	deviceMu        sync.Mutex
	devices         map[string]string
}

func newUpstream(override string) *upstream {
	return &upstream{
		http:            &http.Client{Timeout: upstreamTimeout},
		egress:          newEgressRouter(upstreamTimeout),
		userAPIOverride: strings.TrimRight(override, "/"),
		devices:         map[string]string{},
	}
}

// do sends one request through the account's egress route. An empty accountID
// (the login handshakes) follows the default entry.
func (u *upstream) do(req *http.Request, accountID string) (*http.Response, error) {
	client, err := u.clientFor(accountID)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}

// clientFor resolves the HTTP client for one account.
func (u *upstream) clientFor(accountID string) (*http.Client, error) {
	if u.egress == nil {
		return u.http, nil
	}
	return u.egress.client(accountID)
}

// setEgress replaces the loaded bindings (startup and /reload).
func (u *upstream) setEgress(ef egressFile) {
	if u.egress != nil {
		u.egress.set(ef)
	}
}

// base returns the per-region origin, honouring a debug override.
func (u *upstream) base(region string) string {
	if u.userAPIOverride != "" {
		return u.userAPIOverride
	}
	if b, ok := userapiBase[region]; ok {
		return b
	}
	return userapiBase[regionCN]
}

// doJSON sends a JSON request and unwraps the {code,msg,data} envelope.
// nonEnvelope=true skips the code check for endpoints that answer bare JSON.
// accountID selects the egress binding ("" = default).
func (u *upstream) doJSON(method, accountID, fullURL string, headers map[string]string, body any, nonEnvelope bool) (json.RawMessage, error) {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, fullURL, rd)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := u.do(req, accountID)
	if err != nil {
		return nil, upstreamErrf("server", 0, 0, "%v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, upstreamErrf("server", resp.StatusCode, 0, "read: %v", err)
	}
	text := string(raw)
	if resp.StatusCode >= 400 {
		return nil, upstreamErrf(classify(resp.StatusCode, 0, text), resp.StatusCode, 0, "%.200s", text)
	}
	if nonEnvelope {
		return json.RawMessage(raw), nil
	}
	var env struct {
		Code any `json:"code"`
		Msg  any `json:"msg"`
		Data any `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, upstreamErrf("client", resp.StatusCode, 0, "parse failed: %.120s", text)
	}
	code := toInt64(env.Code)
	if code != 0 {
		kind := classify(resp.StatusCode, code, toString(env.Msg))
		if kind == "none" {
			kind = "client"
		}
		return nil, upstreamErrf(kind, resp.StatusCode, code, "code=%d msg=%.160s", code, toString(env.Msg))
	}
	if env.Data == nil {
		return json.RawMessage("null"), nil
	}
	out, err := json.Marshal(env.Data)
	if err != nil {
		return nil, upstreamErrf("client", resp.StatusCode, 0, "re-encode data: %v", err)
	}
	return out, nil
}

// getJSON issues a signed GET with the web header set and returns the raw body.
// Unlike doJSON it does not unwrap the envelope: the caller inspects {code,data}
// itself (the ledger endpoint nests an extra level).
func (u *upstream) getJSON(fullURL, accountID, token string) (json.RawMessage, error) {
	req, err := http.NewRequest(http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range userapiHeaders(token) {
		req.Header.Set(k, v)
	}
	resp, err := u.do(req, accountID)
	if err != nil {
		return nil, upstreamErrf("server", 0, 0, "%v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, upstreamErrf("server", resp.StatusCode, 0, "read: %v", err)
	}
	if resp.StatusCode >= 400 {
		return nil, upstreamErrf(classify(resp.StatusCode, 0, string(raw)), resp.StatusCode, 0, "%.200s", string(raw))
	}
	return json.RawMessage(raw), nil
}
