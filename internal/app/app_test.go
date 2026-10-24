package app

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aiworkgate/internal/config"
	"aiworkgate/internal/model"
	"aiworkgate/internal/store"
)

// stubListener satisfies net.Listener for the port-selection tests.
type stubListener struct{}

func (stubListener) Accept() (net.Conn, error) { return nil, errors.New("unused") }
func (stubListener) Close() error              { return nil }
func (stubListener) Addr() net.Addr            { return stubAddr("127.0.0.1:0") }

type stubAddr string

func (a stubAddr) Network() string { return "tcp" }
func (a stubAddr) String() string  { return string(a) }

func TestListenFirstAvailable(t *testing.T) {
	occupied := map[int]bool{7865: true, 7866: true}
	var tried []int
	bind := func(port int) (net.Listener, error) {
		tried = append(tried, port)
		if occupied[port] {
			return nil, errors.New("address already in use")
		}
		return stubListener{}, nil
	}

	ln, port, err := listenFirstAvailable(7865, 11, bind)
	if err != nil {
		t.Fatalf("listenFirstAvailable: %v", err)
	}
	if ln == nil {
		t.Fatal("listener is nil")
	}
	_ = ln.Close()
	if port != 7867 {
		t.Fatalf("port = %d, want 7867", port)
	}
	if len(tried) != 3 {
		t.Fatalf("tried %v, want 3 attempts", tried)
	}

	// Every candidate busy must fail rather than spin.
	_, _, err = listenFirstAvailable(9000, 3, func(int) (net.Listener, error) {
		return nil, errors.New("busy")
	})
	if err == nil {
		t.Fatal("expected an error when every port is busy")
	}
}

func TestTailFileLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "host-2026-10-20.jsonl")
	content := strings.Join([]string{`{"n":1}`, `{"n":2}`, `{"n":3}`}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	lines, err := tailFileLines(path, 2)
	if err != nil {
		t.Fatalf("tailFileLines: %v", err)
	}
	if len(lines) != 2 || lines[0] != `{"n":2}` || lines[1] != `{"n":3}` {
		t.Fatalf("lines = %#v, want the last two entries", lines)
	}

	lines, err = tailFileLines(filepath.Join(dir, "missing.jsonl"), 5)
	if err != nil || len(lines) != 0 {
		t.Fatalf("missing file = %#v, %v; want empty and nil", lines, err)
	}
}

func TestFindClientKey(t *testing.T) {
	keys := []model.ClientKey{
		{ID: "ck_1", Name: "laptop", Enabled: true},
		{ID: "ck_2", Name: "old", Enabled: false},
	}
	key, ok := findClientKey(keys, "ck_1")
	if !ok || key.Name != "laptop" || !key.Enabled {
		t.Fatalf("find ck_1 = %+v, %v", key, ok)
	}
	key, ok = findClientKey(keys, "ck_2")
	if !ok || key.Enabled {
		t.Fatalf("find ck_2 = %+v, %v; want found and disabled", key, ok)
	}
	if _, ok := findClientKey(keys, "ck_missing"); ok {
		t.Fatal("unknown id reported as found")
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	app := &App{dataDir: dir, cfg: config.Default()}

	raw, err := app.getSettings()
	if err != nil {
		t.Fatalf("getSettings: %v", err)
	}
	var initial config.Config
	if err := json.Unmarshal(raw, &initial); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	if initial.Port != config.DefaultPort {
		t.Fatalf("initial port = %d, want %d", initial.Port, config.DefaultPort)
	}

	// Only the changed fields are sent; everything else must be preserved.
	patch := json.RawMessage(`{"port":7999,"logging":{"debug_body":true}}`)
	if err := app.saveSettings(patch); err != nil {
		t.Fatalf("saveSettings: %v", err)
	}

	raw, err = app.getSettings()
	if err != nil {
		t.Fatalf("getSettings after save: %v", err)
	}
	var updated config.Config
	if err := json.Unmarshal(raw, &updated); err != nil {
		t.Fatalf("decode updated settings: %v", err)
	}
	if updated.Port != 7999 || !updated.Logging.DebugBody {
		t.Fatalf("updated settings = %+v", updated)
	}
	if updated.A.BaseURL != config.DefaultABaseURL {
		t.Fatalf("a.base_url = %q, want the previous default", updated.A.BaseURL)
	}

	loaded, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if loaded.Port != 7999 || !loaded.Logging.DebugBody {
		t.Fatalf("persisted settings = %+v", loaded)
	}
}

func TestAdminAuthRequired(t *testing.T) {
	cases := []struct {
		name       string
		listenLAN  bool
		isLoopback bool
		isV1       bool
		want       bool
	}{
		{"loopback host", false, false, false, false},
		{"lan loopback admin", true, true, false, false},
		{"lan remote admin", true, false, false, true},
		{"lan remote v1", true, false, true, false},
		{"lan loopback v1", true, true, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := adminAuthRequired(tc.listenLAN, tc.isLoopback, tc.isV1); got != tc.want {
				t.Fatalf("adminAuthRequired(%v, %v, %v) = %v, want %v",
					tc.listenLAN, tc.isLoopback, tc.isV1, got, tc.want)
			}
		})
	}
}

