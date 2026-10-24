package app

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aiworkgate/internal/config"
	"aiworkgate/internal/egress"
	"aiworkgate/internal/model"
	"aiworkgate/internal/plugins"
	"aiworkgate/internal/store"
)

// newDirectApp builds the smallest app-shaped object that owns a plugin, a
// store and an egress manager wired the way app.Run wires them: the manager's
// sink is the proxy traffic table, the supervisor reads the live configuration
// and asks the app where a plugin's direct egress should be recorded.
func newDirectApp(t *testing.T, cfg config.Config) (*App, *store.Store, *egress.Manager) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	pluginDir := filepath.Join(dir, "plugins", "mock")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatalf("mkdir plugin: %v", err)
	}
	mf := model.Manifest{
		ID:           "mock",
		Name:         "Mock",
		Version:      "0.0.1",
		APILevel:     1,
		Capabilities: []string{model.CapEgress},
		Startup:      model.StartupConfig{Mode: model.StartupOnDemand},
		Entry:        model.Entry{Exe: "mock.exe"},
	}
	raw, err := json.Marshal(mf)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, plugins.ManifestFileName), raw, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "mock.exe"), []byte("MZ"), 0o644); err != nil {
		t.Fatalf("write entry: %v", err)
	}

	egressMgr := egress.NewManager(st)
	// The same sink app.Run installs: one row per CONNECT into proxy_logs.
	egressMgr.SetLogSink(func(row model.ProxyLog) { _, _ = st.InsertProxyLog(row) })
	t.Cleanup(func() { _ = egressMgr.CloseAll() })

	// The App has to exist before the plugin manager is built: the supervisor
	// reads the configuration and the direct-egress decision from it, exactly as
	// app.Run wires them, and Discover already asks for the config.
	a := &App{dataDir: dir, cfg: cfg, store: st, egressMgr: egressMgr}
	pluginMgr := plugins.New(plugins.Options{
		DataDir:      dir,
		Config:       a.configSnapshot,
		DirectEgress: a.directEgressURL,
	})
	a.pluginMgr = pluginMgr
	t.Cleanup(func() { _ = pluginMgr.Close() })
	if err := pluginMgr.Discover(); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	return a, st, egressMgr
}

