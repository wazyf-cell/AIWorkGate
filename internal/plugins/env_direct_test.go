package plugins

import (
	"strings"
	"sync"
	"testing"
	"time"

	"aiworkgate/internal/config"
	"aiworkgate/internal/model"
)

// The direct listener URL used throughout: one loopback CONNECT listener that
// dials the target itself (see internal/egress direct.go).
const testDirectListener = "http://127.0.0.1:49321"

// TestPluginEnvInDirectModePointsAtTheDirectListener is the injection guard for
// direct egress accounting: with a listener available, the child environment the
// supervisor hands the plugin names that listener as the proxy, so any client
// that honours the environment (including a CLI the plugin spawns) gets recorded
// in proxy_logs without the plugin changing a line of code.
//
// NO_PROXY is the sharp edge: the host's own bypass list is {"127.0.0.1",
// "localhost"}, and keeping "127.0.0.1" would make an environment-aware client
// skip the very listener we just advertised, silently.
func TestPluginEnvInDirectModePointsAtTheDirectListener(t *testing.T) {
	cfg := config.Default()
	cfg.Proxy.Enabled = false

	base := []string{"PATH=C:\\Windows", "HTTP_PROXY=http://stale:1", "no_proxy=127.0.0.1"}
	env := buildPluginEnv(base, "demo", 4567, "tok", `C:\data\plugin-data\demo`,
		`C:\data\runtime\demo\rules.json`, `C:\data\runtime\demo\egress.json`, 1, cfg.Proxy)
	env = applyDirectEgress(env, testDirectListener, cfg.Proxy.NoProxy)

	got := parseEnvList(env)
	for _, k := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy"} {
		if got[k] != testDirectListener {
			t.Errorf("env %s = %q, want the direct listener %q", k, got[k], testDirectListener)
		}
	}
	for _, k := range []string{"NO_PROXY", "no_proxy"} {
		v := got[k]
		if v == "" {
			t.Fatalf("env %s is empty: the listener host must not be bypassed", k)
		}
		for _, item := range strings.Split(v, ",") {
			if strings.TrimSpace(item) == "127.0.0.1" {
				t.Fatalf("env %s = %q still bypasses loopback, so the listener is never used", k, v)
			}
		}
		if !strings.Contains(v, "localhost") {
			t.Errorf("env %s = %q, want localhost kept out of the listener", k, v)
		}
	}
	// The stale value inherited from the host process must be replaced, not
	// appended to: two HTTP_PROXY entries would make the outcome depend on
	// which one the child's HTTP stack reads last.
	if n := strings.Count(strings.Join(env, "\n"), "HTTP_PROXY="); n != 1 {
		t.Errorf("HTTP_PROXY appears %d times in the child env, want 1", n)
	}
	if strings.Contains(strings.Join(env, "\n"), "http://stale:1") {
		t.Errorf("stale inherited HTTP_PROXY survived: %v", env)
	}
	// Everything the plugin already relies on is still there.
	if got["AIWORKGATE_EGRESS_FILE"] != `C:\data\runtime\demo\egress.json` {
		t.Errorf("AIWORKGATE_EGRESS_FILE = %q, want the egress.json path", got["AIWORKGATE_EGRESS_FILE"])
	}
	if got["AIWORKGATE_PLUGIN_TOKEN"] != "tok" {
		t.Errorf("AIWORKGATE_PLUGIN_TOKEN = %q, want tok", got["AIWORKGATE_PLUGIN_TOKEN"])
	}
}

// TestApplyDirectEgressWithoutListenerIsANoOp is the fail-open half: no listener
// means the child gets exactly the environment it always got. The host must never
// ask a plugin to route through a port that is not there.
func TestApplyDirectEgressWithoutListenerIsANoOp(t *testing.T) {
	in := []string{"PATH=C:\\Windows", "HTTP_PROXY=http://keep:1", "NO_PROXY=127.0.0.1,localhost"}
	out := applyDirectEgress(in, "  ", config.Default().Proxy.NoProxy)
	if strings.Join(out, "\n") != strings.Join(in, "\n") {
		t.Fatalf("empty listener changed the child env:\n in %v\nout %v", in, out)
	}
}

