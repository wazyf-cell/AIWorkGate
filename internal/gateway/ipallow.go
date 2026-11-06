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

// SetTrustedProxies replaces the reverse proxies whose X-Forwarded-For is
// believed. An empty list means the header is never trusted.
func (g *Gateway) SetTrustedProxies(entries []string) {
	if g == nil {
		return
	}
	if len(entries) == 0 {
		g.trustedProxies.Store(ipAllowlist{})
		return
	}
	matcher := parseAllowlist(entries)
	matcher.allowAll = false
	g.trustedProxies.Store(matcher)
}

// trustedProxyList returns the current matcher; the zero value trusts nobody.
func (g *Gateway) trustedProxyList() ipAllowlist {
	stored := g.trustedProxies.Load()
	if stored == nil {
		return ipAllowlist{}
	}
	list, _ := stored.(ipAllowlist)
	return list
}

// clientIP is the address the allowlist judges. The direct peer is the source
// unless that peer is a configured reverse proxy, in which case the first
// address from the right of X-Forwarded-For that is not itself a trusted proxy
// is the client (P5-4). Everything else stays a direct address, so a client
// cannot spoof its way past D-40.
func (g *Gateway) clientIP(r *http.Request) net.IP {
	peer := strings.TrimSpace(r.RemoteAddr)
	if h, _, err := net.SplitHostPort(peer); err == nil {
		peer = h
	}
	peerIP := net.ParseIP(peer)
	trusted := g.trustedProxyList()
	if peerIP == nil || !trusted.allows(peerIP) {
		return peerIP
	}
	parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.TrimSpace(parts[i]))
		if ip == nil {
			continue
		}
		if trusted.allows(ip) {
			continue
		}
		return ip
	}
	return peerIP
}

// ipAllowed checks the effective client address (see clientIP).
func (g *Gateway) ipAllowed(r *http.Request) bool {
	stored := g.allowIPs.Load()
	if stored == nil {
		return true // zero-value gateway: no allowlist configured
	}
	list, ok := stored.(ipAllowlist)
	if !ok {
		return true
	}
	return list.allows(g.clientIP(r))
}