// TestDirectEgressURLIsGatedAndRecorded is the injection guard at the level the
// policy lives: the app decides whether a plugin may be pointed at a direct
// listener, and a listener it points at really writes a proxy_logs row.
func TestDirectEgressURLIsGatedAndRecorded(t *testing.T) {
	cfg := config.Default()
	a, st, egressMgr := newDirectApp(t, cfg)

	// 1. Off by default: no listener, so a plugin gets exactly what it got
	//    before this feature existed.
	if got := a.directEgressURL("mock"); got != "" {
		t.Fatalf("directEgressURL with direct logging off = %q, want \"\"", got)
	}

	if err := egressMgr.SetDirectLogging(true); err != nil {
		t.Fatalf("SetDirectLogging: %v", err)
	}
	listener := a.directEgressURL("mock")
	if listener == "" {
		t.Fatal("directEgressURL = \"\" with direct logging on, want a listener URL")
	}
	u, err := url.Parse(listener)
	if err != nil || u.Scheme != "http" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") {
		t.Fatalf("listener = %q, want a loopback http URL", listener)
	}

	// 2. A client that honours the URL (this is what a plugin transport does)
	//    reaches the origin and leaves one attributable row behind. The origin
	//    is HTTPS on purpose: the listener implements CONNECT only, and a
	//    cleartext origin would be sent as an absolute-form GET instead.
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "direct-ok")
	}))
	t.Cleanup(origin.Close)

	tr := &http.Transport{
		Proxy:             http.ProxyURL(u),
		DisableKeepAlives: true,
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
	}
	t.Cleanup(tr.CloseIdleConnections)
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	resp, err := client.Get(origin.URL + "/via-direct-listener")
	if err != nil {
		t.Fatalf("GET through the direct listener: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "direct-ok" {
		t.Fatalf("body = %q, want direct-ok", body)
	}

	rows := waitForProxyRows(t, st, "mock", 1)
	row := rows[0]
	if row.ProxyID != "direct" || row.ProxyLabel != "直连" {
		t.Fatalf("row proxy = %q/%q, want direct/直连", row.ProxyID, row.ProxyLabel)
	}
	if row.PluginID != "mock" || row.Result != model.ProxyResultOK {
		t.Fatalf("row = %+v, want plugin mock and result ok", row)
	}
	if row.BytesUp <= 0 || row.BytesDown <= 0 {
		t.Fatalf("row counted %d up / %d down, want both", row.BytesUp, row.BytesDown)
	}

	// 3. A host-wide proxy is never replaced by a direct listener: that would
	//    move traffic that used to leave through the configured proxy.
	withProxy := cfg
	withProxy.Proxy = config.ProxyConfig{
		Enabled: true,
		HTTP:    "http://127.0.0.1:7890",
		HTTPS:   "http://127.0.0.1:7891",
		NoProxy: append([]string(nil), config.DefaultNoProxy...),
	}
	a.cfg = withProxy
	if got := a.directEgressURL("mock"); got != "" {
		t.Fatalf("directEgressURL with the host proxy enabled = %q, want \"\"", got)
	}
	a.cfg = cfg

	// 4. Strict mode must not be handed a working exit through the back door.
	strictCfg := cfg
	strictCfg.Plugins.StrictEgress = true
	a.cfg = strictCfg
	if got := a.directEgressURL("mock"); got != "" {
		t.Fatalf("directEgressURL in strict mode = %q, want \"\" (strict has no direct exit)", got)
	}
	if err := a.writeEgressForPlugin("mock", []string{"acc_1"}); err != nil {
		t.Fatalf("writeEgressForPlugin: %v", err)
	}
	strictFile := readEgressFile(t, a.pluginMgr.EgressFile("mock"))
	if strictFile.Default.Mode != model.EgressBlocked || strictFile.Default.URL != "" {
		t.Fatalf("strict default entry = %+v, want blocked without a listener", strictFile.Default)
	}
	if entry := strictFile.Accounts["acc_1"]; entry.URL != "" {
		t.Fatalf("strict account entry = %+v, want no listener attached", entry)
	}
	a.cfg = cfg

	// 5. Non-strict egress.json names the listener, but keeps the mode direct:
	//    a plugin that ignores the URL still dials by itself (fail-open).
	if err := a.writeEgressForPlugin("mock", []string{"acc_1"}); err != nil {
		t.Fatalf("writeEgressForPlugin: %v", err)
	}
	file := readEgressFile(t, a.pluginMgr.EgressFile("mock"))
	def := file.Default
	if def.Mode != model.EgressDirect || def.URL == "" || def.ProxyID != "direct" {
		t.Fatalf("default entry = %+v, want direct carrying the listener", def)
	}
	account := file.Accounts["acc_1"]
	if account.Mode != model.EgressDirect || account.URL == "" || account.ProxyID != "direct" {
		t.Fatalf("acc_1 entry = %+v, want direct carrying its own listener", account)
	}
	if account.URL == def.URL {
		t.Fatalf("acc_1 shares the plugin-wide listener %q; rows would be unattributable", account.URL)
	}
	if _, err := url.Parse(account.URL); err != nil {
		t.Fatalf("acc_1 listener url unparseable: %q", account.URL)
	}

	// 6. Switching the feature off takes the URL back out of the file.
	if err := egressMgr.SetDirectLogging(false); err != nil {
		t.Fatalf("SetDirectLogging(off): %v", err)
	}
	if err := a.writeEgressForPlugin("mock", []string{"acc_1"}); err != nil {
		t.Fatalf("writeEgressForPlugin: %v", err)
	}
	off := readEgressFile(t, a.pluginMgr.EgressFile("mock"))
	if off.Default.Mode != model.EgressDirect || off.Default.URL != "" {
		t.Fatalf("default entry with logging off = %+v, want the bare direct entry", off.Default)
	}
	if off.Accounts["acc_1"].URL != "" {
		t.Fatalf("acc_1 kept a listener after switching off: %+v", off.Accounts["acc_1"])
	}
}

