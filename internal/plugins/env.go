package plugins

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"aiworkgate/internal/config"
	"aiworkgate/internal/model"
)

// envAPILevel is the name of the variable that tells a plugin which contract
// level the host speaks. The value must mirror the plugin's own declaration:
// the manifest field is api_level (model.Manifest.APILevel) and the host
// validates it to MinAPILevel..MaxAPILevel before a plugin is ever launched.
//
// v0.3 hardcoded this to "1" for every plugin, so a manifest declaring
// api_level 2 was still told "1". That made the plugin's own capability
// dispatch (forced-account headers, and later the embeddings passthrough)
// undecidable: the plugin could not know whether the host would send
// X-Aiworkgate-Account at all. v0.4 derives the value from the manifest.
const (
	envPluginID      = "AIWORKGATE_PLUGIN_ID"
	envPluginPort    = "AIWORKGATE_PLUGIN_PORT"
	envPluginToken   = "AIWORKGATE_PLUGIN_TOKEN"
	envPluginDataDir = "AIWORKGATE_PLUGIN_DATA_DIR"
	envRulesFile     = "AIWORKGATE_RULES_FILE"
	envEgressFile    = "AIWORKGATE_EGRESS_FILE"
	envAPILevel      = "AIWORKGATE_API_LEVEL"
)

// DefaultAPILevel is injected when a manifest does not declare a usable
// api_level. It must stay at the oldest level the host still accepts (1):
// a manifest written before the field existed means "contract v0.2", so
// raising the default would silently upgrade legacy plugins to the
// forced-account behaviour they never implemented.
const DefaultAPILevel = model.MinAPILevel

// APILevelFor returns the contract level to inject for mf. A declared level is
// used as-is when the host supports it; an absent (zero), unknown or
// out-of-range value falls back to DefaultAPILevel so an invalid manifest can
// never make the host advertise a level it does not speak.
func APILevelFor(mf model.Manifest) int { return normalizeAPILevel(mf.APILevel) }

// normalizeAPILevel clamps a manifest level to the range this host speaks.
func normalizeAPILevel(level int) int {
	if level >= model.MinAPILevel && level <= model.MaxAPILevel {
		return level
	}
	return DefaultAPILevel
}

// buildPluginEnv returns base with the AIWorkGate variables (and, when the
// host proxy is enabled, HTTP_PROXY/HTTPS_PROXY/NO_PROXY) replaced. Existing
// values for those keys are dropped so a child can never see a stale token or
// port. The host proxy is a single endpoint pair in config; credentials are
// not part of it. apiLevel is the plugin's declared contract level; pass
// APILevelFor(mf) so the child is told what the host will actually speak to it.
func buildPluginEnv(base []string, id string, port int, token, dataDir, rulesFile, egressFile string, apiLevel int, proxy config.ProxyConfig) []string {
	overrides := map[string]string{
		envPluginID:      id,
		envPluginPort:    strconv.Itoa(port),
		envPluginToken:   token,
		envPluginDataDir: dataDir,
		envRulesFile:     rulesFile,
		envEgressFile:    egressFile,
		envAPILevel:      strconv.Itoa(normalizeAPILevel(apiLevel)),
	}
	if proxy.Enabled {
		if v := strings.TrimSpace(proxy.HTTP); v != "" {
			overrides["HTTP_PROXY"] = v
			overrides["http_proxy"] = v
		}
		if v := strings.TrimSpace(proxy.HTTPS); v != "" {
			overrides["HTTPS_PROXY"] = v
			overrides["https_proxy"] = v
		}
		noProxy := make([]string, 0, len(proxy.NoProxy))
		for _, v := range proxy.NoProxy {
			if v = strings.TrimSpace(v); v != "" {
				noProxy = append(noProxy, v)
			}
		}
		if len(noProxy) == 0 {
			noProxy = append(noProxy, config.DefaultNoProxy...)
		}
		joined := strings.Join(noProxy, ",")
		overrides["NO_PROXY"] = joined
		overrides["no_proxy"] = joined
	}

	// Key names are compared case-insensitively: Windows treats the
	// environment as case-insensitive and plugins may be launched from a
	// shell that exported lowercase variants.
	skip := make(map[string]struct{}, len(overrides))
	for k := range overrides {
		skip[strings.ToUpper(k)] = struct{}{}
	}

	out := make([]string, 0, len(base)+len(overrides))
	for _, kv := range base {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		upper := strings.ToUpper(key)
		if _, drop := skip[upper]; drop {
			continue
		}
		if strings.HasPrefix(upper, "AIWORKGATE_") {
			continue
		}
		out = append(out, kv)
	}

	keys := make([]string, 0, len(overrides))
	for k := range overrides {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+overrides[k])
	}
	return out
}

// directEnvKeys are the variables a process uses to find an HTTP proxy. Both
// cases are set because Windows keeps the environment case-insensitive while
// Go's proxy_from_env reads lowercase names first, and curl/Node/Python children
// disagree about which one they honour.
var directEnvKeys = []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy"}

