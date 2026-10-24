package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"aiworkgate/internal/fsutil"
	"aiworkgate/internal/model"
)

// writeAllEgress materializes runtime/<plugin>/egress.json for every plugin.
// The file must exist before a plugin starts; strict plugins treat a missing
// entry as "blocked" and are expected to fail closed.
func (a *App) writeAllEgress() {
	if a.pluginMgr == nil || a.egressMgr == nil {
		return
	}
	for _, st := range a.pluginMgr.List() {
		id := strings.TrimSpace(st.Manifest.ID)
		if id == "" {
			continue
		}
		if err := a.writeEgressForPlugin(id, nil); err != nil {
			a.logger.Warn("app", "cannot write egress file", map[string]any{
				"plugin": id, "error": err.Error(),
			})
		}
	}
}

// writeEgressChecked writes one plugin's egress file and leaves a warning when
// it fails. Egress failures are state drift (the plugin may keep an old proxy
// mapping), not request failures, so callers that already committed their own
// change must not turn them into a 500; they must not swallow them either.
func (a *App) writeEgressChecked(id, reason string) {
	if err := a.writeEgressForPlugin(id, nil); err != nil {
		a.logger.Warn("app", "cannot write egress file", map[string]any{
			"plugin": id, "reason": reason, "error": err.Error(),
		})
	}
}

// writeEgressForPlugin writes runtime/<id>/egress.json. When accounts is nil the
// account ids come from the stored assignments, so the file is correct even
// while the plugin is stopped or still starting.
func (a *App) writeEgressForPlugin(id string, accounts []string) error {
	if a.pluginMgr == nil || a.egressMgr == nil {
		return nil
	}
	if _, ok := a.pluginMgr.Get(id); !ok {
		return nil
	}
	if accounts == nil {
		accounts = a.assignedAccountIDs(id)
	}
	strict := a.pluginEgressStrict(id)
	file, err := a.egressMgr.EgressFile(id, strict, model.EgressDirect, accounts)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	_ = a.pluginMgr.RuntimeDir(id)
	// 0600: the file carries account ids and local proxy endpoints.
	return fsutil.WriteFileAtomic(a.pluginMgr.EgressFile(id), data, 0o600)
}

// pluginEgressStrict resolves the strict flag the plugin's egress file carries.
// Precedence: an explicit per-plugin override (row.StrictSet) wins; otherwise
// the manifest declaration or the global plugins.strict_egress.
func (a *App) pluginEgressStrict(id string) bool {
	strict := false
	if st, ok := a.pluginMgr.Get(id); ok {
		strict = st.Manifest.Egress.Strict
	}
	if a.configSnapshot().Plugins.StrictEgress {
		strict = true
	}
	if a.store != nil {
		if view, found, err := a.store.GetPluginSettingsView(id); err == nil && found && view.StrictSet {
			strict = view.Strict
		}
	}
	return strict
}

// directEgressURL answers the plugin supervisor's question "where should this
// plugin's direct egress be recorded?" (see plugins.Options.DirectEgress).
//
// It deliberately answers "" - no listener, no environment change, the behaviour
// every plugin shipped with - unless all of these hold:
//   - the user turned egress.direct_logging on;
//   - no host-wide proxy is configured, because replacing one with a direct
//     listener would move traffic that used to leave through it;
//   - the plugin is not strict, whose default entry is "blocked": handing a
//     strict plugin a working direct exit would be a leak, not a log.
//
// The listener is the plugin-wide one (no account), because a process environment
// cannot name one listener per account. Account-scoped direct listeners exist for
// the per-account path in egress.json.
func (a *App) directEgressURL(pluginID string) string {
	if a.egressMgr == nil || a.pluginMgr == nil {
		return ""
	}
	if a.configSnapshot().Proxy.Enabled {
		return ""
	}
	if !a.egressMgr.DirectLoggingEnabled() {
		return ""
	}
	if a.pluginEgressStrict(pluginID) {
		return ""
	}
	u, ok := a.egressMgr.DirectURLFor(pluginID, "")
	if !ok {
		return ""
	}
	return u
}

