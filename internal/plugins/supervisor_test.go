package plugins

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aiworkgate/internal/model"
)

var (
	fakePluginExe string
	fakeBuildErr  error
)

// Helper modes let this test binary be re-executed as a child process; the job
// object can only be proven across a real process boundary.
const (
	helperEnv    = "AIWORKGATE_TEST_HELPER"
	helperDirEnv = "AIWORKGATE_TEST_HELPER_DIR"
	helperHold   = "hold" // sleeps until killed
	helperJob    = "job"  // starts children, then waits to be killed itself

	// helperHoldTTL bounds how long a leaked helper can survive.
	helperHoldTTL = 2 * time.Minute
)

// TestMain builds the fake plugin once. If the build fails every supervisor
// test skips instead of failing, because the runtime itself needs no compiler.
func TestMain(m *testing.M) {
	// Helper re-invocations re-enter here, before the fake plugin build.
	if mode := os.Getenv(helperEnv); mode != "" {
		os.Exit(runHelperProcess(mode, os.Getenv(helperDirEnv)))
	}
	dir, err := os.MkdirTemp("", "aiworkgate-fakeplugin-*")
	if err != nil {
		fakeBuildErr = err
	} else {
		exe := filepath.Join(dir, "fake-plugin.exe")
		cmd := exec.Command("go", "build", "-o", exe, ".")
		cmd.Dir = filepath.Join("testdata", "fakeplugin")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, buildErr := cmd.CombinedOutput(); buildErr != nil {
			fakeBuildErr = fmt.Errorf("go build fakeplugin: %v\n%s", buildErr, out)
		} else {
			fakePluginExe = exe
		}
	}
	code := m.Run()
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
	os.Exit(code)
}

func requireFakePlugin(t *testing.T) string {
	t.Helper()
	if fakeBuildErr != nil {
		t.Skipf("fake plugin unavailable: %v", fakeBuildErr)
	}
	if fakePluginExe == "" {
		t.Skip("fake plugin was not built")
	}
	return fakePluginExe
}

func installFakePlugin(t *testing.T, mgr *Manager, id, startupMode string) {
	t.Helper()
	exe := requireFakePlugin(t)
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatalf("read fake plugin: %v", err)
	}
	m := baseManifest()
	m.ID = id
	m.Name = "Fake plugin"
	m.Startup.Mode = startupMode
	m.Entry.Exe = "fake-plugin.exe"
	src := customSource(t, m, data)
	if err := mgr.InstallDir(src); err != nil {
		t.Fatalf("install fake plugin: %v", err)
	}
}

