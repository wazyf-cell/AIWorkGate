//go:build windows

package plugins

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// createNoWindow is CREATE_NO_WINDOW, used for helper processes so taskkill
// does not flash a console on a GUI host.
const createNoWindow = 0x08000000

// jobProcessAccess is the minimum rights AssignProcessToJobObject needs.
const jobProcessAccess = windows.PROCESS_SET_QUOTA | windows.PROCESS_TERMINATE

// hideWindow keeps a child from opening a console window.
func hideWindow() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
}

// pluginProcAttr is the creation attribute for plugin children.
func pluginProcAttr() *syscall.SysProcAttr { return hideWindow() }

// hostJob is the process-wide kill-on-close job object. Its handle is never
// closed by the host, so a killed or crashed host releases (and terminates)
// every enrolled plugin process.
var (
	hostJobMu     sync.Mutex
	hostJobHandle windows.Handle
)

// EnsureJobObject creates the host job on first use and returns the reason it
// is unavailable, if any. The host may call it at startup; the supervisor also
// calls it lazily.
func EnsureJobObject() error {
	_, err := hostJob()
	return err
}

// hostJob returns the singleton handle, creating the job once. A failed attempt
// is not cached, so a transient rejection does not disable the guard forever.
func hostJob() (windows.Handle, error) {
	hostJobMu.Lock()
	defer hostJobMu.Unlock()
	if hostJobHandle != 0 {
		return hostJobHandle, nil
	}

	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("job object: CreateJobObject: %w", err)
	}
	basic := windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
		LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
	}
	// Extended limits: JobObjectBasicLimitInformation is rejected with
	// ERROR_INVALID_PARAMETER on this Windows build.
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{BasicLimitInformation: basic}
	if _, err := windows.SetInformationJobObject(h, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		_ = windows.CloseHandle(h)
		return 0, fmt.Errorf("job object: set KILL_ON_JOB_CLOSE: %w", err)
	}
	hostJobHandle = h
	return hostJobHandle, nil
}

// assignToJob enrols an already started process so it dies with the host.
func assignToJob(proc *os.Process) error {
	if proc == nil {
		return errors.New("job object: no process")
	}
	job, err := hostJob()
	if err != nil {
		return err
	}
	ph, err := windows.OpenProcess(jobProcessAccess, false, uint32(proc.Pid))
	if err != nil {
		return fmt.Errorf("job object: open process %d: %w", proc.Pid, err)
	}
	defer windows.CloseHandle(ph)
	if err := windows.AssignProcessToJobObject(job, ph); err != nil {
		return fmt.Errorf("job object: assign process %d: %w", proc.Pid, err)
	}
	return nil
}

// gracefulStopCommand is the soft stop: taskkill without /F asks the process
// (and its tree) to close. It only works for processes that own a window, so a
// console plugin usually answers nothing and the caller falls back to /F.
func gracefulStopCommand(pid int) *exec.Cmd {
	cmd := exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T")
	cmd.SysProcAttr = hideWindow()
	return cmd
}

// forceStopCommand is the hard stop of the process and all descendants.
func forceStopCommand(pid int) *exec.Cmd {
	cmd := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid))
	cmd.SysProcAttr = hideWindow()
	return cmd
}

// gracefulStop attempts the soft stop and reports whether the request was
// delivered.
func gracefulStop(proc *os.Process) bool {
	if proc == nil {
		return false
	}
	return gracefulStopCommand(proc.Pid).Run() == nil
}

// killProcessTree force-terminates the process and all descendants.
func killProcessTree(proc *os.Process) {
	if proc == nil {
		return
	}
	_ = forceStopCommand(proc.Pid).Run()
	_ = proc.Kill()
}

// Port-ownership verification. allocatePort releases its probe listener before
// the child starts, so a local process can win the race for the port and would
// receive the plugin token with the first /health request. The supervisor
// therefore proves the listener belongs to the child before sending anything.
const (
	// defaultPortVerifyTimeout bounds the wait for the child's listener, which
	// only binds after Start returns.
	defaultPortVerifyTimeout = 5 * time.Second
	// portVerifyInterval is the polling cadence while the child boots.
	portVerifyInterval = 100 * time.Millisecond
)

// netstatOutput returns the Windows TCP endpoint table. It is a variable so
// tests can drive the parser and the polling loop without spawning netstat.
// The full table is requested on purpose: "-p TCP" omits IPv6 rows on some
// Windows builds, which would reject a plugin that binds a dual-stack [::]
// listener. UDP rows are filtered out by the parser.
var netstatOutput = func() ([]byte, error) {
	cmd := exec.Command("netstat", "-ano")
	cmd.SysProcAttr = hideWindow()
	return cmd.Output()
}

// tcpListener is one LISTENING row from netstat.
type tcpListener struct {
	host string // local address with IPv6 brackets stripped: 127.0.0.1, 0.0.0.0, ::
	port int
	pid  int
}

// parseTCPListeners extracts LISTENING TCP rows. It is header-agnostic because
// netstat localizes its header and some builds label IPv6 rows "TCP" while
// others print "TCPv6".
func parseTCPListeners(out []byte) []tcpListener {
	var rows []tcpListener
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		switch strings.ToUpper(fields[0]) {
		case "TCP", "TCPV6":
		default:
			continue
		}
		if !strings.EqualFold(fields[3], "LISTENING") {
			continue
		}
		host, port, ok := splitNetstatEndpoint(fields[1])
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(fields[4])
		if err != nil || pid <= 0 {
			continue
		}
		rows = append(rows, tcpListener{host: host, port: port, pid: pid})
	}
	return rows
}