// waitForProxyRows gives the tunnel goroutine its turn: an "ok" row is written
// when the tunnel closes, not when the response is delivered.
func waitForProxyRows(t *testing.T, st *store.Store, pluginID string, want int) []model.ProxyLog {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows, _, err := st.QueryProxyLogs(store.ProxyLogFilter{PluginID: pluginID})
		if err != nil {
			t.Fatalf("QueryProxyLogs: %v", err)
		}
		if len(rows) >= want {
			return rows
		}
		if time.Now().After(deadline) {
			t.Fatalf("proxy rows = %d (%+v), want %d", len(rows), rows, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func readEgressFile(t *testing.T, path string) model.EgressFile {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read egress.json: %v", err)
	}
	var f model.EgressFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("decode egress.json: %v", err)
	}
	return f
}

// ---------------------------------------------------------------- real children

// The direct-logging switch is only observable on a live plugin process: what it
// changes is that process's environment, and an environment cannot be rewritten
// from outside. These tests therefore install the fake plugin (the same entry the
// plugins package supervises) and read what the child itself reports about the
// environment it was launched with.

var (
	directFakeOnce sync.Once
	directFakeData []byte
	directFakeErr  error
)

// directFakePlugin builds internal/plugins/testdata/fakeplugin once and returns
// its bytes. A build failure fails the test instead of skipping it: these tests
// guard an outage, and a skipped guard reads as a pass.
func directFakePlugin(t *testing.T) []byte {
	t.Helper()
	directFakeOnce.Do(func() {
		dir, err := os.MkdirTemp("", "aiworkgate-app-fakeplugin-*")
		if err != nil {
			directFakeErr = err
			return
		}
		defer func() { _ = os.RemoveAll(dir) }()
		exe := filepath.Join(dir, "fake-plugin.exe")
		cmd := exec.Command("go", "build", "-o", exe, ".")
		cmd.Dir = filepath.Join("..", "plugins", "testdata", "fakeplugin")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, buildErr := cmd.CombinedOutput(); buildErr != nil {
			directFakeErr = fmt.Errorf("go build fakeplugin: %v\n%s", buildErr, out)
			return
		}
		data, readErr := os.ReadFile(exe)
		if readErr != nil {
			directFakeErr = readErr
			return
		}
		directFakeData = data
	})
	if directFakeErr != nil {
		t.Fatalf("fake plugin unavailable for %s: %v", t.Name(), directFakeErr)
	}
	return append([]byte(nil), directFakeData...)
}

// installDirectPlugin installs the fake plugin so the supervisor starts it right
// away, and waits until it is healthy.
func installDirectPlugin(t *testing.T, a *App, id string) model.PluginStatus {
	t.Helper()
	src := filepath.Join(t.TempDir(), id)
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatalf("mkdir install source: %v", err)
	}
	mf := model.Manifest{
		ID:           id,
		Name:         "Direct egress fixture",
		Version:      "0.0.1",
		APILevel:     1,
		Capabilities: []string{model.CapEgress},
		Startup:      model.StartupConfig{Mode: model.StartupAlways},
		Entry:        model.Entry{Exe: "fake-plugin.exe"},
	}
	raw, err := json.Marshal(mf)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, plugins.ManifestFileName), raw, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "fake-plugin.exe"), directFakePlugin(t), 0o755); err != nil {
		t.Fatalf("write entry: %v", err)
	}
	if err := a.pluginMgr.InstallDir(src); err != nil {
		t.Fatalf("InstallDir: %v", err)
	}
	st := waitDirectStatus(t, a, id, func(s model.PluginStatus) bool {
		return s.Running && s.Healthy && s.PID != 0
	}, "healthy start")
	if st.Port == 0 {
		t.Fatalf("plugin %q is healthy without a port", id)
	}
	return st
}

