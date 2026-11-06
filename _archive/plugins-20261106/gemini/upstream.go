// Package main — upstream client for Gemini Cloud Code Assist.
//
// Every endpoint and header is lifted from the reference implementation's
// vendor/channel-pack/src/gemini.ts, gemini-project.ts, gemini-credits.ts and
// gemini-oauth.ts. Nothing is guessed:
//
//	inference   POST https://daily-cloudcode-pa.googleapis.com/v1internal:streamGenerateContent?alt=sse
//	fallback    POST https://daily-cloudcode-pa.sandbox.googleapis.com/v1internal:streamGenerateContent?alt=sse
//	project     POST <endpoint>/v1internal:loadCodeAssist   body {"metadata":{"ideType":"ANTIGRAVITY"}}
//	quota       POST <sandbox>/v1internal:retrieveUserQuotaSummary  body {"project":"..."}
//	token       POST https://oauth2.googleapis.com/token
//	userinfo    GET  https://www.googleapis.com/oauth2/v2/userinfo
//	auth        https://accounts.google.com/o/oauth2/v2/auth
//
// The identity header set is the reference's GEMINI_IDENTITY_HEADERS verbatim;
// the upstream fingerprints on it.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	// endpointDaily and endpointSandbox are GEMINI_ENDPOINTS, in order. The
	// reference rotates to the sandbox on a switchable failure.
	endpointDaily   = "https://daily-cloudcode-pa.googleapis.com"
	endpointSandbox = "https://daily-cloudcode-pa.sandbox.googleapis.com"

	streamPath    = "/v1internal:streamGenerateContent?alt=sse"
	quotaPath     = "/v1internal:retrieveUserQuotaSummary"
	codeAssistPth = "/v1internal:loadCodeAssist"

	// loadCodeAssistBody is GEMINI_LOAD_CODE_ASSIST_BODY, sent byte for byte.
	loadCodeAssistBody = `{"metadata":{"ideType":"ANTIGRAVITY"}}`

	tokenEndpoint    = "https://oauth2.googleapis.com/token"
	userInfoEndpoint = "https://www.googleapis.com/oauth2/v2/userinfo"
	authEndpoint     = "https://accounts.google.com/o/oauth2/v2/auth"

	// clientID identifies the antigravity OAuth client the upstream expects. It is
	// a public identifier, not a secret, and the reference sends it in the clear.
	clientID = "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com"
)

// The reference also embeds a client_secret for that client. Repo discipline is
// "credentials are always <redacted>", so this plugin does not carry it: the
// operator supplies it at run time and it is never written to disk.
//
// A client_secret is only sent on the token endpoint. Without it, an installed-app
// public client still exchanges a code if Google treats this client as public
// (which it does for the loopback flow), but the plugin does not assume that —
// it sends the secret only when one was configured.
const clientSecretEnv = "AIWORKGATE_GEMINI_CLIENT_SECRET"

// clientSecretOrEmpty returns the configured OAuth client secret, or "" when none
// was supplied. An empty value is not an error: it just omits the field.
func clientSecretOrEmpty() string {
	return strings.TrimSpace(os.Getenv(clientSecretEnv))
}

// scopes is GEMINI_SCOPES, in the reference's order.
var scopes = []string{
	"openid",
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/userinfo.profile",
	"https://www.googleapis.com/auth/cclog",
	"https://www.googleapis.com/auth/experimentsandconfigs",
}

// identityHeaders is GEMINI_IDENTITY_HEADERS: the five impersonation headers the
// upstream fingerprints. Omitting them is not a cosmetic difference.
var identityHeaders = [][2]string{
	{"User-Agent", "antigravity/4.3.0 (cmdc-pak)"},
	{"x-client-name", "antigravity"},
	{"x-client-version", "4.3.0"},
	{"x-machine-id", "cmdc-pak"},
	{"x-vscode-sessionid", "proxy"},
}

// upstreamClient issues Cloud Code requests and returns raw responses.
//
// Every request goes through egress.client(accountID), so an account bound to a
// proxy sends inference, project probe, quota and token traffic through that
// proxy. tokenURL and userInfoURL are fields rather than consts only so a test
// can point them at an httptest server.
type upstreamClient struct {
	http        *http.Client // used when no egress router is attached
	egress      *egressRouter
	tokenURL    string
	userInfoURL string
}

