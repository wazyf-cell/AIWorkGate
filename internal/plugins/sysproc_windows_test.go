//go:build windows

package plugins

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestStopCommandConstruction pins the two stop paths without touching a real
// plugin: the soft stop must not carry /F (it is the graceful attempt) and the
// force stop must.
func TestStopCommandConstruction(t *testing.T) {
	const pid = 4242
	pidText := strconv.Itoa(pid)

	soft := gracefulStopCommand(pid)
	if soft.Args[0] != "taskkill" {
		t.Fatalf("soft stop command = %q, want taskkill", soft.Args[0])
	}
	if slices.Contains(soft.Args, "/F") {
		t.Errorf("soft stop must not force: %q", soft.Args)
	}
	if !slices.Contains(soft.Args, "/PID") || !slices.Contains(soft.Args, pidText) {
		t.Errorf("soft stop targets %q, want /PID %s", soft.Args, pidText)
	}
	if !slices.Contains(soft.Args, "/T") {
		t.Errorf("soft stop must cover the tree: %q", soft.Args)
	}

	force := forceStopCommand(pid)
	if !slices.Contains(force.Args, "/F") {
		t.Errorf("force stop must use /F: %q", force.Args)
	}
	if !slices.Contains(force.Args, "/PID") || !slices.Contains(force.Args, pidText) {
		t.Errorf("force stop targets %q, want /PID %s", force.Args, pidText)
	}
	if !slices.Contains(force.Args, "/T") {
		t.Errorf("force stop must cover the tree: %q", force.Args)
	}
	if force.SysProcAttr == nil || !force.SysProcAttr.HideWindow {
		t.Error("helper process must not flash a console window")
	}
}

// netstatFixture mixes IPv4, IPv6, other-interface and non-LISTENING rows, plus
// a localized-looking header; the parser must depend on the row shape only.
const netstatFixture = `Active Connections

  Proto  Local Address          Foreign Address        State           PID
  TCP    0.0.0.0:135            0.0.0.0:0              LISTENING       1916
  TCP    0.0.0.0:445            0.0.0.0:0              LISTENING       4
  TCP    127.0.0.1:51789        0.0.0.0:0              LISTENING       6204
  TCP    127.0.0.1:51789        127.0.0.1:52001        ESTABLISHED     6204
  TCP    127.0.0.1:51789        127.0.0.1:52002        TIME_WAIT       0
  TCP    [::]:135               [::]:0                 LISTENING       1916
  TCP    [::]:51789             [::]:0                 LISTENING       6204
  TCPv6  [::]:51789             [::]:0                 LISTENING       6204
  TCP    [::1]:51789            [::1]:52003            ESTABLISHED     6204
  TCP    192.168.1.10:51789     0.0.0.0:0              LISTENING       7000
  TCP    127.0.0.1:51790        0.0.0.0:0              LISTENING       notapid
  TCP    127.0.0.1:51791        0.0.0.0:0              LISTENING
  UDP    0.0.0.0:5353           *:*                                    3480
`

func TestParseTCPListeners(t *testing.T) {
	rows := parseTCPListeners([]byte(netstatFixture))
	got := make([]string, 0, len(rows))
	for _, row := range rows {
		host := row.host
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		got = append(got, fmt.Sprintf("%s:%d/%d", host, row.port, row.pid))
	}
	want := []string{
		"0.0.0.0:135/1916",
		"0.0.0.0:445/4",
		"127.0.0.1:51789/6204",
		"[::]:135/1916",
		"[::]:51789/6204",
		"[::]:51789/6204",
		"192.168.1.10:51789/7000",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("parsed listeners = %v, want %v", got, want)
	}
}

func TestListenerPIDsFiltersAddressStateAndDuplicates(t *testing.T) {
	rows := parseTCPListeners([]byte(netstatFixture))
	if got := listenerPIDs(rows, 51789); !slices.Equal(got, []int{6204}) {
		t.Errorf("listenerPIDs(51789) = %v, want [6204] (loopback + IPv6 wildcard, no other interface, no ESTABLISHED)", got)
	}
	if got := listenerPIDs(rows, 135); !slices.Equal(got, []int{1916}) {
		t.Errorf("listenerPIDs(135) = %v, want [1916]", got)
	}
	if got := listenerPIDs(rows, 51790); len(got) != 0 {
		t.Errorf("listenerPIDs(51790) = %v, want none (unparsable PID)", got)
	}
	if got := listenerPIDs(rows, 51791); len(got) != 0 {
		t.Errorf("listenerPIDs(51791) = %v, want none (missing PID column)", got)
	}
}

// TestListenerPIDsMultipleOwners covers the overlapping-listener case: the same
// port may appear for several PIDs, and every owner is reported, sorted.
func TestListenerPIDsMultipleOwners(t *testing.T) {
	const fixture = `  TCP    0.0.0.0:8080           0.0.0.0:0              LISTENING       222
  TCP    127.0.0.1:8080         0.0.0.0:0              LISTENING       111
  TCP    [::]:8080              [::]:0                 LISTENING       222
  TCP    127.0.0.1:8081         0.0.0.0:0              LISTENING       333
`
	got := listenerPIDs(parseTCPListeners([]byte(fixture)), 8080)
	if !slices.Equal(got, []int{111, 222}) {
		t.Fatalf("listenerPIDs(8080) = %v, want [111 222]", got)
	}
}

// stubNetstat replaces the command runner for one test and counts calls; the
// real netstat is never spawned here.
func stubNetstat(t *testing.T, out func() ([]byte, error)) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	previous := netstatOutput
	netstatOutput = func() ([]byte, error) {
		calls.Add(1)
		return out()
	}
	t.Cleanup(func() { netstatOutput = previous })
	return &calls
}