func waitDirectStatus(t *testing.T, a *App, id string, pred func(model.PluginStatus) bool, why string) model.PluginStatus {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		st, ok := a.pluginMgr.Get(id)
		if !ok {
			t.Fatalf("plugin %q disappeared while waiting for %s", id, why)
		}
		if pred(st) {
			return st
		}
		if st.Failed {
			t.Fatalf("plugin %q failed while waiting for %s: %s", id, why, st.LastError)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s; running:%v healthy:%v pid:%d error:%q",
				why, st.Running, st.Healthy, st.PID, st.LastError)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// directChildProxies returns the http_proxy each child of id reported at startup,
// oldest first: one entry per process the supervisor launched for it.
func directChildProxies(t *testing.T, a *App, id string) []string {
	t.Helper()
	var out []string
	for _, line := range a.pluginMgr.RecentOutput(id, 200) {
		if !strings.Contains(line, "egress=") || !strings.Contains(line, "http_proxy=") {
			continue
		}
		i := strings.Index(line, "http_proxy=")
		out = append(out, strings.TrimSpace(line[i+len("http_proxy="):]))
	}
	return out
}

func waitForDirectChildren(t *testing.T, a *App, id string, want int) []string {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		got := directChildProxies(t, a, id)
		if len(got) >= want {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("plugin %q reported %d startup environments (%v), want %d", id, len(got), got, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// directListenerOpen proves what a plugin would find when it dials the URL it was
// handed: the closed switch has to really take the port away (that is the outage),
// and the open one has to really be there.
func directListenerOpen(t *testing.T, listener string, want bool) {
	t.Helper()
	u, parseErr := url.Parse(listener)
	if parseErr != nil || u.Host == "" {
		t.Fatalf("parse listener url %q: %v", listener, parseErr)
	}
	conn, err := net.DialTimeout("tcp", u.Host, 1500*time.Millisecond)
	if err == nil {
		_ = conn.Close()
	}
	if want && err != nil {
		t.Fatalf("direct listener %s is not reachable, want it open: %v", listener, err)
	}
	if !want && err == nil {
		t.Fatalf("direct listener %s still accepts connections after the switch was closed", listener)
	}
}

func directAuditRows(t *testing.T, st *store.Store, action string) []model.AuditEntry {
	t.Helper()
	rows, err := st.ListAudit(model.AuditConfig, 200)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	var out []model.AuditEntry
	for _, row := range rows {
		if row.Action == action {
			out = append(out, row)
		}
	}
	return out
}

// TestSaveSettingsClosesTheDirectEgressWindowOnRunningChildren is the outage
// guard for the direct-logging switch. A plugin's exit path is written into its
// process environment at launch, and an environment cannot be rewritten from
// outside: closing egress.direct_logging stops the listeners, so every child that
// was launched while it was open would keep sending its egress - and its callbacks
// to the host - to a port that now refuses them until it happens to restart. The
// save that closes the switch therefore has to relaunch exactly those children.
func TestSaveSettingsClosesTheDirectEgressWindowOnRunningChildren(t *testing.T) {
	cfg := config.Default()
	cfg.Egress.DirectLogging = true
	a, st, egressMgr := newDirectApp(t, cfg)
	// What app.Run does at boot for a host that starts with the switch on.
	if err := egressMgr.SetDirectLogging(true); err != nil {
		t.Fatalf("SetDirectLogging(on): %v", err)
	}

	first := installDirectPlugin(t, a, "envopen")
	listener := a.directEgressURL("envopen")
	if listener == "" {
		t.Fatal("directEgressURL = \"\" with the switch on, want the listener the child was handed")
	}
	if got := waitForDirectChildren(t, a, "envopen", 1)[0]; got != listener {
		t.Fatalf("child was launched with http_proxy = %q, want the listener %q", got, listener)
	}
	directListenerOpen(t, listener, true)

	// The save under test: only the switch changes, sent the way the settings page
	// sends a patch.
	if err := a.saveSettings(json.RawMessage(`{"egress":{"direct_logging":false}}`)); err != nil {
		t.Fatalf("saveSettings(direct_logging off): %v", err)
	}

	// 1. The premise of the outage: that port is gone.
	directListenerOpen(t, listener, false)

	// 2. The invariant: no running child may still name a listener the host closed.
	if drift := a.pluginMgr.DirectEgressDrift(); len(drift) != 0 {
		t.Fatalf("a running child still names a closed direct listener: %v", drift)
	}

	// 3. It is fixed by relaunch, not by leaving the plugin broken: a new process,
	//    healthy again, with no injected listener in its environment.
	waitDirectStatus(t, a, "envopen", func(s model.PluginStatus) bool {
		return s.Running && s.Healthy && s.PID != 0 && s.PID != first.PID
	}, "the child restarted after its listener closed")
	if got := waitForDirectChildren(t, a, "envopen", 2)[1]; got != "" {
		t.Fatalf("the relaunched child was still handed http_proxy = %q, want none", got)
	}

	// 4. The restarted child reads the egress file the same save rewrote, so the
	//    listener URL is gone from its per-account map too (fail-open: direct mode,
	//    no exit handed to it).
	file := readEgressFile(t, a.pluginMgr.EgressFile("envopen"))
	if file.Default.Mode != model.EgressDirect || file.Default.URL != "" {
		t.Fatalf("default egress entry after switching off = %+v, want the bare direct entry", file.Default)
	}

	// 5. The user is told which plugins a settings save restarted.
	rows := directAuditRows(t, st, "direct_egress_restart")
	if len(rows) != 1 {
		t.Fatalf("direct_egress_restart audit rows = %d (%+v), want 1", len(rows), rows)
	}
	if !strings.Contains(rows[0].Detail, "envopen") {
		t.Fatalf("audit detail = %q, want it to name the restarted plugin envopen", rows[0].Detail)
	}
	if rows[0].Result != "ok" {
		t.Fatalf("audit result = %q, want ok", rows[0].Result)
	}
}

// TestDirectLoggingOnDoesNotRestartRunningChildren pins the other half of the
// asymmetry, and with it the promise that this fix costs nothing when the switch
// is not being closed. Turning direct logging on can only ever cost a missing
// row, so it must leave a running child exactly as it is: a plugin that was
// launched without a listener keeps dialling by itself (direct mode's fail-open),
// and a plugin that is already recording keeps its process across an unrelated
// save. Restarting either one here would be an outage bought with nothing.
func TestDirectLoggingOnDoesNotRestartRunningChildren(t *testing.T) {
	cfg := config.Default() // shipped default: direct logging off
	a, st, _ := newDirectApp(t, cfg)

	// A child launched while the switch is off has no listener in its environment.
	late := installDirectPlugin(t, a, "envlate")
	if got := waitForDirectChildren(t, a, "envlate", 1)[0]; got != "" {
		t.Fatalf("a plugin launched with the switch off was handed http_proxy = %q, want none", got)
	}

	if err := a.saveSettings(json.RawMessage(`{"egress":{"direct_logging":true}}`)); err != nil {
		t.Fatalf("saveSettings(direct_logging on): %v", err)
	}
	listener := a.directEgressURL("envlate")
	if listener == "" {
		t.Fatal("the switch was not applied: directEgressURL = \"\" after turning direct logging on")
	}
	directListenerOpen(t, listener, true)

	// The child that predates the switch is untouched: same process, same
	// environment, and nothing is reported as restarted.
	if now, _ := a.pluginMgr.Get("envlate"); now.PID != late.PID || !now.Running || !now.Healthy {
		t.Fatalf("envlate = %+v after turning the switch on, want the same healthy process (pid %d)", now, late.PID)
	}
	if got := directChildProxies(t, a, "envlate"); len(got) != 1 || got[0] != "" {
		t.Fatalf("envlate startup environments = %v, want exactly one without a listener", got)
	}
	if drift := a.pluginMgr.DirectEgressDrift(); len(drift) != 0 {
		t.Fatalf("DirectEgressDrift after turning the switch on = %v, want empty (a child with no listener cannot be stale)", drift)
	}
	if rows := directAuditRows(t, st, "direct_egress_restart"); len(rows) != 0 {
		t.Fatalf("turning the switch on reported restarts: %+v", rows)
	}

	// A child launched with the switch on is injected, and an unrelated save leaves
	// it running and still recording.
	on := installDirectPlugin(t, a, "envkeep")
	keep := a.directEgressURL("envkeep")
	if keep == "" {
		t.Fatal("envkeep has no direct listener while the switch is on")
	}
	if got := waitForDirectChildren(t, a, "envkeep", 1)[0]; got != keep {
		t.Fatalf("envkeep was launched with http_proxy = %q, want the listener %q", got, keep)
	}
	if err := a.saveSettings(json.RawMessage(`{"logging":{"debug_body":true}}`)); err != nil {
		t.Fatalf("saveSettings(unrelated setting): %v", err)
	}
	if !a.configSnapshot().Logging.DebugBody {
		t.Fatal("the unrelated setting in the same patch did not apply")
	}
	if now, _ := a.pluginMgr.Get("envkeep"); now.PID != on.PID || !now.Running || !now.Healthy {
		t.Fatalf("envkeep = %+v after an unrelated save, want the same healthy process (pid %d)", now, on.PID)
	}
	if got := directChildProxies(t, a, "envkeep"); len(got) != 1 || got[0] != keep {
		t.Fatalf("envkeep startup environments = %v, want exactly one naming %q", got, keep)
	}
	if drift := a.pluginMgr.DirectEgressDrift(); len(drift) != 0 {
		t.Fatalf("DirectEgressDrift while nothing closed = %v, want empty", drift)
	}
	if rows := directAuditRows(t, st, "direct_egress_restart"); len(rows) != 0 {
		t.Fatalf("an unrelated save reported restarts: %+v", rows)
	}

	// And the listener it was handed is still the one that records its traffic.
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "recorded")
	}))
	t.Cleanup(origin.Close)
	u, err := url.Parse(keep)
	if err != nil {
		t.Fatalf("parse listener: %v", err)
	}
	tr := &http.Transport{Proxy: http.ProxyURL(u), DisableKeepAlives: true,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	t.Cleanup(tr.CloseIdleConnections)
	resp, err := (&http.Client{Transport: tr, Timeout: 10 * time.Second}).Get(origin.URL + "/keep-recording")
	if err != nil {
		t.Fatalf("GET through the still-open listener: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	rows := waitForProxyRows(t, st, "envkeep", 1)
	if rows[0].Result != model.ProxyResultOK {
		t.Fatalf("envkeep row result = %q, want ok (row %+v)", rows[0].Result, rows[0])
	}
	if rows[0].ProxyID != "direct" {
		t.Fatalf("envkeep row is not a direct row: %+v", rows[0])
	}
}

// TestDirectEgressFailOpenRecordsFailuresWithoutTouchingTheTunnel keeps the
// fail-open rule of direct mode intact after the closed-switch relaunch exists: an
// internal failure (here: a target nobody answers) must still produce its row in
// proxy_logs and must not damage the listener, so the next tunnel through the same
// port is still served and still recorded as ok.
func TestDirectEgressFailOpenRecordsFailuresWithoutTouchingTheTunnel(t *testing.T) {
	cfg := config.Default()
	a, st, egressMgr := newDirectApp(t, cfg)
	if err := egressMgr.SetDirectLogging(true); err != nil {
		t.Fatalf("SetDirectLogging(on): %v", err)
	}
	listener := a.directEgressURL("mock")
	if listener == "" {
		t.Fatal("no direct listener with the switch on")
	}
	u, err := url.Parse(listener)
	if err != nil {
		t.Fatalf("parse listener: %v", err)
	}
	tr := &http.Transport{Proxy: http.ProxyURL(u), DisableKeepAlives: true,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	t.Cleanup(tr.CloseIdleConnections)
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}

	dead := freeDirectPort(t)
	if _, err := client.Get("https://127.0.0.1:" + dead + "/nobody-home"); err == nil {
		t.Fatal("the direct dial of a closed port was expected to fail")
	}
	failed := waitForProxyRows(t, st, "mock", 1)
	if failed[0].Result != model.ProxyResultFailed {
		t.Fatalf("failed-dial row result = %q, want %q", failed[0].Result, model.ProxyResultFailed)
	}
	if failed[0].ProxyID != "direct" {
		t.Fatalf("failed row is not a direct row: %+v", failed[0])
	}

	// The same listener still serves the next tunnel, and still writes its row.
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "still-open")
	}))
	t.Cleanup(origin.Close)
	resp, err := client.Get(origin.URL + "/after-the-failure")
	if err != nil {
		t.Fatalf("the listener did not survive its failed dial: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "still-open" {
		t.Fatalf("body = %q, want still-open", body)
	}
	rows := waitForProxyRows(t, st, "mock", 2)
	var ok int
	for _, row := range rows {
		if row.Result == model.ProxyResultOK {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("ok rows = %d of %+v, want exactly 1", ok, rows)
	}
}

// freeDirectPort returns a loopback port nothing is listening on.
func freeDirectPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen to find a free port: %v", err)
	}
	addr := ln.Addr().(*net.TCPAddr)
	_ = ln.Close()
	return fmt.Sprint(addr.Port)
}
