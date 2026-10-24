package plugins

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// process is one plugin child process. exited is closed once cmd.Wait has
// returned; waitErr is only valid after that.
type process struct {
	// id is carried only so the stop path can log which plugin was stopped.
	id      string
	cmd     *exec.Cmd
	port    int
	token   string
	exited  chan struct{}
	waitErr error
}

// Supervision cadence. A 10s poll with two strikes flags a dead plugin within
// ~20s, where the old 30s x 3 allowed up to 90s of traffic to a dead child.
const (
	healthCheckEvery     = 10 * time.Second
	healthFailureStrikes = 2
	// softStopWait is how long terminate waits after a graceful (taskkill
	// without /F) request before force-killing the tree.
	softStopWait = 3 * time.Second
)

// stopReason explains why the watch loop returned.
type stopReason int

const (
	reasonStop stopReason = iota
	reasonCrash
	reasonUnhealthy
)

// runPlugin is the per-plugin supervisor: it launches the process, waits for
// health, watches it and restarts it with exponential backoff until the plugin
// is stopped or reaches the failure threshold.
func (m *Manager) runPlugin(p *pluginState, ctx context.Context) {
	defer m.wg.Done()
	defer m.finishRun(p)

	var failures []time.Time
	for {
		if ctx.Err() != nil {
			return
		}
		ps, err := m.launch(p, ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			m.setError(p, err)
			m.logWarn("plugin start failed", map[string]any{"plugin": p.id, "error": err.Error()})
			if !m.addFailure(&failures) {
				m.markFailed(p, err)
				return
			}
			if !m.waitBackoff(ctx, len(failures)-1) {
				return
			}
			continue
		}

		reason := m.watch(p, ps, ctx)
		m.terminate(ps)
		if reason == reasonStop || ctx.Err() != nil {
			return
		}

		err = fmt.Errorf("plugin exited unexpectedly: %s", exitDescription(ps))
		m.setError(p, err)
		m.logWarn("plugin exited", map[string]any{"plugin": p.id, "error": err.Error(), "reason": reason.String()})
		if !m.addFailure(&failures) {
			m.markFailed(p, err)
			return
		}
		if !m.waitBackoff(ctx, len(failures)-1) {
			return
		}
	}
}

func (r stopReason) String() string {
	switch r {
	case reasonStop:
		return "stopped"
	case reasonUnhealthy:
		return "unhealthy"
	default:
		return "crashed"
	}
}

// finishRun clears process state and releases Stop waiters.
func (m *Manager) finishRun(p *pluginState) {
	m.mu.Lock()
	p.running = false
	p.healthy = false
	p.starting = false
	p.port = 0
	p.pid = 0
	p.startedAt = 0
	// No child, no environment: nothing can be pointing at a closed listener.
	p.directEgress = ""
	p.runCancel = nil
	done := p.runDone
	p.runDone = nil
	m.mu.Unlock()
	if done != nil {
		close(done)
	}
}

