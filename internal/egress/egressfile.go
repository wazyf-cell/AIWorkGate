package egress

import (
	"strings"

	"aiworkgate/internal/model"
)

// BuildEgressFile resolves, for every account of a plugin, the egress entry the
// plugin will enforce. It never includes proxy credentials, only the local
// forwarder URL.
//
// localURL is asked per (plugin, account, proxy) so each account gets its own
// listener and the host can attribute the traffic it carries.
//
// Resolution rules:
//   - The default entry is defaultMode (or "direct" when empty). In strict mode
//     a direct default becomes "blocked".
//   - An account without an assignment inherits the default entry.
//   - An account assigned to "proxy" gets {mode:"proxy", url, proxy_id} when
//     that proxy's forwarder is running; otherwise it becomes "blocked" in
//     strict mode and "direct" in non-strict mode.
//   - Explicit "direct" / "blocked" assignments are preserved; the plugin
//     itself rejects "direct" in strict mode (contract §18.4).
//   - A "direct" entry also carries the url/proxy_id of the host's direct
//     listener when one exists. The mode stays "direct", so a plugin that
//     dials by itself is unaffected (fail-open preserved) while a plugin that
//     honours the url gets its traffic recorded in proxy_logs. When direct
//     logging is off, localURL reports false and the entry keeps the shape it
//     always had: {"mode":"direct"}.
func BuildEgressFile(pluginID string, strict bool, defaultMode string, accounts []string, assignments []model.AccountEgress, localURL func(pluginID, accountID, proxyID string) (string, bool)) model.EgressFile {
	if localURL == nil {
		localURL = func(string, string, string) (string, bool) { return "", false }
	}

	def := model.EgressEntry{Mode: normalizeDefaultMode(defaultMode)}
	if strict && def.Mode == model.EgressDirect {
		def.Mode = model.EgressBlocked
	}
	// The file-level default covers every account without an assignment, so it
	// points at the plugin-wide listener (no account).
	def = withDirectEgress(def, strict, pluginID, "", localURL)

	file := model.EgressFile{
		Version:  1,
		Strict:   strict,
		Default:  def,
		Accounts: make(map[string]model.EgressEntry, len(accounts)),
	}

	byAccount := make(map[string]model.AccountEgress, len(assignments))
	for _, a := range assignments {
		if a.AccountID == "" {
			continue
		}
		if a.PluginID != "" && pluginID != "" && a.PluginID != pluginID {
			continue
		}
		byAccount[a.AccountID] = a
	}

	for _, account := range accounts {
		if account == "" {
			continue
		}
		a, ok := byAccount[account]
		if !ok {
			// Inheriting the default keeps the per-account listener, otherwise
			// every unassigned account would share the plugin-wide one and its
			// rows could not be attributed.
			inherited := def
			inherited.URL, inherited.ProxyID = "", ""
			file.Accounts[account] = withDirectEgress(inherited, strict, pluginID, account, localURL)
			continue
		}
		file.Accounts[account] = resolveAssignment(a, strict, pluginID, account, localURL)
	}
	return file
}

// withDirectEgress attaches the direct listener of (pluginID, accountID) to a
// "direct" entry. Anything else passes through untouched.
//
// Strict mode never attaches a listener: strict promises that an account without
// a proxy has no usable exit, so advertising a working one - even a loopback
// listener that only records - would hand the plugin a way around that promise.
func withDirectEgress(entry model.EgressEntry, strict bool, pluginID, accountID string, localURL func(pluginID, accountID, proxyID string) (string, bool)) model.EgressEntry {
	if entry.Mode != model.EgressDirect || strict {
		return entry
	}
	u, ok := localURL(pluginID, accountID, DirectProxyID)
	if !ok || strings.TrimSpace(u) == "" {
		return entry
	}
	entry.URL = u
	entry.ProxyID = DirectProxyID
	return entry
}

func resolveAssignment(a model.AccountEgress, strict bool, pluginID, accountID string, localURL func(pluginID, accountID, proxyID string) (string, bool)) model.EgressEntry {
	mode := strings.ToLower(strings.TrimSpace(a.Mode))
	if mode == "" {
		if a.ProxyID != "" {
			mode = model.EgressProxy
		} else {
			mode = model.EgressDirect
		}
	}

	switch mode {
	case model.EgressBlocked:
		return model.EgressEntry{Mode: model.EgressBlocked}
	case model.EgressDirect:
		return withDirectEgress(model.EgressEntry{Mode: model.EgressDirect}, strict, pluginID, accountID, localURL)
	case model.EgressProxy:
		if a.ProxyID != "" && !IsDirectProxyID(a.ProxyID) {
			if u, ok := localURL(pluginID, accountID, a.ProxyID); ok && u != "" {
				return model.EgressEntry{Mode: model.EgressProxy, URL: u, ProxyID: a.ProxyID}
			}
		}
		if strict {
			return model.EgressEntry{Mode: model.EgressBlocked}
		}
		// Unresolvable proxy assignment: non-strict mode degrades to direct,
		// and that direct egress is recorded like any other.
		return withDirectEgress(model.EgressEntry{Mode: model.EgressDirect}, strict, pluginID, accountID, localURL)
	default:
		// Unknown assignment modes are treated conservatively.
		if strict {
			return model.EgressEntry{Mode: model.EgressBlocked}
		}
		return withDirectEgress(model.EgressEntry{Mode: model.EgressDirect}, strict, pluginID, accountID, localURL)
	}
}

func normalizeDefaultMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case model.EgressProxy:
		return model.EgressProxy
	case model.EgressBlocked:
		return model.EgressBlocked
	default:
		return model.EgressDirect
	}
}