func waitStatus(t *testing.T, mgr *Manager, id string, timeout time.Duration, why string, pred func(model.PluginStatus) bool) model.PluginStatus {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		status, ok := mgr.Get(id)
		if !ok {
			t.Fatalf("plugin %q disappeared while waiting for %s", id, why)
		}
		if pred(status) {
			return status
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s; status = running:%v healthy:%v failed:%v pid:%d error:%q",
				why, status.Running, status.Healthy, status.Failed, status.PID, status.LastError)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSupervisorLifecycle(t *testing.T) {
	mgr := newTestManager(t)
	installFakePlugin(t, mgr, "fake", model.StartupAlways)

	first := waitStatus(t, mgr, "fake", 20*time.Second, "healthy first start", func(s model.PluginStatus) bool {
		return s.Running && s.Healthy && s.PID != 0
	})
	if first.Port == 0 {
		t.Fatal("running plugin has no port")
	}
	firstPID := first.PID

	base, token, ok := mgr.Endpoint("fake")
	if !ok || token == "" {
		t.Fatalf("Endpoint = %q,%q,%v; want a running endpoint with token", base, token, ok)
	}

	// stdout capture reaches the ring buffer and the plugin log file.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var joined string
		for _, line := range mgr.RecentOutput("fake", 50) {
			joined += line
		}
		if strings.Contains(joined, "fake plugin listening") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("captured output does not contain the startup line: %q", joined)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if mgr.opts.PluginLog != nil {
		lines, err := mgr.opts.PluginLog.Tail("fake", 20)
		if err != nil {
			t.Fatalf("tail plugin log: %v", err)
		}
		if len(lines) == 0 {
			t.Error("plugin log file is empty after startup")
		}
	}

	// Typed client wrappers against the fake contract.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	accounts, err := mgr.Accounts(ctx, "fake")
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(accounts) != 1 || accounts[0].ID != "acc_1" {
		t.Fatalf("accounts = %+v", accounts)
	}
	meta, err := mgr.Meta(ctx, "fake")
	if err != nil {
		t.Fatalf("Meta: %v", err)
	}
	if meta.Version != "0.1.0" || meta.APILevel != 1 {
		t.Errorf("meta = %+v", meta)
	}
	models, err := mgr.Models(ctx, "fake")
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 1 || models[0].ID != "fake-chat" {
		t.Fatalf("models = %+v", models)
	}
	checkin, err := mgr.Checkin(ctx, "fake", "acc_1")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if checkin.Result != model.CheckinOK || checkin.CreditsAfter == nil || *checkin.CreditsAfter != 110 {
		t.Errorf("checkin = %+v", checkin)
	}
	refresh, err := mgr.Refresh(ctx, "fake", "acc_1")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if refresh.Result != "ok" {
		t.Errorf("refresh = %+v", refresh)
	}
	login, err := mgr.LoginStart(ctx, "fake", "api_key")
	if err != nil {
		t.Fatalf("LoginStart: %v", err)
	}
	if login.SessionID == "" || login.Status != model.LoginNeedInput {
		t.Errorf("login start = %+v", login)
	}
	submitted, err := mgr.LoginSubmit(ctx, "fake", login.SessionID, map[string]string{"api_key": "sk-test"})
	if err != nil {
		t.Fatalf("LoginSubmit: %v", err)
	}
	if submitted.Status != model.LoginSuccess || submitted.AccountID != "acc_1" {
		t.Errorf("login submit = %+v", submitted)
	}
	polled, err := mgr.LoginPoll(ctx, "fake", login.SessionID)
	if err != nil {
		t.Fatalf("LoginPoll: %v", err)
	}
	if polled.Status != model.LoginSuccess {
		t.Errorf("login poll = %+v", polled)
	}
	if err := mgr.LoginCancel(ctx, "fake", login.SessionID); err != nil {
		t.Fatalf("LoginCancel: %v", err)
	}
	if err := mgr.PatchAccount(ctx, "fake", "acc_1", map[string]any{"enabled": false}); err != nil {
		t.Fatalf("PatchAccount: %v", err)
	}
	if err := mgr.DeleteAccount(ctx, "fake", "acc_1"); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	if err := mgr.Reload(ctx, "fake"); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	// Stop is graceful and clears the runtime state.
	if err := mgr.Stop("fake"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	stopped, _ := mgr.Get("fake")
	if stopped.Running || stopped.Healthy || stopped.PID != 0 {
		t.Errorf("status after stop = %+v", stopped)
	}
	if _, _, ok := mgr.Endpoint("fake"); ok {
		t.Error("endpoint still exposed after stop")
	}

	// Start allocates a new port and process.
	if err := mgr.Start("fake"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	second := waitStatus(t, mgr, "fake", 20*time.Second, "healthy restart", func(s model.PluginStatus) bool {
		return s.Running && s.Healthy && s.PID != 0 && s.PID != firstPID
	})

	// Killing the child makes the supervisor restart it automatically.
	proc, err := os.FindProcess(second.PID)
	if err != nil {
		t.Fatalf("find process: %v", err)
	}
	if err := proc.Kill(); err != nil {
		t.Fatalf("kill plugin: %v", err)
	}
	third := waitStatus(t, mgr, "fake", 30*time.Second, "automatic restart after crash", func(s model.PluginStatus) bool {
		return s.Running && s.Healthy && s.PID != 0 && s.PID != second.PID
	})
	if third.PID == second.PID {
		t.Fatal("supervisor did not restart the crashed plugin")
	}
}

func TestSupervisorRestartsAfterHealthFailures(t *testing.T) {
	mgr := newTestManager(t)
	mgr.healthEvery = 25 * time.Millisecond
	mgr.backoff = []time.Duration{20 * time.Millisecond, 20 * time.Millisecond}
	mgr.startTimeout = 10 * time.Second
	installFakePlugin(t, mgr, "fake", model.StartupAlways)

	first := waitStatus(t, mgr, "fake", 20*time.Second, "first start", func(s model.PluginStatus) bool {
		return s.Running && s.Healthy && s.PID != 0
	})
	marker := filepath.Join(mgr.pluginDataDir("fake"), "healthfail")
	writeTestFile(t, marker, []byte("1"))

	restarted := waitStatus(t, mgr, "fake", 20*time.Second, "restart after health failures", func(s model.PluginStatus) bool {
		return s.Running && s.PID != 0 && s.PID != first.PID
	})
	if err := os.Remove(marker); err != nil {
		t.Fatalf("remove health marker: %v", err)
	}
	waitStatus(t, mgr, "fake", 20*time.Second, "healthy after health marker removed", func(s model.PluginStatus) bool {
		return s.Running && s.Healthy && s.PID == restarted.PID
	})
}

func TestSupervisorCrashLoopMarksFailed(t *testing.T) {
	mgr := newTestManager(t)
	mgr.backoff = []time.Duration{10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond}
	mgr.startTimeout = 5 * time.Second
	installFakePlugin(t, mgr, "fake", model.StartupOnDemand)

	// The fake plugin exits immediately while the marker file exists.
	writeTestFile(t, filepath.Join(mgr.pluginDataDir("fake"), "crash"), []byte("1"))
	if err := mgr.Start("fake"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	failed := waitStatus(t, mgr, "fake", 20*time.Second, "failed state", func(s model.PluginStatus) bool {
		return s.Failed
	})
	if failed.Running {
		t.Error("failed plugin should not be running")
	}
	if failed.LastError == "" {
		t.Error("failed plugin should carry the last error")
	}
	// A failed plugin is not retried by EnsureRunning.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := mgr.EnsureRunning(ctx, "fake"); err == nil {
		t.Fatal("EnsureRunning should refuse a failed plugin")
	}
	// Enable is the explicit retry path; remove the marker first.
	if err := os.Remove(filepath.Join(mgr.pluginDataDir("fake"), "crash")); err != nil {
		t.Fatalf("remove crash marker: %v", err)
	}
	if err := mgr.Enable("fake"); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	waitStatus(t, mgr, "fake", 20*time.Second, "recovery after Enable", func(s model.PluginStatus) bool {
		return s.Running && s.Healthy
	})
}

func TestOnDemandWakeAndIdleStop(t *testing.T) {
	mgr := newTestManager(t)
	installFakePlugin(t, mgr, "fake", model.StartupOnDemand)

	status, _ := mgr.Get("fake")
	if status.Running {
		t.Fatal("on-demand plugin must not start at install time")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := mgr.Meta(ctx, "fake"); err != nil {
		t.Fatalf("Meta should wake the plugin: %v", err)
	}
	waitStatus(t, mgr, "fake", 10*time.Second, "running after wake", func(s model.PluginStatus) bool {
		return s.Running && s.Healthy
	})

	mgr.mu.Lock()
	p := mgr.plugins["fake"]
	p.lastUsed = time.Now().Add(-11 * time.Minute)
	p.idleTimeout = 10 * time.Minute
	mgr.mu.Unlock()
	mgr.reapIdle(time.Now())

	waitStatus(t, mgr, "fake", 10*time.Second, "stopped after idle timeout", func(s model.PluginStatus) bool {
		return !s.Running
	})
}

// TestColdStartWaitUsesStartTimeout pins the cold-start bound to the supervisor
// start timeout: with a 300ms start timeout the request must give up after
// ~300ms, not after the old 8s (or the 15s default).
func TestColdStartWaitUsesStartTimeout(t *testing.T) {
	mgr := newTestManager(t)
	mgr.startTimeout = 300 * time.Millisecond
	mgr.backoff = []time.Duration{50 * time.Millisecond}
	installFakePlugin(t, mgr, "fake", model.StartupOnDemand)
	// The plugin starts but /health always fails, so it cannot become healthy.
	writeTestFile(t, filepath.Join(mgr.pluginDataDir("fake"), "healthfail"), []byte("1"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	err := mgr.EnsureRunning(ctx, "fake")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("EnsureRunning must fail for a plugin that never becomes healthy")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("cold start waited %s, want the 300ms start timeout", elapsed)
	}
	if !strings.Contains(err.Error(), "300ms") {
		t.Fatalf("error = %v, want the start timeout named", err)
	}
}

// countingReader records how many bytes a consumer actually read.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// TestPumpOutputHandlesLongLines is the regression for the 1 MiB scanner: a
// longer line used to end the pump and close the pipe, which can crash the
// plugin on its next write. The pump must truncate, keep reading and capture
// the following lines.
func TestPumpOutputHandlesLongLines(t *testing.T) {
	mgr := &Manager{plugins: map[string]*pluginState{"p": {id: "p"}}}
	long := strings.Repeat("x", maxOutputLineBytes+4096)
	input := long + "\nshort line\n"
	src := &countingReader{r: strings.NewReader(input)}

	mgr.wg.Add(1)
	mgr.pumpOutput("p", io.NopCloser(src))

	lines := mgr.RecentOutput("p", 10)
	if len(lines) != 2 {
		t.Fatalf("captured %d lines, want 2 (long line + short line)", len(lines))
	}
	if !strings.Contains(lines[0], "[truncated at") {
		t.Errorf("long line was not marked truncated")
	}
	if len(lines[0]) > maxOutputLineBytes+64 {
		t.Errorf("truncated line length = %d, want the cap to hold", len(lines[0]))
	}
	if lines[1] != "short line" {
		t.Errorf("line after the long one = %q, want %q", lines[1], "short line")
	}
	if src.n != len(input) {
		t.Errorf("pump consumed %d bytes, want all %d: it must never stop reading", src.n, len(input))
	}
}

// TestHelperProcess exists only as a re-exec target, so a helper whose env
// interception broke cannot run the real suite.
func TestHelperProcess(t *testing.T) {
	t.Skip("re-exec target for the job object test")
}

// runHelperProcess implements the helper modes and runs inside the child.
func runHelperProcess(mode, dir string) int {
	switch mode {
	case helperHold:
		// Nothing this process can observe ends it, so its death can only be a
		// kill: that makes it a usable job object witness.
		time.Sleep(helperHoldTTL)
		return 0
	case helperJob:
		return runJobHelper(dir)
	default:
		return helperFail(dir, fmt.Errorf("unknown helper mode %q", mode))
	}
}

// runJobHelper enrols one hold child in the host job object and starts a second
// one outside it, then waits to be force-killed like a crashed host.
func runJobHelper(dir string) int {
	m := &Manager{} // startProcess carries the supervisor's job wiring.

	var assignErr error
	original := assignJob
	assignJob = func(proc *os.Process) error {
		assignErr = original(proc)
		return assignErr
	}
	_, err := startHoldChild(m, dir, "enrolled", true)
	assignJob = original
	if err != nil {
		return helperFail(dir, err)
	}
	if assignErr != nil {
		_ = os.WriteFile(filepath.Join(dir, "enrolled.err"), []byte(assignErr.Error()), 0o600)
	}
	if _, err := startHoldChild(m, dir, "control", false); err != nil {
		return helperFail(dir, err)
	}

	time.Sleep(helperHoldTTL)
	return 0
}

// startHoldChild re-executes this binary in hold mode and writes its pid to
// <dir>/<label>.pid once it is running.
func startHoldChild(m *Manager, dir, label string, enrolled bool) (int, error) {
	cmd, closeStdio := holdChildCommand()
	defer closeStdio()
	var err error
	if enrolled {
		err = m.startProcess(label, cmd)
	} else {
		err = cmd.Start()
	}
	if err != nil {
		return 0, fmt.Errorf("start %s child: %w", label, err)
	}
	pid := cmd.Process.Pid
	if err := os.WriteFile(filepath.Join(dir, label+".pid"), []byte(strconv.Itoa(pid)), 0o600); err != nil {
		return 0, fmt.Errorf("record %s child: %w", label, err)
	}
	return pid, nil
}

// holdChildCommand builds a child that sleeps until it is killed, plus a closer
// for the NUL handle it was wired to. Its stdio goes to NUL, because an
// inherited output pipe would hold the parent's `go test` output open after the
// parent is gone.
func holdChildCommand() (*exec.Cmd, func()) {
	cmd := exec.Command(helperPath(), "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(), helperEnv+"="+helperHold)
	cmd.SysProcAttr = hideWindow()
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return cmd, func() {}
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devNull, devNull, devNull
	return cmd, func() { _ = devNull.Close() }
}

var helperPathCache string

// helperPath returns the running test binary, which doubles as the helper.
func helperPath() string {
	if helperPathCache == "" {
		exe, err := os.Executable()
		if err != nil {
			return os.Args[0]
		}
		helperPathCache = exe
	}
	return helperPathCache
}

// helperFail records why a helper could not set up, for the parent to report.
func helperFail(dir string, err error) int {
	if dir != "" {
		_ = os.WriteFile(filepath.Join(dir, "helper.err"), []byte(err.Error()), 0o600)
	}
	fmt.Fprintln(os.Stderr, "helper:", err)
	return 1
}

// processAlive asks tasklist for the exact pid field; a grandchild cannot be
// Wait()ed from this process, so this is the only liveness signal.
func processAlive(pid int) bool {
	cmd := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/NH", "/FO", "CSV")
	cmd.SysProcAttr = hideWindow()
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), `"`+strconv.Itoa(pid)+`"`)
}

// forceKill terminates a process this test did not spawn.
func forceKill(pid int) {
	if runtime.GOOS == "windows" {
		cmd := exec.Command("taskkill", "/F", "/PID", strconv.Itoa(pid))
		cmd.SysProcAttr = hideWindow()
		_ = cmd.Run()
		return
	}
	if proc, err := os.FindProcess(pid); err == nil {
		_ = proc.Kill()
	}
}

// killAndWait terminates a process and waits for it to disappear, so a
// t.TempDir cleanup never races a process that still locks its files.
func killAndWait(pid int) {
	forceKill(pid)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// readHelperPID waits for a helper pid file and reports a helper failure early.
func readHelperPID(t *testing.T, dir, label string, timeout time.Duration) int {
	t.Helper()
	pidPath := filepath.Join(dir, label+".pid")
	deadline := time.Now().Add(timeout)
	for {
		if data, err := os.ReadFile(pidPath); err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatalf("pid file %q: %v", pidPath, err)
			}
			return pid
		}
		if data, err := os.ReadFile(filepath.Join(dir, "helper.err")); err == nil {
			t.Fatalf("job helper failed: %s", data)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", pidPath)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestJobObjectReapsChildrenWhenHostDies is the regression test for the reported
// bug: plugin children survived a force-killed host and kept their loopback
// ports. The helper stands in for the host, the enrolled hold child for a plugin,
// and the unenrolled one is the control that must survive, which is what proves
// the job object (not a process-tree kill) reaped the child.
func TestJobObjectReapsChildrenWhenHostDies(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("job objects are Windows-only")
	}
	dir := testTempDir(t)

	helper := exec.Command(helperPath(), "-test.run=^TestHelperProcess$")
	helper.Env = append(os.Environ(), helperEnv+"="+helperJob, helperDirEnv+"="+dir)
	helper.SysProcAttr = hideWindow()
	// Deliberately not enrolled: a process already inside another job cannot be
	// moved into the helper's job.
	logFile, err := os.Create(filepath.Join(dir, "helper.log"))
	if err != nil {
		t.Fatalf("create helper log: %v", err)
	}
	defer logFile.Close()
	helper.Stdout, helper.Stderr = logFile, logFile

	if err := helper.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	helperPID := helper.Process.Pid
	t.Cleanup(func() { killAndWait(helperPID) })

	enrolled := readHelperPID(t, dir, "enrolled", 30*time.Second)
	control := readHelperPID(t, dir, "control", 30*time.Second)
	t.Cleanup(func() {
		killAndWait(enrolled)
		killAndWait(control)
	})
	if reason, err := os.ReadFile(filepath.Join(dir, "enrolled.err")); err == nil {
		t.Fatalf("this machine does not allow job enrolment: %s", reason)
	}

	if !waitForProcess(t, enrolled, 10*time.Second, true, "enrolled child while the helper was alive") {
		t.Fatal("the enrolled child was not running before the host died")
	}
	if !waitForProcess(t, control, 10*time.Second, true, "control child while the helper was alive") {
		t.Fatal("the control child was not running before the host died")
	}

	// Kill only the helper: its job handle closes, and every enrolled process
	// must go with it.
	if err := helper.Process.Kill(); err != nil {
		t.Fatalf("kill helper: %v", err)
	}
	_ = helper.Wait()

	if !waitForProcess(t, enrolled, 10*time.Second, false, "enrolled child after the host died") {
		t.Fatalf("enrolled child pid %d outlived the host: KILL_ON_JOB_CLOSE did not reap it", enrolled)
	}
	if !waitForProcess(t, control, 2*time.Second, true, "control child after the host died") {
		t.Fatal("the unenrolled control child died too, so the test proves nothing")
	}
}

// waitForProcess polls tasklist until the process reaches the wanted state.
func waitForProcess(t *testing.T, pid int, timeout time.Duration, alive bool, what string) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if processAlive(pid) == alive {
			return true
		}
		if time.Now().After(deadline) {
			t.Logf("timeout waiting for the %s to be alive=%v", what, alive)
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestStartProcessEnrolsChildInHostJob pins the supervisor's job wiring: every
// started child is handed to the job object.
func TestStartProcessEnrolsChildInHostJob(t *testing.T) {
	mgr := newTestManager(t)
	var enrolled []int
	original := assignJob
	assignJob = func(proc *os.Process) error {
		enrolled = append(enrolled, proc.Pid)
		return original(proc)
	}
	t.Cleanup(func() { assignJob = original })

	cmd, closeStdio := holdChildCommand()
	t.Cleanup(closeStdio)
	if err := mgr.startProcess("fake", cmd); err != nil {
		t.Fatalf("startProcess: %v", err)
	}
	t.Cleanup(func() { killAndWait(cmd.Process.Pid) })

	if len(enrolled) != 1 || enrolled[0] != cmd.Process.Pid {
		t.Fatalf("job enrolments = %v, want exactly one for pid %d", enrolled, cmd.Process.Pid)
	}
}

// TestStartProcessLogsJobEnrolmentFailure keeps the promise that a plugin always
// starts, and that the reason it is unprotected stays visible.
func TestStartProcessLogsJobEnrolmentFailure(t *testing.T) {
	mgr := newTestManager(t)
	original := assignJob
	assignJob = func(*os.Process) error { return errors.New("already assigned to another job") }
	t.Cleanup(func() { assignJob = original })

	cmd, closeStdio := holdChildCommand()
	t.Cleanup(closeStdio)
	if err := mgr.startProcess("fake", cmd); err != nil {
		t.Fatalf("startProcess must not fail when enrolment fails: %v", err)
	}
	t.Cleanup(func() { killAndWait(cmd.Process.Pid) })
	if cmd.Process == nil || cmd.Process.Pid <= 0 {
		t.Fatal("the child was not started")
	}

	entry := hostLogEntry(t, mgr, "plugin not enrolled in host job object")
	if entry["level"] != "warn" {
		t.Errorf("level = %v, want warn", entry["level"])
	}
	if entry["plugin"] != "fake" {
		t.Errorf("plugin = %v, want fake", entry["plugin"])
	}
	if !strings.Contains(fmt.Sprint(entry["error"]), "another job") {
		t.Errorf("error = %v, want the enrolment reason", entry["error"])
	}
	if fmt.Sprint(entry["pid"]) != strconv.Itoa(cmd.Process.Pid) {
		t.Errorf("pid = %v, want %d", entry["pid"], cmd.Process.Pid)
	}
}

// hostLogEntry reads one JSONL entry out of the manager's host log.
func hostLogEntry(t *testing.T, mgr *Manager, msg string) map[string]any {
	t.Helper()
	path := filepath.Join(mgr.dataDir, "logs", "host-"+time.Now().Format("2006-01-02")+".jsonl")
	deadline := time.Now().Add(5 * time.Second)
	for {
		f, err := os.Open(path)
		if err == nil {
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 64*1024), 1024*1024)
			for sc.Scan() {
				var entry map[string]any
				if json.Unmarshal(sc.Bytes(), &entry) != nil {
					continue
				}
				if entry["msg"] == msg {
					_ = f.Close()
					return entry
				}
			}
			_ = f.Close()
		}
		if time.Now().After(deadline) {
			t.Fatalf("host log %s has no entry %q", path, msg)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestEnsureJobObjectIsIdempotent keeps the singleton cheap enough for the host
// to pre-create at startup.
func TestEnsureJobObjectIsIdempotent(t *testing.T) {
	for call := 1; call <= 2; call++ {
		if err := EnsureJobObject(); err != nil {
			t.Fatalf("EnsureJobObject call %d: %v", call, err)
		}
	}
}

// portTestServer starts a handler-backed endpoint and returns its port. No
// plugin process is involved: these tests pin the start sequence itself.
func portTestServer(t *testing.T, handler http.Handler) int {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server url %q: %v", srv.URL, err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse test server port %q: %v", u.Port(), err)
	}
	return port
}

// testChild is a started child process shape, without a real process: only the
// PID is read by the ownership gate.
func testChild(port, pid int, token string) *process {
	return &process{
		id:     "fake",
		cmd:    &exec.Cmd{Process: &os.Process{Pid: pid}},
		port:   port,
		token:  token,
		exited: make(chan struct{}),
	}
}

// TestForeignPortOwnerFailsStartBeforeTokenRequest is the regression for the
// allocatePort TOCTOU: when the listener cannot be proven to belong to the
// child, the start must abort and the token-bearing /health poll must never
// happen.
func TestForeignPortOwnerFailsStartBeforeTokenRequest(t *testing.T) {
	var requests, authorized atomic.Int32
	port := portTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "" {
			authorized.Add(1)
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))

	mismatch := fmt.Errorf("plugins: port 127.0.0.1:%d is owned by PID 999, not the plugin child PID 4242 (token not sent)", port)
	mgr := &Manager{
		startTimeout: 2 * time.Second,
		verifyOwner: func(gotPort, gotPID int, timeout time.Duration) error {
			if gotPort != port || gotPID != 4242 {
				t.Errorf("verifyOwner(port=%d, pid=%d), want (%d, 4242)", gotPort, gotPID, port)
			}
			if timeout <= 0 {
				t.Errorf("verifyOwner timeout = %s, want a positive budget", timeout)
			}
			return mismatch
		},
	}

	ps := testChild(port, 4242, "secret-token")
	err := mgr.awaitHealthy(context.Background(), ps)
	if !errors.Is(err, mismatch) {
		t.Fatalf("awaitHealthy error = %v, want the ownership mismatch", err)
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("plugin endpoint saw %d requests after a failed ownership check, want 0", got)
	}
	if got := authorized.Load(); got != 0 {
		t.Errorf("plugin endpoint saw %d token-bearing requests, want 0", got)
	}
}

// TestPortOwnershipVerifiedBeforeFirstHealthRequest proves the ordering of the
// successful path: the health request exists, carries the token, and only
// arrives after the verifier returned.
func TestPortOwnershipVerifiedBeforeFirstHealthRequest(t *testing.T) {
	var requests, authorized atomic.Int32
	var verified atomic.Bool
	var premature atomic.Bool
	port := portTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") == "Bearer secret-token" {
			authorized.Add(1)
		}
		if !verified.Load() {
			premature.Store(true)
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))

	mgr := &Manager{
		startTimeout:  2 * time.Second,
		healthTimeout: time.Second,
		verifyOwner: func(gotPort, gotPID int, timeout time.Duration) error {
			verified.Store(true)
			return nil
		},
	}

	if err := mgr.awaitHealthy(context.Background(), testChild(port, 4242, "secret-token")); err != nil {
		t.Fatalf("awaitHealthy: %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("plugin endpoint saw %d requests, want exactly 1 health poll", got)
	}
	if got := authorized.Load(); got != 1 {
		t.Errorf("authorized requests = %d, want 1", got)
	}
	if premature.Load() {
		t.Error("health request arrived before the ownership check completed")
	}
}