// launch allocates a port, injects the environment, starts the entry process
// and waits up to the start timeout for /health.
func (m *Manager) launch(p *pluginState, ctx context.Context) (*process, error) {
	m.mu.Lock()
	id := p.id
	dir := p.dir
	mf := p.manifest
	m.mu.Unlock()

	if dir == "" {
		return nil, errors.New("plugins: plugin has no installation directory")
	}
	exe := filepath.Join(dir, filepath.FromSlash(mf.Entry.Exe))
	if !isRegularFile(exe) {
		return nil, fmt.Errorf("plugins: %q: entry.exe %q is missing", id, mf.Entry.Exe)
	}
	port, err := allocatePort()
	if err != nil {
		return nil, err
	}
	token, err := m.tokenFor(id)
	if err != nil {
		return nil, err
	}
	dataDir := m.pluginDataDir(id)
	if dataDir == "" {
		return nil, errors.New("plugins: data directory is not configured")
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("plugins: create plugin data dir %q: %w", dataDir, err)
	}
	rulesFile := m.RulesFile(id)
	egressFile := m.EgressFile(id)
	cfg := m.currentConfig()

	cmd := exec.Command(exe, mf.Entry.Args...)
	cmd.Dir = dir
	env := buildPluginEnv(os.Environ(), id, port, token, dataDir, rulesFile, egressFile, APILevelFor(mf), cfg.Proxy)
	// Direct egress stays fail-open: with no listener the child gets exactly the
	// environment it always got. The listener URL is only advertised when it is
	// actually bound, so an environment-aware client can never be told to route
	// through a port that is not there.
	listener := m.directEgressURL(id, cfg.Proxy)
	if listener != "" {
		env = applyDirectEgress(env, listener, cfg.Proxy.NoProxy)
	}
	cmd.Env = env
	cmd.SysProcAttr = pluginProcAttr()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("plugins: start %q: stdout pipe: %w", exe, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdout.Close()
		return nil, fmt.Errorf("plugins: start %q: stderr pipe: %w", exe, err)
	}
	if err := m.startProcess(id, cmd); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("plugins: start %q: %w", exe, err)
	}

	ps := &process{id: id, cmd: cmd, port: port, token: token, exited: make(chan struct{})}
	m.wg.Add(3)
	go m.pumpOutput(id, stdout)
	go m.pumpOutput(id, stderr)
	go func() {
		defer m.wg.Done()
		ps.waitErr = cmd.Wait()
		close(ps.exited)
	}()

	m.mu.Lock()
	p.running = true
	p.healthy = false
	p.starting = true
	p.port = port
	p.pid = cmd.Process.Pid
	p.startedAt = m.now().Unix()
	p.token = token
	p.lastUsed = m.now()
	// What this child was actually told, so the host can later prove whether it
	// still names a listener it serves. A relaunch that injects nothing must
	// clear the record of the previous child, hence the unconditional write.
	p.directEgress = listener
	m.mu.Unlock()
	m.logInfo("plugin process started", map[string]any{"plugin": id, "pid": cmd.Process.Pid, "port": port})

	if err := m.awaitHealthy(ctx, ps); err != nil {
		m.mu.Lock()
		p.starting = false
		m.mu.Unlock()
		m.logWarn("plugin did not become ready; stopping child", map[string]any{
			"plugin": id, "pid": cmd.Process.Pid, "port": port, "error": err.Error(),
		})
		m.terminate(ps)
		return nil, err
	}
	m.mu.Lock()
	p.healthy = true
	p.starting = false
	p.lastError = ""
	// A success clears any failure left over from an earlier attempt.
	p.failed = false
	p.failedAt = time.Time{}
	p.lastUsed = m.now()
	m.mu.Unlock()
	return ps, nil
}

// assignJob enrols a started child in the host job object; tests replace it to
// exercise the failure path.
var assignJob = assignToJob

// startProcess starts cmd and enrols the child in the host job object. A child
// that cannot be enrolled still starts: only the orphan guarantee is lost, so
// the reason is logged, never returned.
func (m *Manager) startProcess(id string, cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	// Enrolled after Start, not at CREATE_SUSPENDED: os/exec hides the resume handle.
	if err := assignJob(cmd.Process); err != nil {
		m.logWarn("plugin not enrolled in host job object", map[string]any{
			"plugin": id, "pid": cmd.Process.Pid, "error": err.Error(),
		})
	}
	return nil
}