// TestDirectEgressURLHonoursHostProxy guards the one case where pointing at a
// direct listener would move traffic rather than watch it: a host-wide proxy the
// user switched on deliberately.
func TestDirectEgressURLHonoursHostProxy(t *testing.T) {
	hooked := New(Options{DirectEgress: func(string) string { return testDirectListener }})
	if got := hooked.directEgressURL("demo", config.ProxyConfig{}); got != testDirectListener {
		t.Fatalf("directEgressURL = %q, want the listener", got)
	}

	enabled := config.ProxyConfig{
		Enabled: true,
		HTTP:    "http://127.0.0.1:7890",
		HTTPS:   "http://127.0.0.1:7891",
		NoProxy: []string{"127.0.0.1", "localhost"},
	}
	if got := hooked.directEgressURL("demo", enabled); got != "" {
		t.Fatalf("directEgressURL = %q with the host proxy enabled, want \"\" (never replace a deliberate proxy)", got)
	}

	// Composed the way launch() does it, the host proxy pair must survive intact.
	cfg := config.Default()
	cfg.Proxy = enabled
	env := buildPluginEnv(nil, "demo", 1, "t", "d", "r", "e", 1, cfg.Proxy)
	if listener := hooked.directEgressURL("demo", cfg.Proxy); listener != "" {
		env = applyDirectEgress(env, listener, cfg.Proxy.NoProxy)
	}
	got := parseEnvList(env)
	if got["HTTP_PROXY"] != "http://127.0.0.1:7890" || got["HTTPS_PROXY"] != "http://127.0.0.1:7891" {
		t.Fatalf("host proxy overwritten by the direct listener: %v", got)
	}

	plain := New(Options{})
	if got := plain.directEgressURL("demo", config.ProxyConfig{}); got != "" {
		t.Fatalf("directEgressURL = %q without a hook, want \"\"", got)
	}
}

// TestDirectEgressSurvivesHookFailure states the contract the accounting exists
// to serve: a failure on the recording side may cost the row, never the plugin.
func TestDirectEgressSurvivesHookFailure(t *testing.T) {
	panicky := New(Options{DirectEgress: func(string) string { panic("egress manager is gone") }})
	if got := panicky.directEgressURL("demo", config.ProxyConfig{}); got != "" {
		t.Fatalf("directEgressURL = %q after a hook panic, want \"\"", got)
	}
	env := buildPluginEnv(nil, "demo", 1, "t", "d", "r", "e", 1, config.ProxyConfig{})
	before := strings.Join(env, "\n")
	after := strings.Join(applyDirectEgress(env, panicky.directEgressURL("demo", config.ProxyConfig{}),
		config.Default().Proxy.NoProxy), "\n")
	if before != after {
		t.Fatalf("a failed hook changed the child env:\nbefore %v\nafter  %v", env, strings.Split(after, "\n"))
	}
}