// reloadEgressPlugins asks every running plugin that consumes egress.json to
// re-read it. Without this a strict-mode switch would only apply on restart.
func (a *App) reloadEgressPlugins() {
	if a.pluginMgr == nil {
		return
	}
	for _, st := range a.pluginMgr.List() {
		id := strings.TrimSpace(st.Manifest.ID)
		if id == "" || !st.Running || !pluginHasCapability(st.Manifest, model.CapEgress) {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err := a.pluginMgr.Reload(ctx, id)
		cancel()
		if err != nil {
			a.logger.Warn("app", "cannot reload plugin after egress change", map[string]any{
				"plugin": id, "error": err.Error(),
			})
		}
	}
}

// reconcileDirectEgressChildren takes the direct-egress listeners out of the
// environments of the children that were handed one.
//
// egress.direct_logging is injected into a plugin process as HTTP(S)_PROXY at
// launch, and a running process's environment cannot be rewritten from outside.
// So closing the switch - which stops those listeners - would leave every plugin
// that was launched while it was open sending all of its environment-based
// outbound traffic, and its callbacks to the host port, to a port that now
// refuses them, until that plugin happens to restart. Relaunching the affected
// children is therefore the only clean way to apply the switch, and it is done
// here, in the request that made it, so no caller can observe the broken window.
//
// Only children that were actually injected are candidates: a child that was
// launched without a listener keeps dialling by itself (direct mode's fail-open),
// so turning the switch on restarts nothing and changes nothing for a running
// plugin.
//
// A restart does interrupt whatever that plugin is doing, so the plugin ids are
// reported in the host log and in the audit trail; an id whose relaunch failed is
// only in the log, because that child is stopped (or still stale) and needs a
// manual restart.
func (a *App) reconcileDirectEgressChildren() []string {
	if a.pluginMgr == nil {
		return nil
	}
	restarted, err := a.pluginMgr.ReconcileDirectEgress()
	if len(restarted) == 0 && err == nil {
		return nil
	}
	if len(restarted) > 0 {
		a.logger.Info("app", "restarted plugins whose direct egress listener closed", map[string]any{
			"plugins": restarted,
		})
	}
	if err != nil {
		a.logger.Warn("app", "cannot restart plugin whose direct egress listener closed", map[string]any{
			"restarted": restarted, "error": err.Error(),
		})
	}
	result := "ok"
	if err != nil {
		result = "fail"
	}
	a.writeAuditDetail(model.AuditConfig, "direct_egress_restart", "egress.direct_logging",
		strings.Join(restarted, ","), result)
	return restarted
}

// pluginHasCapability reports whether the manifest declares capability.
func pluginHasCapability(mf model.Manifest, capability string) bool {
	for _, c := range mf.Capabilities {
		if c == capability {
			return true
		}
	}
	return false
}

// assignedAccountIDs lists the accounts that have an egress assignment. The
// plugin only needs entries for those; every other account follows the
// plugin's default mode.
func (a *App) assignedAccountIDs(id string) []string {
	if a.store == nil {
		return nil
	}
	assignments, err := a.store.ListAccountEgress(id)
	if err != nil {
		a.logger.Warn("app", "cannot list account egress", map[string]any{
			"plugin": id, "error": err.Error(),
		})
		return nil
	}
	ids := make([]string, 0, len(assignments))
	for _, e := range assignments {
		if e.AccountID != "" {
			ids = append(ids, e.AccountID)
		}
	}
	return ids
}

// setAccountEgress stores one account assignment (or removes it when mode is
// "default" or empty), rewrites the plugin's egress file and asks a running
// plugin to reload. A failed reload is returned as a warning, never as an
// error: the assignment is already durable.
func (a *App) setAccountEgress(pluginID, accountID, mode, proxyID string) (bool, string, error) {
	if a.store == nil || a.pluginMgr == nil || a.egressMgr == nil {
		return false, "", fmt.Errorf("app: egress services are not available")
	}
	pluginID = strings.TrimSpace(pluginID)
	accountID = strings.TrimSpace(accountID)
	mode = strings.ToLower(strings.TrimSpace(mode))
	proxyID = strings.TrimSpace(proxyID)

	switch mode {
	case "", "default":
		if err := a.store.DeleteAccountEgress(pluginID, accountID); err != nil {
			return false, "", err
		}
		mode = "default"
	case model.EgressDirect:
		if err := a.store.SetAccountEgress(model.AccountEgress{
			PluginID: pluginID, AccountID: accountID, Mode: model.EgressDirect,
		}); err != nil {
			return false, "", err
		}
	case model.EgressProxy:
		if proxyID == "" {
			return false, "", fmt.Errorf("app: proxy_id is required for egress mode proxy")
		}
		if err := a.store.SetAccountEgress(model.AccountEgress{
			PluginID: pluginID, AccountID: accountID, Mode: model.EgressProxy, ProxyID: proxyID,
		}); err != nil {
			return false, "", err
		}
	default:
		return false, "", fmt.Errorf("app: unsupported egress mode %q", mode)
	}

	a.logger.Info("app", "account egress assigned", map[string]any{
		"plugin": pluginID, "account": accountID, "mode": mode, "proxy": proxyID,
	})
	if a.egressHealth != nil {
		a.egressHealth.InvalidateAll()
	}

	warning := ""
	if err := a.writeEgressForPlugin(pluginID, nil); err != nil {
		a.logger.Warn("app", "cannot write egress file", map[string]any{
			"plugin": pluginID, "error": err.Error(),
		})
		warning = "cannot write egress file: " + err.Error()
	}

	st, ok := a.pluginMgr.Get(pluginID)
	if !ok || !st.Running {
		return false, warning, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := a.pluginMgr.Reload(ctx, pluginID); err != nil {
		a.logger.Warn("app", "cannot reload plugin after egress change", map[string]any{
			"plugin": pluginID, "error": err.Error(),
		})
		return false, "plugin reload failed: " + err.Error(), nil
	}
	return true, warning, nil
}