// watch waits for the process to exit or the context to be cancelled, polling
// /health every healthEvery. Two consecutive failures are treated as a crash:
// with the 10s cadence the supervisor replaces a dead child within ~20s, and
// Endpoint stops handing out its port as soon as the first failure is marked.
func (m *Manager) watch(p *pluginState, ps *process, ctx context.Context) stopReason {
	every := m.healthEvery
	if every <= 0 {
		every = healthCheckEvery
	}
	strikes := healthFailureStrikes
	if strikes <= 0 {
		strikes = 2
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	failures := 0
	for {
		select {
		case <-ctx.Done():
			return reasonStop
		case <-ps.exited:
			if ctx.Err() != nil {
				return reasonStop
			}
			return reasonCrash
		case <-ticker.C:
			if err := m.checkHealth(ps); err != nil {
				failures++
				m.mu.Lock()
				p.healthy = false
				p.lastError = "health check failed: " + err.Error()
				m.mu.Unlock()
				if failures >= strikes {
					return reasonUnhealthy
				}
				continue
			}
			failures = 0
			m.mu.Lock()
			p.healthy = true
			if strings.HasPrefix(p.lastError, "health check failed") {
				p.lastError = ""
			}
			m.mu.Unlock()
		}
	}
}

// awaitHealthy proves the listener on the plugin port belongs to the child and
// only then polls /health. The ownership check must come first: allocatePort
// released the port before Start, so a local process can win the race and would
// otherwise receive the plugin token with the first health request.
func (m *Manager) awaitHealthy(ctx context.Context, ps *process) error {
	if err := m.awaitPortOwner(ctx, ps); err != nil {
		return err
	}
	return m.waitHealthy(ctx, ps)
}

// awaitPortOwner runs the ownership verifier off the caller's goroutine so a
// child exit or a cancelled start aborts the wait immediately. The verifier is
// bounded by portVerifyTimeout, so an abandoned goroutine ends within it.
func (m *Manager) awaitPortOwner(ctx context.Context, ps *process) error {
	if ps == nil || ps.cmd == nil || ps.cmd.Process == nil {
		return errors.New("plugins: cannot verify the plugin port: no child process")
	}
	timeout := m.portVerifyTimeout
	if timeout <= 0 {
		timeout = defaultPortVerifyTimeout
	}
	verify := m.verifyOwner
	if verify == nil {
		verify = verifyPortOwner
	}
	pid := ps.cmd.Process.Pid
	done := make(chan error, 1)
	go func() {
		done <- verify(ps.port, pid, timeout)
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-ps.exited:
		return fmt.Errorf("plugin exited during startup: %s", exitDescription(ps))
	}
}

// waitHealthy polls /health until the process answers, exits or the start
// timeout elapses.
func (m *Manager) waitHealthy(ctx context.Context, ps *process) error {
	timeout := m.startTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	var lastErr error
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ps.exited:
			return fmt.Errorf("plugin exited during startup: %s", exitDescription(ps))
		case <-deadline.C:
			if lastErr != nil {
				return fmt.Errorf("plugin did not become healthy within %s: %w", timeout, lastErr)
			}
			return fmt.Errorf("plugin did not become healthy within %s", timeout)
		case <-ticker.C:
			if err := m.checkHealth(ps); err != nil {
				lastErr = err
				continue
			}
			return nil
		}
	}
}

