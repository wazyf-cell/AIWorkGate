package app

import (
	"net"
	"strings"
)

// allowedRequestHost rejects DNS-rebinding attacks on the loopback admin API.
// The same-origin guard in internal/webui compares Origin with the request's
// own Host, so an attacker page served from evil.example:7865 that re-resolves
// to 127.0.0.1 would otherwise look same-origin and could read the admin API
// and obtain the session cookie. Only loopback names are accepted on a local
// install; LAN mode additionally accepts IP literals (a DNS name is exactly
// what rebinding needs). Ports are ignored.
func allowedRequestHost(host string, listenLAN bool) bool {
	h := strings.TrimSpace(host)
	if h == "" {
		return false
	}
	if hn, _, err := net.SplitHostPort(h); err == nil {
		h = hn
	} else {
		h = strings.Trim(h, "[]")
	}
	h = strings.Trim(h, "[]")
	switch strings.ToLower(h) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	if !listenLAN {
		return false
	}
	return net.ParseIP(h) != nil
}
