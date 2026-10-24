package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"aiworkgate/internal/model"
)

// Client timeouts from contract section 6. Health checks are cheap, control
// calls may do network I/O, check-in can take minutes.
const (
	controlTimeout = 60 * time.Second
	checkinTimeout = 5 * time.Minute
	// identityTimeout covers an OS-level identity switch, which may launch
	// helper processes before the plugin answers.
	identityTimeout = 20 * time.Second

	// responseHeaderTimeout bounds the wait for response headers on a plugin
	// call. It is transport-level on purpose: unlike http.Client.Timeout it
	// does not cap the response body, so an SSE body can stream for as long as
	// the gateway's request context allows.
	responseHeaderTimeout = 90 * time.Second

	maxResponseBytes = 8 << 20
)

var (
	pluginTransportOnce sync.Once
	pluginTransport     *http.Transport
)

// pluginHTTPTransport is the shared loopback transport for plugin traffic. It
// has no overall deadline (the caller's context or the per-call timeout bounds
// the request) but does fail a request whose headers never arrive.
//
// The streaming proxy of /v1/chat/completions lives in internal/gateway and
// uses its own transport; it must keep relying on the request context for
// cancellation and add the same ResponseHeaderTimeout there. Nothing in this
// package proxies a stream, so no http.Client.Timeout here can truncate one.
func pluginHTTPTransport() *http.Transport {
	pluginTransportOnce.Do(func() {
		pluginTransport = &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ResponseHeaderTimeout: responseHeaderTimeout,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConnsPerHost:   8,
			// Loopback only: never send plugin traffic through an env proxy.
			Proxy: nil,
		}
	})
	return pluginTransport
}

// HTTPError is a non-2xx answer from a plugin, carrying the OpenAI-style error
// message when the plugin supplied one.
type HTTPError struct {
	Status  int
	Message string
}