// splitNetstatEndpoint parses a local-address column such as "127.0.0.1:51789"
// or "[::]:51789", returning the host without IPv6 brackets.
func splitNetstatEndpoint(s string) (string, int, bool) {
	i := strings.LastIndexByte(s, ':')
	if i <= 0 || i == len(s)-1 {
		return "", 0, false
	}
	port, err := strconv.Atoi(s[i+1:])
	if err != nil || port <= 0 || port > 65535 {
		return "", 0, false
	}
	host := strings.TrimSuffix(strings.TrimPrefix(s[:i], "["), "]")
	return host, port, true
}

// matchesPort reports whether the listener can carry a request addressed to
// 127.0.0.1:port. A plugin may bind loopback, all IPv4 interfaces or the IPv6
// wildcard (dual-stack); a listener on another interface address cannot be the
// one the health check reaches and is not accepted.
func (l tcpListener) matchesPort(port int) bool {
	if l.port != port {
		return false
	}
	switch l.host {
	case "127.0.0.1", "0.0.0.0", "::":
		return true
	}
	return false
}

// listenerPIDs returns the de-duplicated, sorted PIDs listening on port on an
// address the supervisor could reach.
func listenerPIDs(rows []tcpListener, port int) []int {
	seen := make(map[int]struct{}, 1)
	var pids []int
	for _, row := range rows {
		if !row.matchesPort(port) {
			continue
		}
		if _, ok := seen[row.pid]; ok {
			continue
		}
		seen[row.pid] = struct{}{}
		pids = append(pids, row.pid)
	}
	sort.Ints(pids)
	return pids
}

// tcpListenersOnPort snapshots netstat and returns the owners of the port.
func tcpListenersOnPort(port int) ([]int, error) {
	out, err := netstatOutput()
	if err != nil {
		return nil, fmt.Errorf("netstat: %w", err)
	}
	return listenerPIDs(parseTCPListeners(out), port), nil
}

// processParents snapshots the parent link of every live process via a
// toolhelp snapshot, which needs no admin rights and no PowerShell.
func processParents() (map[uint32]uint32, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("process snapshot: %w", err)
	}
	defer windows.CloseHandle(snap)

	parents := make(map[uint32]uint32)
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Process32First(snap, &entry); err != nil {
		return nil, fmt.Errorf("process snapshot: %w", err)
	}
	for {
		parents[entry.ProcessID] = entry.ParentProcessID
		if err := windows.Process32Next(snap, &entry); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				break
			}
			return nil, fmt.Errorf("process snapshot: %w", err)
		}
	}
	return parents, nil
}

// isDescendant reports whether owner sits below root in the parent map. The hop
// cap stops a recycled parent PID from looping.
func isDescendant(owner, root uint32, parents map[uint32]uint32) bool {
	if owner == 0 || root == 0 || owner == root {
		return false
	}
	pid := owner
	for hops := 0; hops < 64; hops++ {
		parent, ok := parents[pid]
		if !ok || parent == 0 || parent == pid {
			return false
		}
		if parent == root {
			return true
		}
		pid = parent
	}
	return false
}

// childOwner returns the listener PID that belongs to the plugin child, either
// the child itself or a process it launched. When the parent snapshot is
// unavailable only a direct match is accepted (fail closed).
func childOwner(owners []int, pid int) (int, bool) {
	for _, owner := range owners {
		if owner == pid {
			return owner, true
		}
	}
	parents, err := processParents()
	if err != nil {
		return 0, false
	}
	for _, owner := range owners {
		if isDescendant(uint32(owner), uint32(pid), parents) {
			return owner, true
		}
	}
	return 0, false
}

// joinPIDs renders the owner list for the mismatch error.
func joinPIDs(pids []int) string {
	parts := make([]string, len(pids))
	for i, pid := range pids {
		parts[i] = strconv.Itoa(pid)
	}
	if len(parts) == 1 {
		return "PID " + parts[0]
	}
	return "PIDs " + strings.Join(parts, ", ")
}

// verifyPortOwner waits until a listener for port belongs to pid or one of its
// descendants. A foreign listener fails fast: it can never become ours. A child
// that is slow to bind is polled until timeout because it binds after Start
// returns. The returned errors state that no token was sent.
//
// The check is a snapshot, not a lock: it closes the allocatePort window in
// which a squatter is already listening before the child starts, but it cannot
// prove which socket accepts a connection that is made after it returns. A
// squatter that re-binds with SO_REUSEADDR between the check and the first
// request, or shares an overlapping listener, is beyond what netstat reports.
func verifyPortOwner(port int, pid int, timeout time.Duration) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("plugins: verify port owner: invalid port %d", port)
	}
	if pid <= 0 {
		return fmt.Errorf("plugins: verify port owner: invalid pid %d", pid)
	}
	if timeout <= 0 {
		timeout = defaultPortVerifyTimeout
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		owners, err := tcpListenersOnPort(port)
		switch {
		case err != nil:
			lastErr = err
		case len(owners) > 0:
			if _, ok := childOwner(owners, pid); ok {
				return nil
			}
			return fmt.Errorf("plugins: port 127.0.0.1:%d is owned by %s, not the plugin child PID %d (token not sent)",
				port, joinPIDs(owners), pid)
		}
		if !time.Now().Before(deadline) {
			if lastErr != nil {
				return fmt.Errorf("plugins: port 127.0.0.1:%d was not bound by the plugin child PID %d within %s: %w (token not sent)",
					port, pid, timeout, lastErr)
			}
			return fmt.Errorf("plugins: port 127.0.0.1:%d was not bound by the plugin child PID %d within %s (token not sent)",
				port, pid, timeout)
		}
		time.Sleep(portVerifyInterval)
	}
}