func TestIsV1PathAndLoopback(t *testing.T) {
	for _, path := range []string{"/v1", "/v1/models", "/v1/chat/completions"} {
		if !isV1Path(path) {
			t.Fatalf("%q is not recognized as a v1 path", path)
		}
	}
	for _, path := range []string{"/", "/api/status", "/v10/models"} {
		if isV1Path(path) {
			t.Fatalf("%q wrongly recognized as a v1 path", path)
		}
	}
	for _, remote := range []string{"127.0.0.1:1234", "[::1]:5000", "localhost:80"} {
		if !isLoopbackRemote(remote) {
			t.Fatalf("%q should be loopback", remote)
		}
	}
	for _, remote := range []string{"192.168.1.5:1234", "", "garbage"} {
		if isLoopbackRemote(remote) {
			t.Fatalf("%q should not be loopback", remote)
		}
	}
}

func TestParseTimeQuery(t *testing.T) {
	if got, ok := parseTimeQuery("1760000000", false); !ok || got != 1760000000 {
		t.Fatalf("unix = %d, %v", got, ok)
	}
	from, ok := parseTimeQuery("2026-10-20", false)
	if !ok {
		t.Fatal("date from was rejected")
	}
	to, ok := parseTimeQuery("2026-10-20", true)
	if !ok {
		t.Fatal("date to was rejected")
	}
	if to-from != 24*60*60-1 {
		t.Fatalf("date window = %d seconds, want one day minus one second", to-from)
	}
	if _, ok := parseTimeQuery("not-a-date", false); ok {
		t.Fatal("garbage was accepted")
	}
}