// TestLaunchEnvPointsAtDirectListener is the end-to-end half of the injection
// guard: it starts the real fake plugin and reads what the child process itself
// saw, so the assertion covers supervisor.launch's composition of the child
// environment, not just the helper.
func TestLaunchEnvPointsAtDirectListener(t *testing.T) {
	listener := "http://127.0.0.1:49322"

	start := func(hook func(pluginID string) string) string {
		t.Helper()
		mgr := newTestManager(t)
		mgr.opts.DirectEgress = hook
		t.Cleanup(func() { _ = mgr.Close() })
		installFakePlugin(t, mgr, "fake", model.StartupAlways)
		waitStatus(t, mgr, "fake", 30*time.Second, "healthy start", func(s model.PluginStatus) bool {
			return s.Running && s.Healthy
		})
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			var joined string
			for _, line := range mgr.RecentOutput("fake", 200) {
				joined += line + "\n"
			}
			if strings.Contains(joined, "egress=") {
				return joined
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("the fake plugin never reported its startup environment")
		return ""
	}

	withListener := start(func(string) string { return listener })
	if !strings.Contains(withListener, "http_proxy="+listener) {
		t.Fatalf("child env does not name the direct listener:\n%s", withListener)
	}
	if strings.Contains(withListener, "http_proxy=http://127.0.0.1,") {
		t.Fatalf("child NO_PROXY still bypasses loopback:\n%s", withListener)
	}

	withoutHook := start(nil)
	if strings.Contains(withoutHook, "http_proxy="+listener) {
		t.Fatalf("a plugin without a direct listener was told to use one:\n%s", withoutHook)
	}
	if !strings.Contains(withoutHook, "http_proxy=\n") {
		t.Fatalf("expected an empty http_proxy without the hook, got:\n%s", withoutHook)
	}
}

// settableHook is the app's direct-egress policy answer as a test can move it:
// one URL per plugin, changed while a child is already running, which is exactly
// the situation the closed switch creates.
type settableHook struct {
	mu     sync.Mutex
	byName map[string]string
}

func (h *settableHook) set(id, url string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.byName[id] = url
}

func (h *settableHook) get(id string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.byName[id]
}

// childEnvLines returns the lines the fake plugin prints about the environment it
// was launched with, oldest first. One line per child process, so its length also
// tells a test whether the supervisor replaced the child.
func childEnvLines(mgr *Manager, id string) []string {
	var out []string
	for _, line := range mgr.RecentOutput(id, maxPluginLogLines) {
		if strings.Contains(line, "egress=") && strings.Contains(line, "http_proxy=") {
			out = append(out, line)
		}
	}
	return out
}

func proxyOfEnvLine(line string) string {
	i := strings.Index(line, "http_proxy=")
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(line[i+len("http_proxy="):])
}

func waitForEnvLines(t *testing.T, mgr *Manager, id string, want int) []string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		lines := childEnvLines(mgr, id)
		if len(lines) >= want {
			return lines
		}
		if time.Now().After(deadline) {
			t.Fatalf("plugin %q reported %d startup environment lines (%v), want %d", id, len(lines), lines, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestReconcileDirectEgressRestoresClosedListeners is the outage guard for the
// direct-logging switch: the listener URL lives in the child's own environment,
// which the host cannot rewrite, so a child that was handed a listener the host
// has since closed (or moved) must be relaunched before it can egress again.
//
// It asserts the two halves of that promise: the host can name the stale children
// while they are still running, and reconciling replaces exactly those children
// and nothing else - a child that never got a listener keeps running, because
// "not recorded" is direct mode's fail-open and costs it nothing.
func TestReconcileDirectEgressRestoresClosedListeners(t *testing.T) {
	mgr := newTestManager(t)
	hook := &settableHook{byName: map[string]string{"stale": testDirectListener, "plain": ""}}
	mgr.opts.DirectEgress = hook.get
	t.Cleanup(func() { _ = mgr.Close() })

	installFakePlugin(t, mgr, "stale", model.StartupAlways)
	installFakePlugin(t, mgr, "plain", model.StartupAlways)
	for _, id := range []string{"stale", "plain"} {
		waitStatus(t, mgr, id, 30*time.Second, "healthy start", func(s model.PluginStatus) bool {
			return s.Running && s.Healthy && s.PID != 0
		})
	}
	staleLines := waitForEnvLines(t, mgr, "stale", 1)
	if got := proxyOfEnvLine(staleLines[0]); got != testDirectListener {
		t.Fatalf("stale child was launched with http_proxy = %q, want the listener %q", got, testDirectListener)
	}
	plainLines := waitForEnvLines(t, mgr, "plain", 1)
	if got := proxyOfEnvLine(plainLines[0]); got != "" {
		t.Fatalf("plain child was launched with http_proxy = %q, want no listener", got)
	}

	// 1. Nothing has changed, so nothing is stale: saving an unrelated setting
	//    must not restart a plugin that is happily recording.
	if drift := mgr.DirectEgressDrift(); len(drift) != 0 {
		t.Fatalf("DirectEgressDrift with the switch untouched = %v, want no drift", drift)
	}
	if restarted, err := mgr.ReconcileDirectEgress(); len(restarted) != 0 || err != nil {
		t.Fatalf("ReconcileDirectEgress with no drift = %v, %v; want nothing restarted", restarted, err)
	}

	// 2. The switch goes off under the running child: its listener is gone, and
	//    the host must be able to name it - that record is the whole point.
	hook.set("stale", "")
	drift := mgr.DirectEgressDrift()
	if len(drift) != 1 || drift["stale"] != testDirectListener {
		t.Fatalf("DirectEgressDrift after the listener closed = %v, want {stale: %s}", drift, testDirectListener)
	}

	before := map[string]int{}
	for _, id := range []string{"stale", "plain"} {
		st, _ := mgr.Get(id)
		before[id] = st.PID
	}

	restarted, err := mgr.ReconcileDirectEgress()
	if err != nil {
		t.Fatalf("ReconcileDirectEgress: %v", err)
	}
	if strings.Join(restarted, ",") != "stale" {
		t.Fatalf("restarted = %v, want exactly [stale]", restarted)
	}
	if after := mgr.DirectEgressDrift(); len(after) != 0 {
		t.Fatalf("DirectEgressDrift after reconciling = %v, want no running child naming a closed listener", after)
	}

	waitStatus(t, mgr, "stale", 30*time.Second, "restarted child", func(s model.PluginStatus) bool {
		return s.Running && s.Healthy && s.PID != 0 && s.PID != before["stale"]
	})
	lines := waitForEnvLines(t, mgr, "stale", 2)
	if got := proxyOfEnvLine(lines[len(lines)-1]); got != "" {
		t.Fatalf("the relaunched child still names a listener: http_proxy = %q", got)
	}
	if st, _ := mgr.Get("plain"); st.PID != before["plain"] {
		t.Fatalf("the plugin without a listener was restarted anyway: pid %d -> %d", before["plain"], st.PID)
	}
	if len(childEnvLines(mgr, "plain")) != 1 {
		t.Fatalf("the untouched plugin printed %d startup environments, want 1", len(childEnvLines(mgr, "plain")))
	}

	// 3. The child that came back has no listener, so re-opening the switch is not
	//    drift for it: drift is what a running process actually names, and a child
	//    that dials by itself is direct mode's fail-open, not an outage.
	hook.set("stale", testDirectListener)
	if drift := mgr.DirectEgressDrift(); len(drift) != 0 {
		t.Fatalf("DirectEgressDrift for the relaunched child = %v, want empty", drift)
	}
	if restarted, err := mgr.ReconcileDirectEgress(); len(restarted) != 0 || err != nil {
		t.Fatalf("ReconcileDirectEgress for a child without a listener = %v, %v; want nothing restarted", restarted, err)
	}
}

// TestReconcileDirectEgressNamesAListenerThatMoved covers the other way a child
// can be left on a dead port: the host re-bound its direct listener, so the URL
// the child was handed no longer exists even though some listener does.
func TestReconcileDirectEgressNamesAListenerThatMoved(t *testing.T) {
	const moved = "http://127.0.0.1:49323"
	mgr := newTestManager(t)
	hook := &settableHook{byName: map[string]string{"moved": testDirectListener}}
	mgr.opts.DirectEgress = hook.get
	t.Cleanup(func() { _ = mgr.Close() })

	installFakePlugin(t, mgr, "moved", model.StartupAlways)
	waitStatus(t, mgr, "moved", 30*time.Second, "healthy start", func(s model.PluginStatus) bool {
		return s.Running && s.Healthy && s.PID != 0
	})
	if got := waitForEnvLines(t, mgr, "moved", 1)[0]; proxyOfEnvLine(got) != testDirectListener {
		t.Fatalf("child was launched with %q, want the listener", proxyOfEnvLine(got))
	}

	hook.set("moved", moved)
	if drift := mgr.DirectEgressDrift(); drift["moved"] != testDirectListener {
		t.Fatalf("a moved listener is not drift for the child on the old one: %v", drift)
	}
	restarted, err := mgr.ReconcileDirectEgress()
	if err != nil || strings.Join(restarted, ",") != "moved" {
		t.Fatalf("ReconcileDirectEgress after the listener moved = %v, %v; want [moved]", restarted, err)
	}
	lines := waitForEnvLines(t, mgr, "moved", 2)
	if got := proxyOfEnvLine(lines[len(lines)-1]); got != moved {
		t.Fatalf("the relaunched child was handed http_proxy = %q, want the new listener %q", got, moved)
	}
	if after := mgr.DirectEgressDrift(); len(after) != 0 {
		t.Fatalf("DirectEgressDrift after the move = %v, want empty", after)
	}
}

// TestReconcileDirectEgressWithoutChildrenIsANoOp keeps the settings save cheap
// and honest: with no injected child running, reconciling must not touch a
// plugin, must not start one, and must not report anything.
func TestReconcileDirectEgressWithoutChildrenIsANoOp(t *testing.T) {
	mgr := newTestManager(t)
	mgr.opts.DirectEgress = func(string) string { return testDirectListener }
	t.Cleanup(func() { _ = mgr.Close() })
	installFakePlugin(t, mgr, "asleep", model.StartupOnDemand)

	if drift := mgr.DirectEgressDrift(); len(drift) != 0 {
		t.Fatalf("DirectEgressDrift with no running child = %v, want empty", drift)
	}
	restarted, err := mgr.ReconcileDirectEgress()
	if err != nil {
		t.Fatalf("ReconcileDirectEgress: %v", err)
	}
	if len(restarted) != 0 {
		t.Fatalf("restarted = %v, want nothing", restarted)
	}
	if st, _ := mgr.Get("asleep"); st.Running {
		t.Fatal("reconciling started a stopped plugin on its own")
	}
}
