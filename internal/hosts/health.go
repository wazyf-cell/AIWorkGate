package hosts

import (
	"context"
	"errors"
	"net/http"
)

// AState is the three-state health classification of A's gateway, derived
// from GET /health instead of a bare TCP probe.
type AState string

const (
	// AStateOK means A answers GET /health.
	AStateOK AState = "ok"
	// AStateUnauthorized means A is up but rejected the API key (401/403);
	// this also covers "no A API Key stored".
	AStateUnauthorized AState = "unauthorized"
	// AStateDown means nothing is listening or the request failed outright.
	AStateDown AState = "down"
)

// HealthState calls GET /health and maps the result: 2xx -> ok, 401/403 ->
// unauthorized, transport errors or any other status -> down. The detail is
// empty on ok and the underlying error text otherwise.
func (c *Client) HealthState(ctx context.Context) (AState, string) {
	err := c.Health(ctx)
	if err == nil {
		return AStateOK, ""
	}
	var he *HTTPError
	if errors.As(err, &he) &&
		(he.StatusCode == http.StatusUnauthorized || he.StatusCode == http.StatusForbidden) {
		return AStateUnauthorized, err.Error()
	}
	return AStateDown, err.Error()
}