// applyDirectEgress points a plugin child's proxy environment at the host's
// direct-egress listener, so outbound traffic that a plugin (or a CLI it spawns)
// sends through the environment proxy is recorded in proxy_logs instead of
// vanishing into the plugin's line log.
//
// It is a no-op when listenerURL is empty: no listener, no injection, and the
// child keeps exactly the environment it always had. That is the fail-open rule
// of direct mode - the host never asks a plugin to route through a port that is
// not there.
//
// NO_PROXY is rebuilt from keep (the host's own bypass list) with every entry
// that would match the listener's own host, because the default bypass list is
// {"127.0.0.1","localhost"}: leaving "127.0.0.1" in it would make an
// environment-aware client skip the listener we just advertised. "localhost" is
// kept, so only literal loopback-IP calls are captured.
func applyDirectEgress(env []string, listenerURL string, keep []string) []string {
	listenerURL = strings.TrimSpace(listenerURL)
	if listenerURL == "" {
		return env
	}

	host := directListenerHost(listenerURL)
	joined := make([]string, 0, len(keep))
	for _, v := range keep {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if v == "*" || strings.EqualFold(v, host) || v == "0.0.0.0/0" {
			continue
		}
		joined = append(joined, v)
	}
	if len(joined) == 0 {
		joined = append(joined, "localhost")
	}

	out := make([]string, 0, len(env)+len(directEnvKeys))
	skip := make(map[string]struct{}, len(directEnvKeys))
	for _, k := range directEnvKeys {
		skip[strings.ToUpper(k)] = struct{}{}
	}
	for _, kv := range env {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		if _, drop := skip[strings.ToUpper(key)]; drop {
			continue
		}
		out = append(out, kv)
	}

	// Sorted so a caller (and a test) can rely on one stable spelling.
	names := []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy"}
	values := map[string]string{
		"HTTP_PROXY": listenerURL, "http_proxy": listenerURL,
		"HTTPS_PROXY": listenerURL, "https_proxy": listenerURL,
		"NO_PROXY": strings.Join(joined, ","), "no_proxy": strings.Join(joined, ","),
	}
	for _, k := range names {
		out = append(out, k+"="+values[k])
	}
	return out
}

// directListenerHost is the host part of a listener URL ("http://127.0.0.1:7913"
// → "127.0.0.1"). An unparsable URL yields "" so nothing is bypassed by accident.
func directListenerHost(listenerURL string) string {
	u, err := url.Parse(listenerURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// DirectEgressDrift lists the running children whose environment names a host
// direct listener that the host does not advertise for that plugin any more,
// keyed by plugin id with the URL they were handed.
//
// Only a child that *was* injected can drift. That asymmetry is deliberate and is
// the fail-open rule of direct mode seen from the other side: a child with no
// injected listener dials by itself, which costs a log row but never breaks
// anything, so turning direct logging on does not disturb a running plugin. A
// child left pointing at a listener that has been closed is the opposite - every
// environment-aware egress call it makes, including its callbacks to the host,
// gets connection refused until it restarts.
func (m *Manager) DirectEgressDrift() map[string]string {
	m.mu.Lock()
	type entry struct {
		id  string
		url string
	}
	live := make([]entry, 0, len(m.plugins))
	for id, p := range m.plugins {
		if !p.running || p.directEgress == "" {
			continue
		}
		live = append(live, entry{id: id, url: p.directEgress})
	}
	m.mu.Unlock()
	if len(live) == 0 {
		return nil
	}

	cfg := m.currentConfig()
	out := make(map[string]string, len(live))
	for _, e := range live {
		if got := m.directEgressURL(e.id, cfg.Proxy); got != e.url {
			out[e.id] = e.url
		}
	}
	return out
}

// ReconcileDirectEgress closes the window DirectEgressDrift describes by
// restarting every drifted child: a process environment cannot be rewritten from
// outside, so a relaunch is the only clean way to take a closed listener out of
// it. It returns the plugin ids it restarted (ordered, so a caller can report
// them) and the errors it hit; a child it could not restart is left out of the
// list and still named in the error, because that one keeps the broken exit until
// something else restarts it.
//
// Stopping a child interrupts whatever it is doing. That is accepted here on
// purpose: the alternative is a plugin whose every outbound call is refused, and
// the idle reaper's in-flight protection does not help - the traffic is already
// failing.
func (m *Manager) ReconcileDirectEgress() ([]string, error) {
	drift := m.DirectEgressDrift()
	if len(drift) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(drift))
	for id := range drift {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	restarted := make([]string, 0, len(ids))
	var errs []error
	for _, id := range ids {
		if err := m.Stop(id); err != nil {
			errs = append(errs, fmt.Errorf("plugins: stop %q to drop its direct listener %q: %w", id, drift[id], err))
			continue
		}
		// A disabled plugin stays stopped: no child, so no stale environment.
		if err := m.Start(id); err != nil && !errors.Is(err, ErrDisabled) {
			errs = append(errs, fmt.Errorf("plugins: restart %q after its direct listener closed: %w", id, err))
			continue
		}
		restarted = append(restarted, id)
		m.logInfo("plugin restarted: its direct egress listener is gone", map[string]any{
			"plugin": id, "listener": drift[id],
		})
	}
	return restarted, errors.Join(errs...)
}