func (e *HTTPError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("plugins: http %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("plugins: http %d: %s", e.Status, http.StatusText(e.Status))
}

// HTTPStatus exposes the plugin's own status code so the admin API can answer
// with the same class of error. A plugin rejecting a bad code with 400 is a
// user error, not a gateway failure, and must not surface as 502.
func (e *HTTPError) HTTPStatus() int { return e.Status }

// Meta is the runtime information returned by GET /meta.
type Meta struct {
	ID           string   `json:"id"`
	Name         string   `json:"name,omitempty"`
	Version      string   `json:"version"`
	APILevel     int      `json:"api_level"`
	Capabilities []string `json:"capabilities,omitempty"`
	AccountCount int      `json:"account_count"`
	UptimeS      int64    `json:"uptime_s"`
	StartedAt    int64    `json:"started_at,omitempty"`
}

// Accounts fetches GET /accounts with inline quota.
func (m *Manager) Accounts(ctx context.Context, id string) ([]model.Account, error) {
	var payload model.AccountsResponse
	if err := m.call(ctx, id, http.MethodGet, "/accounts", nil, nil, &payload, controlTimeout); err != nil {
		return nil, err
	}
	return payload.Accounts, nil
}

// Checkin runs POST /accounts/{id}/checkin (5 minute timeout).
func (m *Manager) Checkin(ctx context.Context, id, accountID string) (model.CheckinResult, error) {
	var out model.CheckinResult
	path := "/accounts/" + url.PathEscape(accountID) + "/checkin"
	if err := m.call(ctx, id, http.MethodPost, path, nil, struct{}{}, &out, checkinTimeout); err != nil {
		return model.CheckinResult{}, err
	}
	return out, nil
}

// Refresh runs POST /accounts/{id}/refresh.
func (m *Manager) Refresh(ctx context.Context, id, accountID string) (model.RefreshResult, error) {
	var out model.RefreshResult
	path := "/accounts/" + url.PathEscape(accountID) + "/refresh"
	if err := m.call(ctx, id, http.MethodPost, path, nil, struct{}{}, &out, controlTimeout); err != nil {
		return model.RefreshResult{}, err
	}
	return out, nil
}

// LoginStart begins a login session. method is one of api_key, oauth,
// local_state or cookie; an empty method lets the plugin choose.
func (m *Manager) LoginStart(ctx context.Context, id, method string) (model.LoginStart, error) {
	var out model.LoginStart
	body := struct {
		Method string `json:"method,omitempty"`
	}{Method: strings.TrimSpace(method)}
	if err := m.call(ctx, id, http.MethodPost, "/accounts/login", nil, body, &out, controlTimeout); err != nil {
		return model.LoginStart{}, err
	}
	out.NormalizeLoginURL()
	return out, nil
}

// LoginSubmit posts user-supplied form values to
// POST /accounts/login/{sid}/submit.
func (m *Manager) LoginSubmit(ctx context.Context, id, sessionID string, values map[string]string) (model.LoginStatus, error) {
	var out model.LoginStatus
	if values == nil {
		values = map[string]string{}
	}
	path := "/accounts/login/" + url.PathEscape(sessionID) + "/submit"
	if err := m.call(ctx, id, http.MethodPost, path, nil, model.LoginSubmit{Values: values}, &out, controlTimeout); err != nil {
		return model.LoginStatus{}, err
	}
	return out, nil
}

// LoginPoll reads GET /accounts/login/{sid}.
func (m *Manager) LoginPoll(ctx context.Context, id, sessionID string) (model.LoginStatus, error) {
	var out model.LoginStatus
	path := "/accounts/login/" + url.PathEscape(sessionID)
	if err := m.call(ctx, id, http.MethodGet, path, nil, nil, &out, controlTimeout); err != nil {
		return model.LoginStatus{}, err
	}
	return out, nil
}

// LoginCancel abandons a login session. A plugin that does not implement the
// optional endpoint answers 404, which is treated as success.
func (m *Manager) LoginCancel(ctx context.Context, id, sessionID string) error {
	path := "/accounts/login/" + url.PathEscape(sessionID)
	err := m.call(ctx, id, http.MethodDelete, path, nil, nil, nil, controlTimeout)
	var he *HTTPError
	if errors.As(err, &he) && (he.Status == http.StatusNotFound || he.Status == http.StatusMethodNotAllowed) {
		return nil
	}
	return err
}

// PatchAccount applies a partial update such as {"enabled":false}.
func (m *Manager) PatchAccount(ctx context.Context, id, accountID string, patch map[string]any) error {
	if patch == nil {
		patch = map[string]any{}
	}
	path := "/accounts/" + url.PathEscape(accountID)
	return m.call(ctx, id, http.MethodPatch, path, nil, patch, nil, controlTimeout)
}

// ResetIdentity asks the plugin to reset one account's device identity. mode is
// "request" (rotate the per-request identifiers only) or "os" (switch the
// machine-level identity). The host never touches the OS identity itself; it
// only proxies the plugin call.
func (m *Manager) ResetIdentity(ctx context.Context, id, accountID, mode string) error {
	path := "/accounts/" + url.PathEscape(accountID) + "/identity/reset"
	body := struct {
		Mode string `json:"mode"`
	}{Mode: strings.ToLower(strings.TrimSpace(mode))}
	return m.call(ctx, id, http.MethodPost, path, nil, body, nil, identityTimeout)
}

// DeleteAccount removes an account and its credentials.
func (m *Manager) DeleteAccount(ctx context.Context, id, accountID string) error {
	path := "/accounts/" + url.PathEscape(accountID)
	return m.call(ctx, id, http.MethodDelete, path, nil, nil, nil, controlTimeout)
}

// Models fetches GET /v1/models (local, unprefixed ids).
func (m *Manager) Models(ctx context.Context, id string) ([]model.ModelInfo, error) {
	return m.models(ctx, id, false)
}

// ModelsFresh asks the plugin to bypass its own model cache (contract v0.3:
// GET /v1/models?refresh=1). Older plugins simply ignore the query.
func (m *Manager) ModelsFresh(ctx context.Context, id string) ([]model.ModelInfo, error) {
	return m.models(ctx, id, true)
}

func (m *Manager) models(ctx context.Context, id string, refresh bool) ([]model.ModelInfo, error) {
	path := "/v1/models"
	if refresh {
		path += "?refresh=1"
	}
	// Decode through the shared tolerant parser. Decoding straight into
	// model.ModelInfo only understood the canonical spellings, so a plugin that
	// reported "display"/"rate"/"supports_image"/"supported_efforts" (which is
	// what the AI Work 助手 does) had those fields silently dropped.
	var raw json.RawMessage
	if err := m.call(ctx, id, http.MethodGet, path, nil, nil, &raw, controlTimeout); err != nil {
		return nil, err
	}
	out, err := model.ParseModelList(raw)
	if err != nil {
		return nil, err
	}
	kept := make([]model.ModelInfo, 0, len(out))
	for _, e := range out {
		if strings.TrimSpace(e.ID) == "" {
			continue
		}
		kept = append(kept, e)
	}
	return kept, nil
}

// Meta fetches GET /meta.
func (m *Manager) Meta(ctx context.Context, id string) (Meta, error) {
	var out Meta
	if err := m.call(ctx, id, http.MethodGet, "/meta", nil, nil, &out, controlTimeout); err != nil {
		return Meta{}, err
	}
	return out, nil
}

// Reload asks the plugin to re-read its rule pack and egress file. Plugins
// without the optional endpoint (404/501) are ignored.
func (m *Manager) Reload(ctx context.Context, id string) error {
	err := m.call(ctx, id, http.MethodPost, "/reload", nil, struct{}{}, nil, controlTimeout)
	var he *HTTPError
	if errors.As(err, &he) && (he.Status == http.StatusNotFound || he.Status == http.StatusNotImplemented) {
		return nil
	}
	return err
}

// SetUpstream hands the assistant's base_url and api_key to a plugin that keeps
// them itself (POST /admin/upstream, decision Q18). The host never passes the
// key through the plugin environment, so this call is the only channel.
//
// Both values are required: an empty api_key would overwrite a credential the
// plugin already holds with nothing (the plugin stores whatever it is given),
// so it is rejected here instead of being forwarded. The values are never
// included in an error message, only the plugin id and path are.
func (m *Manager) SetUpstream(ctx context.Context, id, baseURL, apiKey string) error {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return errors.New("plugins: set upstream: base_url is required")
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		return fmt.Errorf("plugins: set upstream: base_url %q must be an absolute http(s) URL", base)
	}
	key := strings.TrimSpace(apiKey)
	if key == "" {
		return errors.New("plugins: set upstream: api_key is required")
	}
	body := struct {
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
	}{BaseURL: base, APIKey: key}
	// A 2xx is the whole contract; the plugin's echo ({ok, key_present,
	// key_fingerprint}) is informational and already logged on its own side.
	if err := m.call(ctx, id, http.MethodPost, "/admin/upstream", nil, body, nil, controlTimeout); err != nil {
		return err
	}
	return nil
}

