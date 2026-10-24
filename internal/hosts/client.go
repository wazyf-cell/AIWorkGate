package hosts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"aiworkgate/internal/model"
)

const (
	modelsPath = "/v1/models"
	healthPath = "/health"

	// DefaultHTTPTimeout is the per-request timeout for A gateway calls.
	// Model listing and health are cheap; long chat streams are proxied by
	// the gateway layer, not by this client.
	DefaultHTTPTimeout = 10 * time.Second

	// maxErrorBody caps how much of an error response is quoted.
	maxErrorBody = 512
)

// Client talks to A's built-in OpenAI-compatible gateway.
// The API key cannot be imported from A (F73); the caller supplies it.
type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

// NewClient returns a client for baseURL (e.g. "http://127.0.0.1:7864").
// A trailing slash is tolerated; apiKey may be empty for auth_disabled setups.
func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		APIKey:  strings.TrimSpace(apiKey),
		HTTP:    &http.Client{Timeout: DefaultHTTPTimeout},
	}
}

// The /v1/models payload is decoded by model.ParseModelList. The local
// modelEntry/modelsResponse pair that used to live here understood only
// id/name/display/context_length/context_window/tools/vision/supports_image,
// which is why vendor, multiplier, thinking and free came back empty for every
// model reached through this client.

// Models calls GET /v1/models with a Bearer key and returns the model list.
// Entries without an id are skipped; an empty list is not an error.
func (c *Client) Models(ctx context.Context) ([]model.ModelInfo, error) {
	body, err := c.get(ctx, modelsPath)
	if err != nil {
		return nil, err
	}
	// Shared tolerant parser: this hand-rolled copy used to map only id/name/
	// context/vision and dropped vendor, multiplier, thinking and free, so the
	// catalog page showed those columns empty for every model reached this way.
	out, err := model.ParseModelList(body)
	if err != nil {
		return nil, fmt.Errorf("hosts: decode %s: %w", modelsPath, err)
	}
	kept := make([]model.ModelInfo, 0, len(out))
	for _, e := range out {
		if strings.TrimSpace(e.ID) == "" {
			continue
		}
		kept = append(kept, e)
	}
	return out, nil
}

// Health calls GET /health. It succeeds only on a 2xx response whose JSON
// does not report a non-ok status; any transport or HTTP error is returned.
func (c *Client) Health(ctx context.Context) error {
	body, err := c.get(ctx, healthPath)
	if err != nil {
		return err
	}
	var payload struct {
		Status  string `json:"status"`
		Running *bool  `json:"running"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		// A answered with 2xx; a non-JSON body still means the gateway is up.
		return nil
	}
	if payload.Status != "" && !strings.EqualFold(payload.Status, "ok") {
		return fmt.Errorf("hosts: A gateway unhealthy: status=%q", payload.Status)
	}
	if payload.Running != nil && !*payload.Running {
		return errors.New("hosts: A gateway reports running=false")
	}
	return nil
}

// HTTPError is a non-2xx response from A's gateway. The text keeps the
// historical "hosts: GET path: HTTP code: body" format so callers that match
// on messages are unaffected.
type HTTPError struct {
	Path       string
	StatusCode int
	Body       string // already capped at maxErrorBody
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("hosts: GET %s: HTTP %d: %s", e.Path, e.StatusCode, e.Body)
}

// get performs an authenticated GET against the A gateway and returns the body.
func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	if c == nil {
		return nil, errors.New("hosts: nil client")
	}
	if c.BaseURL == "" {
		return nil, errors.New("hosts: empty base URL")
	}
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: DefaultHTTPTimeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("hosts: build request %s: %w", path, err)
	}
	req.Header.Set("Accept", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hosts: GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet := strings.TrimSpace(string(body))
		if len(snippet) > maxErrorBody {
			snippet = snippet[:maxErrorBody] + "..."
		}
		return nil, &HTTPError{Path: path, StatusCode: resp.StatusCode, Body: snippet}
	}
	if readErr != nil {
		return nil, fmt.Errorf("hosts: read %s: %w", path, readErr)
	}
	return body, nil
}
