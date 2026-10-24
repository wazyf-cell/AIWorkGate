package app

import (
	"errors"
	"sync"
	"time"

	"aiworkgate/internal/hosts"
	"aiworkgate/internal/model"
	"aiworkgate/internal/safego"
	"aiworkgate/internal/store"
)

// systemProxySyncInterval is how often the reserved "system" proxy row is
// compared with Windows Internet Settings. The registry has no change
// notification the host can rely on, and a 30s poll costs nothing.
const systemProxySyncInterval = 30 * time.Second

// errSystemProxyManaged rejects edits to the reserved row: its endpoint is read
// from Windows, so a manual save would be overwritten on the next poll.
var errSystemProxyManaged = errors.New("系统代理行由宿主跟随 Windows 设置维护，不能手动修改或删除")

// systemProxyState holds the human-readable reason the managed row is off. It is
// derived on read, so a Windows change shows up without a separate API.
type systemProxyState struct {
	mu   sync.Mutex
	note string
}

func (s *systemProxyState) set(note string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.note = note
}

func (s *systemProxyState) get() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.note
}

// startSystemProxySync mirrors the Windows proxy once before the forwarders
// start, then keeps watching it. It runs in the background so a registry read
// that hangs (a broken policy provider) cannot delay startup.
func (a *App) startSystemProxySync() {
	if a == nil || a.store == nil || a.egressMgr == nil {
		return
	}
	a.syncSystemProxy()
	safego.Go(a.logger, "systemProxySync", func() {
		ticker := time.NewTicker(systemProxySyncInterval)
		defer ticker.Stop()
		for {
			select {
			case <-a.shutdownCh:
				return
			case <-ticker.C:
				a.syncSystemProxy()
			}
		}
	})
}

// syncSystemProxy keeps the reserved proxy row equal to the fixed proxy Windows
// is configured to use. The row is the binding target for "follow the system
// proxy"; Windows remains the source of truth, so the row is never editable.
//
// A disabled Windows proxy, or a PAC script (which picks a route per URL and
// therefore cannot back a single fixed upstream), leaves the row disabled with
// the reason exposed through ManagedDetail. Accounts bound to it then follow the
// normal disabled-proxy rules: direct in non-strict plugins, blocked in strict
// ones.
func (a *App) syncSystemProxy() bool {
	if a == nil || a.store == nil || a.egressMgr == nil {
		return false
	}
	read := a.systemProxyRead
	if read == nil {
		read = hosts.ReadSystemProxy
	}
	sp, err := read()

	desired := model.Proxy{
		ID:      model.SystemProxyID,
		Name:    model.SystemProxyName,
		Enabled: false,
	}
	note := ""
	switch {
	case errors.Is(err, hosts.ErrSystemProxyPAC):
		note = "Windows 使用 PAC 自动配置，暂不支持跟随；请改用手动代理。"
	case err != nil:
		note = "无法读取 Windows 代理设置：" + err.Error()
	case !sp.Configured:
		note = "Windows 未开启系统代理，开启后自动同步。"
	default:
		desired.Type, desired.Host, desired.Port = sp.Type, sp.Host, sp.Port
		desired.Enabled = true
	}

	current, _, getErr := a.store.GetProxy(model.SystemProxyID)
	if getErr != nil && !errors.Is(getErr, store.ErrNotFound) {
		a.logger.Warn("app", "cannot read the system proxy row", map[string]any{"error": getErr.Error()})
		return false
	}
	if getErr == nil {
		desired.LastCheckAt = current.LastCheckAt
		desired.LastExitIP = current.LastExitIP
		desired.LastLatencyMS = current.LastLatencyMS
		desired.LastError = current.LastError
		if current.Type == desired.Type && current.Host == desired.Host &&
			current.Port == desired.Port && current.Enabled == desired.Enabled {
			a.systemProxy.set(note)
			return false
		}
	}

	if err := a.store.UpsertProxy(desired, ""); err != nil {
		a.logger.Warn("app", "cannot save the system proxy row", map[string]any{"error": err.Error()})
		return false
	}
	a.systemProxy.set(note)

	var forwarderErr error
	if desired.Enabled {
		forwarderErr = a.egressMgr.StartProxy(desired)
	} else {
		forwarderErr = a.egressMgr.StopProxy(model.SystemProxyID)
	}
	if forwarderErr != nil {
		a.logger.Warn("app", "system proxy forwarder failed", map[string]any{"error": forwarderErr.Error()})
	}
	if a.egressHealth != nil {
		a.egressHealth.InvalidateAll()
	}
	a.logger.Info("app", "system proxy synced", map[string]any{
		"enabled": desired.Enabled, "type": desired.Type,
		"host": desired.Host, "port": desired.Port, "note": note,
	})
	a.writeAllEgress()
	a.reloadEgressPlugins()
	return true
}

// systemProxyNote returns the current reason the managed row is disabled.
func (a *App) systemProxyNote() string {
	if a == nil {
		return ""
	}
	return a.systemProxy.get()
}