// call ensures the plugin is running, then performs an authenticated JSON
// request against its loopback endpoint.
func (m *Manager) call(ctx context.Context, id, method, path string, query url.Values, body, out any, timeout time.Duration) error {
	if ctx == nil {
		ctx = context.Background()
	}
	// The call counts as in flight from the cold start until the response is
	// read, so the idle reaper never stops the plugin underneath it.
	release := m.BeginCall(id)
	defer release()
	if err := m.EnsureRunning(ctx, id); err != nil {
		return err
	}
	base, token, err := m.endpoint(id)
	if err != nil {
		return err
	}
	target := base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("plugins: %s %s: encode request: %w", id, path, err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return fmt.Errorf("plugins: %s %s: %w", id, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if timeout <= 0 {
		timeout = controlTimeout
	}

	// These calls carry a bounded JSON body, so an overall timeout is correct;
	// the transport still applies responseHeaderTimeout independently.
	client := &http.Client{Timeout: timeout, Transport: pluginHTTPTransport()}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("plugins: %s %s: %w", id, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("plugins: %s %s: read response: %w", id, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &HTTPError{Status: resp.StatusCode, Message: errorMessage(resp.StatusCode, data)}
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("plugins: %s %s: decode response: %w", id, path, err)
		}
	}
	m.Touch(id)
	return nil
}

// errorMessage extracts the OpenAI-style error message from a plugin body.
func errorMessage(status int, data []byte) string {
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &payload); err == nil && strings.TrimSpace(payload.Error.Message) != "" {
		return strings.TrimSpace(payload.Error.Message)
	}
	text := strings.TrimSpace(string(data))
	if len(text) > 256 {
		text = text[:256]
	}
	if text == "" {
		return http.StatusText(status)
	}
	return text
}
