package hosts

// Windows system proxy discovery. The host mirrors the fixed proxy Windows is
// configured to use (Internet Settings -> ProxyServer) as a managed proxy row,
// so "follow the system proxy" needs no per-machine typing. PAC scripts are
// deliberately not followed: a PAC picks a route per URL, while the egress
// forwarder needs one fixed upstream for every CONNECT.

import (
	"errors"
	"net"
	"strconv"
	"strings"
)

// ErrSystemProxyPAC reports an auto-config (PAC) setup. It is not an error the
// caller retries: the row simply stays disabled and says why.
var ErrSystemProxyPAC = errors.New("system proxy uses a PAC script")

// SystemProxy is the fixed upstream proxy Windows currently selects.
type SystemProxy struct {
	// Configured is true only when Windows has the proxy switch on and names a
	// usable host:port. A stale ProxyServer with the switch off is not used.
	Configured bool
	Type       string // "http" or "socks5"
	Host       string
	Port       int
}

// ParseSystemProxy turns the ProxyServer value into a proxy endpoint. Accepted
// shapes (Windows writes either form):
//
//	127.0.0.1:7897
//	http=127.0.0.1:7897;https=127.0.0.1:7897
//	socks=127.0.0.1:7891
//	http://127.0.0.1:7897
//
// Preference within the per-protocol form is https, then http, then socks.
func ParseSystemProxy(raw string) (SystemProxy, error) {
	server := strings.TrimSpace(raw)
	if server == "" {
		return SystemProxy{}, nil
	}
	kind := "http"
	if strings.Contains(server, "=") {
		var https, http, socks string
		for _, field := range strings.Split(server, ";") {
			key, value, ok := strings.Cut(field, "=")
			if !ok {
				continue
			}
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "https":
				https = strings.TrimSpace(value)
			case "http":
				http = strings.TrimSpace(value)
			case "socks", "socks5":
				socks = strings.TrimSpace(value)
			}
		}
		switch {
		case https != "":
			server = https
		case http != "":
			server = http
		case socks != "":
			server, kind = socks, "socks5"
		default:
			return SystemProxy{}, nil
		}
	}
	lowered := strings.ToLower(server)
	switch {
	case strings.HasPrefix(lowered, "socks5://"):
		kind, server = "socks5", server[len("socks5://"):]
	case strings.HasPrefix(lowered, "socks://"):
		kind, server = "socks5", server[len("socks://"):]
	case strings.HasPrefix(lowered, "http://"):
		server = server[len("http://"):]
	case strings.HasPrefix(lowered, "https://"):
		server = server[len("https://"):]
	}
	host, portText, err := net.SplitHostPort(strings.TrimSpace(server))
	if err != nil {
		return SystemProxy{}, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 || port > 65535 || strings.TrimSpace(host) == "" {
		return SystemProxy{}, errors.New("system proxy has an invalid host:port")
	}
	return SystemProxy{Configured: true, Type: kind, Host: host, Port: port}, nil
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