func newUpstreamClient() *upstreamClient {
	return &upstreamClient{
		http:        &http.Client{Timeout: 10 * time.Minute},
		egress:      newEgressRouter(10 * time.Minute),
		tokenURL:    tokenEndpoint,
		userInfoURL: userInfoEndpoint,
	}
}

// httpFor resolves the HTTP client for one account.
func (c *upstreamClient) httpFor(accountID string) (*http.Client, error) {
	if c.egress == nil {
		return c.http, nil
	}
	return c.egress.client(accountID)
}

// geminiHeaders is geminiHeaders: bearer + JSON + the identity set. Accept is
// deliberately absent: the reference documents that the streaming request must
// not send Accept, matching its own traffic capture.
func geminiHeaders(accessToken string) [][2]string {
	h := [][2]string{
		{"Authorization", "Bearer " + accessToken},
		{"Content-Type", "application/json"},
	}
	h = append(h, identityHeaders...)
	return h
}

// stream POSTs the envelope to one endpoint and returns an unread response.
func (c *upstreamClient) stream(ctx context.Context, accessToken, accountID, endpoint string, body []byte) (*http.Response, error) {
	if len(body) > maxRequestBodyBytes {
		return nil, fmt.Errorf("gemini: request body %d bytes exceeds the %d byte limit", len(body), maxRequestBodyBytes)
	}
	target := strings.TrimRight(endpoint, "/") + streamPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for _, kv := range geminiHeaders(accessToken) {
		req.Header.Set(kv[0], kv[1])
	}
	client, err := c.httpFor(accountID)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}

// loadCodeAssist probes the project id, trying sandbox first then daily — the
// order resolveGeminiProject uses.
func (c *upstreamClient) loadCodeAssist(ctx context.Context, accessToken, accountID string, endpoints []string) (project string, probed bool, err error) {
	var lastErr string
	for _, endpoint := range endpoints {
		raw, callErr := c.postJSON(ctx, accessToken, accountID, endpoint+codeAssistPth, []byte(loadCodeAssistBody), creditsTimeout)
		if callErr != nil {
			lastErr = callErr.Error()
			continue
		}
		project = parseProject(raw)
		if project == "" {
			return defaultProject, true, nil
		}
		return project, true, nil
	}
	if lastErr == "" {
		lastErr = "loadCodeAssist probe failed"
	}
	return "", false, fmt.Errorf("gemini: project probe failed: %s", lastErr)
}

// parseProject is parseGeminiProject: top-level cloudaicompanionProject, then the
// one nested inside currentTier.
func parseProject(raw []byte) string {
	var payload struct {
		CloudaicompanionProject string `json:"cloudaicompanionProject"`
		CurrentTier             *struct {
			CloudaicompanionProject string `json:"cloudaicompanionProject"`
		} `json:"currentTier"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return ""
	}
	if payload.CloudaicompanionProject != "" {
		return payload.CloudaicompanionProject
	}
	if payload.CurrentTier != nil {
		return payload.CurrentTier.CloudaicompanionProject
	}
	return ""
}

// quota POSTs /v1internal:retrieveUserQuotaSummary with {"project": id}. The
// reference notes an empty object 403s for a second account, so project is
// mandatory.
func (c *upstreamClient) quota(ctx context.Context, accessToken, accountID, endpoint, project string) ([]byte, error) {
	body, err := json.Marshal(map[string]string{"project": project})
	if err != nil {
		return nil, err
	}
	return c.postJSON(ctx, accessToken, accountID, strings.TrimRight(endpoint, "/")+quotaPath, body, creditsTimeout)
}

// postJSON POSTs a JSON body with the identity headers and returns the body.
func (c *upstreamClient) postJSON(ctx context.Context, accessToken, accountID, target string, body []byte, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for _, kv := range geminiHeaders(accessToken) {
		req.Header.Set(kv[0], kv[1])
	}
	client, err := c.httpFor(accountID)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("gemini: HTTP %d: %s", resp.StatusCode, truncate(string(data), 256))
	}
	return data, nil
}

// newRequestID is newGeminiRequestId: "agent/<ms>/<8 hex>".
func newRequestID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("agent/%d/%s", time.Now().UnixMilli(), hex.EncodeToString(b[:]))
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// firstNonEmpty returns the first value that is not blank.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