func TestVerifyPortOwnerPollsUntilChildBinds(t *testing.T) {
	var calls atomic.Int32
	previous := netstatOutput
	netstatOutput = func() ([]byte, error) {
		// The child has a client connection before it owns a listener.
		if calls.Add(1) < 3 {
			return []byte("  TCP    127.0.0.1:51789        127.0.0.1:52001        ESTABLISHED     6204\n"), nil
		}
		return []byte("  TCP    127.0.0.1:51789        0.0.0.0:0              LISTENING       6204\n"), nil
	}
	t.Cleanup(func() { netstatOutput = previous })

	start := time.Now()
	if err := verifyPortOwner(51789, 6204, 3*time.Second); err != nil {
		t.Fatalf("verifyPortOwner: %v", err)
	}
	if got := calls.Load(); got < 3 {
		t.Errorf("netstat polled %d times, want at least 3", got)
	}
	// At least one retry interval must have elapsed; Windows timer granularity
	// makes exact multiples flaky, so only assert one interval.
	if elapsed := time.Since(start); elapsed < portVerifyInterval {
		t.Errorf("returned after %s, want at least one %s retry interval", elapsed, portVerifyInterval)
	}
}

func TestVerifyPortOwnerTimesOutOnUnboundPort(t *testing.T) {
	stubNetstat(t, func() ([]byte, error) {
		return []byte("  TCP    0.0.0.0:135            0.0.0.0:0              LISTENING       1916\n"), nil
	})
	start := time.Now()
	err := verifyPortOwner(51789, 6204, 150*time.Millisecond)
	if err == nil {
		t.Fatal("verifyPortOwner must fail when the child never binds")
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Errorf("timed out after %s, want at least the 150ms budget", elapsed)
	}
	msg := err.Error()
	if !strings.Contains(msg, "port 127.0.0.1:51789 was not bound by the plugin child PID 6204 within 150ms") {
		t.Errorf("error = %q, want the timeout and both PIDs named", msg)
	}
	if !strings.Contains(msg, "(token not sent)") {
		t.Errorf("error = %q, want the no-token guarantee stated", msg)
	}
}

func TestVerifyPortOwnerRejectsForeignOwner(t *testing.T) {
	stubNetstat(t, func() ([]byte, error) {
		return []byte("  TCP    127.0.0.1:51789        0.0.0.0:0              LISTENING       999\n"), nil
	})
	start := time.Now()
	err := verifyPortOwner(51789, 6204, 5*time.Second)
	if err == nil {
		t.Fatal("verifyPortOwner must reject a listener owned by another process")
	}
	want := "plugins: port 127.0.0.1:51789 is owned by PID 999, not the plugin child PID 6204 (token not sent)"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("foreign owner took %s to reject, want a fast failure", elapsed)
	}
}

// TestVerifyPortOwnerAcceptsWildcardListeners is the regression for plugins
// that bind 0.0.0.0 or a dual-stack [::] socket instead of loopback.
func TestVerifyPortOwnerAcceptsWildcardListeners(t *testing.T) {
	cases := []struct{ name, row string }{
		{"loopback", "  TCP    127.0.0.1:51789        0.0.0.0:0              LISTENING       6204\n"},
		{"all-interfaces", "  TCP    0.0.0.0:51789          0.0.0.0:0              LISTENING       6204\n"},
		{"dual-stack", "  TCP    [::]:51789             [::]:0                 LISTENING       6204\n"},
		{"tcpv6-label", "  TCPv6  [::]:51789             [::]:0                 LISTENING       6204\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubNetstat(t, func() ([]byte, error) { return []byte(tc.row), nil })
			if err := verifyPortOwner(51789, 6204, time.Second); err != nil {
				t.Fatalf("verifyPortOwner: %v", err)
			}
		})
	}
}

func TestVerifyPortOwnerReportsNetstatFailure(t *testing.T) {
	stubNetstat(t, func() ([]byte, error) { return nil, errors.New("boom") })
	err := verifyPortOwner(51789, 6204, 100*time.Millisecond)
	if err == nil {
		t.Fatal("verifyPortOwner must report a broken netstat")
	}
	for _, want := range []string{"boom", "(token not sent)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err.Error(), want)
		}
	}
}

func TestVerifyPortOwnerRejectsInvalidArguments(t *testing.T) {
	if err := verifyPortOwner(0, 6204, time.Second); err == nil {
		t.Error("verifyPortOwner must reject port 0")
	}
	if err := verifyPortOwner(51789, 0, time.Second); err == nil {
		t.Error("verifyPortOwner must reject pid 0")
	}
}

func TestIsDescendant(t *testing.T) {
	// 1 -> 10 -> 11, a recycled self-parent 13, and an unknown 99.
	parents := map[uint32]uint32{
		10: 1,
		11: 10,
		12: 0,
		13: 13,
	}
	for _, tc := range []struct {
		name  string
		owner uint32
		want  bool
	}{
		{"direct-child", 10, true},
		{"grandchild", 11, true},
		{"root-is-not-its-own-descendant", 1, false},
		{"orphan", 12, false},
		{"self-parent-loop", 13, false},
		{"unknown-pid", 99, false},
	} {
		if got := isDescendant(tc.owner, 1, parents); got != tc.want {
			t.Errorf("%s: isDescendant(%d) = %v, want %v", tc.name, tc.owner, got, tc.want)
		}
	}
}