// checkHealth calls GET /health with the plugin token.
func (m *Manager) checkHealth(ps *process) error {
	if ps == nil || ps.port == 0 {
		return errors.New("no plugin port")
	}
	timeout := m.healthTimeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/health", ps.port), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+ps.token)
	client := &http.Client{Timeout: timeout, Transport: pluginHTTPTransport()}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("health returned %s", resp.Status)
	}
	var payload struct {
		OK     *bool  `json:"ok"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &payload); err == nil {
		if payload.OK != nil && !*payload.OK {
			return errors.New("health reported ok=false")
		}
		if payload.Status != "" && !strings.EqualFold(payload.Status, "ok") {
			return fmt.Errorf("health status %q", payload.Status)
		}
	}
	return nil
}

// terminate stops the process: a graceful attempt first (Windows taskkill
// without /F), a short wait, then a forced kill of the whole process tree. The
// path actually taken is logged, because taskkill without /F only reaches
// windowed processes and a console plugin almost always needs the force path.
func (m *Manager) terminate(ps *process) {
	if ps == nil || ps.cmd == nil {
		return
	}
	select {
	case <-ps.exited:
		return
	default:
	}
	if gracefulStop(ps.cmd.Process) {
		select {
		case <-ps.exited:
			m.logInfo("plugin stopped gracefully", map[string]any{"plugin": ps.id, "pid": ps.cmd.Process.Pid})
			return
		case <-time.After(softStopWait):
			m.logWarn("plugin ignored the graceful stop; forcing", map[string]any{
				"plugin": ps.id, "pid": ps.cmd.Process.Pid, "waited": softStopWait.String(),
			})
		}
	} else {
		// The soft request could not even be delivered (typical for console
		// children); there is nothing to wait for.
		m.logInfo("graceful stop unavailable; forcing", map[string]any{"plugin": ps.id, "pid": ps.cmd.Process.Pid})
	}
	killProcessTree(ps.cmd.Process)
	select {
	case <-ps.exited:
	case <-time.After(3 * time.Second):
	}
}

// tokenFor returns the persisted plugin token, generating one on first use.
func (m *Manager) tokenFor(id string) (string, error) {
	if m.opts.Secrets != nil {
		tok, err := m.opts.Secrets.EnsurePluginToken(id)
		if err != nil {
			return "", fmt.Errorf("plugins: token for %q: %w", id, err)
		}
		return tok, nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("plugins: generate token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// allocatePort reserves a free loopback port by listening on :0 and closing
// the listener immediately. The port is therefore not held when the child
// starts; the supervisor proves the listener belongs to the child
// (verifyPortOwner) before it sends the plugin token to it.
func allocatePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("plugins: allocate port: %w", err)
	}
	defer ln.Close()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return 0, errors.New("plugins: allocate port: unexpected listener address")
	}
	return addr.Port, nil
}

// maxOutputLineBytes caps one captured log line. A longer line is truncated
// and the remainder discarded, but reading continues: closing the pipe (what a
// scanner error used to cause) can kill the plugin on its next write.
const maxOutputLineBytes = 1 << 20

// pumpOutput copies child output line by line into the plugin log and the
// in-memory ring buffer. It never stops reading early, whatever the child
// writes, so a huge single line cannot wedge or crash the plugin.
func (m *Manager) pumpOutput(id string, r io.ReadCloser) {
	defer m.wg.Done()
	defer r.Close()
	br := bufio.NewReaderSize(r, 64*1024)
	line := make([]byte, 0, 512)
	truncated := false
	for {
		chunk, err := br.ReadSlice('\n')
		if len(chunk) > 0 {
			room := maxOutputLineBytes - len(line)
			switch {
			case room <= 0:
				// At the cap, only real content counts as a truncation; a lone
				// newline is just the line terminator.
				if strings.TrimRight(string(chunk), "\r\n") != "" {
					truncated = true
				}
			case len(chunk) > room:
				line = append(line, chunk[:room]...)
				truncated = true
			default:
				line = append(line, chunk...)
			}
		}
		switch {
		case err == nil:
			m.emitOutput(id, line, truncated)
			line = line[:0]
			truncated = false
		case errors.Is(err, bufio.ErrBufferFull):
			continue // keep consuming the same over-long line
		default:
			// EOF or a read error: flush a trailing line without newline,
			// exactly like bufio.Scanner's final token.
			if len(line) > 0 || truncated {
				m.emitOutput(id, line, truncated)
			}
			return
		}
	}
}

// emitOutput appends one captured line, marking a truncated one so the log does
// not silently look complete.
func (m *Manager) emitOutput(id string, data []byte, truncated bool) {
	line := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if truncated {
		line += fmt.Sprintf(" [truncated at %d bytes]", maxOutputLineBytes)
	}
	if m.opts.PluginLog != nil {
		_ = m.opts.PluginLog.Write(id, line)
	}
	m.appendOutput(id, line)
}

// addFailure records a failure and reports whether another restart is allowed.
func (m *Manager) addFailure(failures *[]time.Time) bool {
	now := m.now()
	window := m.failureWindow
	if window <= 0 {
		window = 10 * time.Minute
	}
	kept := (*failures)[:0]
	for _, t := range *failures {
		if now.Sub(t) <= window {
			kept = append(kept, t)
		}
	}
	kept = append(kept, now)
	*failures = kept
	max := m.maxFailures
	if max <= 0 {
		max = 5
	}
	return len(kept) < max
}

// waitBackoff sleeps for the attempt's backoff delay, returning false when the
// supervisor was cancelled.
func (m *Manager) waitBackoff(ctx context.Context, attempt int) bool {
	list := m.backoff
	if len(list) == 0 {
		list = []time.Duration{time.Second}
	}
	if attempt < 0 {
		attempt = 0
	}
	if attempt >= len(list) {
		attempt = len(list) - 1
	}
	delay := list[attempt]
	if delay <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// setError stores the latest failure reason.
func (m *Manager) setError(p *pluginState, err error) {
	if err == nil {
		return
	}
	m.mu.Lock()
	p.lastError = err.Error()
	p.healthy = false
	m.mu.Unlock()
}

// markFailed flips the plugin into the failed state and stops retrying. The
// timestamp lets Manager.expireFailure retry automatically after the failure
// window instead of replaying the error until a manual Enable.
func (m *Manager) markFailed(p *pluginState, err error) {
	m.mu.Lock()
	p.failed = true
	p.failedAt = m.now()
	p.healthy = false
	if err != nil {
		p.lastError = err.Error()
	}
	msg := p.lastError
	m.mu.Unlock()
	m.logError("plugin marked failed", map[string]any{"plugin": p.id, "error": msg})
}

// exitDescription renders why the child ended.
func exitDescription(ps *process) string {
	if ps == nil {
		return "no process"
	}
	if ps.waitErr != nil {
		return ps.waitErr.Error()
	}
	return "process exited with status 0"
}
