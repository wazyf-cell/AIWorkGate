package app

import (
	"aiworkgate/internal/plugins"
)

// initUpdateTimer wires the P2-5 market update check into the host lifecycle.
// The timer is only created when the configured interval is positive
// (plugins.market_update_interval_minutes, 0 = off); its policy comes from the
// same config, defaulting to prompt (record available updates, never install).
// Run starts it with a context that Shutdown cancels before the plugin manager
// and the store close.
func (a *App) initUpdateTimer() {
	if a.pluginMgr == nil || a.store == nil {
		return
	}
	if a.configSnapshot().Plugins.MarketUpdateIntervalMinutes <= 0 {
		return
	}
	a.updateTimer = plugins.NewUpdateTimer(a.pluginMgr, a.store, a.configSnapshot)
}
