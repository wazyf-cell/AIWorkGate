//go:build windows

package hosts

import (
	"strings"

	"golang.org/x/sys/windows/registry"
)

const internetSettingsKey = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

// ReadSystemProxy reads the current Windows proxy selection. It returns
// (zero, nil) when the switch is off or no proxy is named; ErrSystemProxyPAC
// when Windows is configured through an auto-config script.
func ReadSystemProxy() (SystemProxy, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, internetSettingsKey, registry.QUERY_VALUE)
	if err != nil {
		return SystemProxy{}, err
	}
	defer key.Close()

	enabled := uint64(0)
	if value, _, err := key.GetIntegerValue("ProxyEnable"); err == nil {
		enabled = value
	}
	server, _, _ := key.GetStringValue("ProxyServer")
	pac, _, _ := key.GetStringValue("AutoConfigURL")
	if enabled == 0 {
		return SystemProxy{}, nil
	}
	if strings.TrimSpace(server) == "" {
		if strings.TrimSpace(pac) != "" {
			return SystemProxy{}, ErrSystemProxyPAC
		}
		return SystemProxy{}, nil
	}
	return ParseSystemProxy(server)
}