func TestAccountEntryEgress(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	app := &App{store: st}

	if err := st.SetAccountEgress(model.AccountEgress{
		PluginID: "mock", AccountID: "acc_1", Mode: model.EgressProxy, ProxyID: "px_1",
	}); err != nil {
		t.Fatalf("SetAccountEgress: %v", err)
	}

	accounts := []model.Account{{ID: "acc_1", Label: "one"}, {ID: "acc_2", Label: "two"}}
	entries := tagAccounts("mock", accounts, app.accountEgress("mock"))
	if len(entries) != 2 {
		t.Fatalf("entries = %+v, want two", entries)
	}
	if entries[0].PluginID != "mock" || entries[0].Label != "one" {
		t.Fatalf("entry 0 = %+v", entries[0])
	}
	if entries[0].Egress == nil {
		t.Fatal("assigned account carries no egress")
	}
	if got := entries[0].Egress; got.Mode != model.EgressProxy || got.ProxyID != "px_1" || got.AccountID != "acc_1" {
		t.Fatalf("egress = %+v", got)
	}
	if entries[1].Egress != nil {
		t.Fatalf("unassigned account carries egress: %+v", entries[1].Egress)
	}

	// The assignment must reach the admin JSON with snake_case keys, and the
	// unassigned account must not grow an "egress" field at all.
	raw, err := json.Marshal(entries[0])
	if err != nil {
		t.Fatalf("marshal entry: %v", err)
	}
	if !strings.Contains(string(raw), `"plugin_id":"mock"`) ||
		!strings.Contains(string(raw), `"egress":{`) ||
		!strings.Contains(string(raw), `"proxy_id":"px_1"`) {
		t.Fatalf("entry JSON = %s", raw)
	}
	raw, err = json.Marshal(entries[1])
	if err != nil {
		t.Fatalf("marshal unassigned entry: %v", err)
	}
	if strings.Contains(string(raw), "egress") {
		t.Fatalf("unassigned entry JSON = %s, want no egress field", raw)
	}

	// A host without a store simply reports no assignments.
	if got := (&App{}).accountEgress("mock"); len(got) != 0 {
		t.Fatalf("nil store egress = %+v, want empty", got)
	}
	// Deleting the assignment restores the plugin default.
	if err := st.DeleteAccountEgress("mock", "acc_1"); err != nil {
		t.Fatalf("DeleteAccountEgress: %v", err)
	}
	if got := tagAccounts("mock", accounts, app.accountEgress("mock")); got[0].Egress != nil {
		t.Fatalf("egress survived deletion: %+v", got[0].Egress)
	}
}

func TestReadWriteGatewayPort(t *testing.T) {
	dir := t.TempDir()
	if port := readGatewayPort(dir); port != 0 {
		t.Fatalf("missing port file = %d, want 0", port)
	}
	if err := writeGatewayPort(dir, 7871); err != nil {
		t.Fatalf("writeGatewayPort: %v", err)
	}
	if port := readGatewayPort(dir); port != 7871 {
		t.Fatalf("port = %d, want 7871", port)
	}
	path := gatewayPortPath(dir)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("port file missing: %v", err)
	}
	if err := os.WriteFile(path, []byte("bad"), 0o600); err != nil {
		t.Fatalf("overwrite port file: %v", err)
	}
	if port := readGatewayPort(dir); port != 0 {
		t.Fatalf("invalid port file = %d, want 0", port)
	}
}

func TestRetryDelaysFromConfig(t *testing.T) {
	if got := retryDelaysFromConfig(0, []int{5, 20}); len(got) != 0 || got == nil {
		t.Fatalf("count 0 = %v, want empty non-nil", got)
	}
	got := retryDelaysFromConfig(2, []int{5, 20})
	if len(got) != 2 || got[0].Seconds() != 5 || got[1].Seconds() != 20 {
		t.Fatalf("count 2 = %v, want [5s 20s]", got)
	}
	got = retryDelaysFromConfig(4, []int{1, 2})
	if len(got) != 4 || got[2].Seconds() != 2 || got[3].Seconds() != 2 {
		t.Fatalf("count 4 with two backoffs = %v, want last value repeated", got)
	}
}

func TestAssignedAccountIDs(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	app := &App{store: st}

	if got := app.assignedAccountIDs("mock"); len(got) != 0 {
		t.Fatalf("assignedAccountIDs without rows = %v, want empty", got)
	}
	for _, id := range []string{"acc_b", "acc_a"} {
		if err := st.SetAccountEgress(model.AccountEgress{
			PluginID: "mock", AccountID: id, Mode: model.EgressDirect,
		}); err != nil {
			t.Fatal(err)
		}
	}
	got := app.assignedAccountIDs("mock")
	if len(got) != 2 || got[0] != "acc_a" || got[1] != "acc_b" {
		t.Fatalf("assignedAccountIDs = %v, want [acc_a acc_b]", got)
	}
	if got := app.assignedAccountIDs("other"); len(got) != 0 {
		t.Fatalf("other plugin ids = %v, want empty", got)
	}
}
