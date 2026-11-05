package app

// P10-2 接线（task-21）：config.redact → gateway 脱敏配置的映射。
// 独立成文件，避免在 grand 的 New 调用附近塞函数导致的编排噪音。

import (
	"aiworkgate/internal/config"
	"aiworkgate/internal/gateway"
)

// redactOptionsFromConfig maps the config.redact section to the gateway's
// redaction wiring. The zero value (enabled=false) leaves forwarding
// untouched, so a host with no redact section behaves exactly as before.
func redactOptionsFromConfig(c config.Config) gateway.RedactOptions {
	return gateway.RedactOptions{
		On:         c.Redact.Enabled,
		KeyFile:    c.Redact.KeyFile,
		RouteAware: c.Redact.RouteAware,
		Kinds:      c.Redact.Kinds,
		Categories: c.Redact.Categories,
	}
}
