package gateway

// D-40: the global /v1 source-address allowlist. It runs before authentication
// so an unlisted network cannot even reach the key check, and it never reads
// X-Forwarded-For (trusted proxies are a separate, not-yet-built feature), so a
// direct client cannot spoof its way in.

import (
	"net"
	"net/http"
	"strings"
)

type ipAllowlist struct {
	allowAll bool
	exact    map[string]bool
	networks []*net.IPNet
}

// parseAllowlist turns config entries into matchers. A nil slice means "no
// restriction" (tests and standalone gateways); any non-nil slice that yields
// no valid entry fails safe to loopback only.
func parseAllowlist(entries []string) ipAllowlist {
	if entries == nil {
		return ipAllowlist{allowAll: true}
	}
	list := ipAllowlist{exact: make(map[string]bool, len(entries))}
	for _, raw := range entries {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		if ip := net.ParseIP(value); ip != nil {
			list.exact[ip.String()] = true
			continue
		}
		if _, network, err := net.ParseCIDR(value); err == nil {
			list.networks = append(list.networks, network)
		}
	}
	if len(list.exact) == 0 && len(list.networks) == 0 {
		list.exact["127.0.0.1"] = true
		list.exact["::1"] = true
	}
	return list
}

func (l ipAllowlist) allows(ip net.IP) bool {
	if l.allowAll {
		return true
	}
	if ip == nil {
		return false
	}
	if l.exact[ip.String()] {
		return true
	}
	v4 := ip.To4()
	if v4 != nil && l.exact[v4.String()] {
		return true
	}
	for _, network := range l.networks {
		if network.Contains(ip) || (v4 != nil && network.Contains(v4)) {
			return true
		}
	}
	return false
}

// SetAllowIPs replaces the allowlist while the gateway serves traffic.
func (g *Gateway) SetAllowIPs(entries []string) {
	if g == nil {
		return
	}
	g.allowIPs.Store(parseAllowlist(entries))
}

// ipAllowed checks the direct peer address; X-Forwarded-For is deliberately
// ignored until trusted_proxies exists (P5-4).
func (g *Gateway) ipAllowed(r *http.Request) bool {
	stored := g.allowIPs.Load()
	if stored == nil {
		return true // zero-value gateway: no allowlist configured
	}
	list, ok := stored.(ipAllowlist)
	if !ok {
		return true
	}
	host := strings.TrimSpace(r.RemoteAddr)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return list.allows(net.ParseIP(host))
}
