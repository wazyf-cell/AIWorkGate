package egress

// Direct-egress listeners.
//
// A direct listener is structurally the same loopback CONNECT listener a proxy
// forwarder binds (same lifecycle, same accounting, same proxy_logs rows); the
// only difference is what it dials: the CONNECT target itself, with no upstream
// proxy in between. That is what lets an account in "direct" mode be recorded
// instead of disappearing into the plugin's line log.
//
// Direct mode exists precisely because the host must NOT get in the way, so a
// direct listener keeps fail-open semantics on three fronts:
//
//   - The only dial it performs is the dial the plugin would have made itself,
//     so a dial failure is a failure the plugin had anyway. The listener adds
//     no new way to lose a connection.
//   - Accounting is best effort and isolated (see logProxy): a sink that errors
//     or panics can never change the outcome of the tunnel it describes.
//   - There is deliberately no "listener broke, use another path" fallback
//     inside the forwarder. A hidden second egress path would undo the point of
//     binding one listener per (plugin, account, direct): the row in
//     proxy_logs would no longer describe the bytes that actually left.
//
// Listeners are created lazily and only while direct logging is enabled on the
// Manager, so a host that never turns the feature on binds no extra ports.

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"aiworkgate/internal/model"
)

const (
	// ProxyTypeDirect is the pseudo proxy type of a direct listener. It is not
	// a row in the proxies table: there is no upstream to describe.
	ProxyTypeDirect = "direct"

	// DirectProxyID is the fixed proxy_id of every direct-egress row.
	DirectProxyID = "direct"

	// DirectProxyLabel is the human-readable proxy_label stored with
	// DirectProxyID. Rows are read by people on the Logs page, so it is
	// Chinese like the rest of that surface.
	DirectProxyLabel = "直连"
)

// DirectProxy returns the synthetic proxy description that turns a Forwarder
// into a direct listener. It carries no host, port or credentials by design: a
// direct listener must not be able to talk to an upstream.
func DirectProxy() model.Proxy {
	return model.Proxy{
		ID:      DirectProxyID,
		Name:    DirectProxyLabel,
		Type:    ProxyTypeDirect,
		Enabled: true,
	}
}

// IsDirectProxyID reports whether id names the synthetic direct egress target.
func IsDirectProxyID(id string) bool {
	return strings.EqualFold(strings.TrimSpace(id), DirectProxyID)
}

// StartDirect binds a direct-egress listener for one (plugin, account) pair and
// returns its loopback URL, exactly like StartFor does for an upstream proxy.
func (f *Forwarder) StartDirect(id Identity, sink LogSink) (string, error) {
	return f.StartFor(DirectProxy(), id, sink)
}

// dialDirect connects straight to the CONNECT target. DNS resolves locally, as
// it did when the plugin dialled the target itself.
func dialDirect(ctx context.Context, target string, dialTimeout time.Duration) (net.Conn, error) {
	d := &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", target)
	if err != nil {
		return nil, fmt.Errorf("egress: direct dial %s: %w", target, err)
	}
	return conn, nil
}
