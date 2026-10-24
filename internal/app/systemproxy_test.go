package app

import (
	"errors"
	"testing"

	"aiworkgate/internal/egress"
	"aiworkgate/internal/hosts"
	"aiworkgate/internal/logging"
	"aiworkgate/internal/model"
	"aiworkgate/internal/store"
)

func newSystemProxyTestApp(t *testing.T) (*App, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	logger, err := logging.NewLogger(dir)
	if err != nil {
		t.Fatalf("logging.NewLogger: %v", err)
	}
	t.Cleanup(func() { _ = logger.Close() })
	em := egress.NewManager(st)
	t.Cleanup(func() { _ = em.CloseAll() })
	return &App{dataDir: dir, store: st, egressMgr: em, logger: logger}, st
}

func TestSyncSystemProxyFollowsWindows(t *testing.T) {
	app, st := newSystemProxyTestApp(t)
	read := hosts.SystemProxy{Configured: true, Type: "http", Host: "127.0.0.1", Port: 7897}
	app.systemProxyRead = func() (hosts.SystemProxy, error) { return read, nil }

	if !app.syncSystemProxy() {
		t.Fatal("first sync reported no change; the managed row did not exist yet")
	}
	row, _, err := st.GetProxy(model.SystemProxyID)
	if err != nil {
		t.Fatalf("GetProxy: %v", err)
	}
	if !row.Enabled || row.Host != "127.0.0.1" || row.Port != 7897 || row.Type != "http" {
		t.Fatalf("row = %+v, want enabled http 127.0.0.1:7897", row)
	}
	if row.Name != model.SystemProxyName {
		t.Fatalf("row name = %q, want %q", row.Name, model.SystemProxyName)
	}

	// A poll that sees the same settings must not rewrite the row: upserts reset
	// the health fields, and a rewrite every 30s would also churn plugin reloads.
	if app.syncSystemProxy() {
		t.Fatal("unchanged settings reported a change")
	}

	// Health written by a manual check survives an endpoint change.
	if err := st.UpdateProxyHealth(model.SystemProxyID, 100, "203.0.113.7", 42, ""); err != nil {
		t.Fatalf("UpdateProxyHealth: %v", err)
	}
	read.Port = 7898
	if !app.syncSystemProxy() {
		t.Fatal("endpoint change reported no change")
	}
	row, _, err = st.GetProxy(model.SystemProxyID)
	if err != nil {
		t.Fatalf("GetProxy: %v", err)
	}
	if row.Port != 7898 || row.LastExitIP != "203.0.113.7" || row.LastLatencyMS != 42 || row.LastCheckAt != 100 {
		t.Fatalf("row after endpoint change = %+v, want port 7898 with health preserved", row)
	}
}

func TestSyncSystemProxyDisablesWhenWindowsSwitchIsOff(t *testing.T) {
	app, st := newSystemProxyTestApp(t)
	on := true
	app.systemProxyRead = func() (hosts.SystemProxy, error) {
		if !on {
			return hosts.SystemProxy{}, nil
		}
		return hosts.SystemProxy{Configured: true, Type: "socks5", Host: "10.0.0.2", Port: 1080}, nil
	}
	if !app.syncSystemProxy() {
		t.Fatal("initial sync reported no change")
	}

	on = false
	if !app.syncSystemProxy() {
		t.Fatal("switching the Windows proxy off reported no change")
	}
	row, _, err := st.GetProxy(model.SystemProxyID)
	if err != nil {
		t.Fatalf("GetProxy: %v", err)
	}
	if row.Enabled {
		t.Fatalf("row is still enabled with the Windows proxy off: %+v", row)
	}
	if note := app.systemProxyNote(); note == "" {
		t.Fatal("no reason recorded for the disabled managed row")
	}

	on = true
	if !app.syncSystemProxy() {
		t.Fatal("switching the Windows proxy back on reported no change")
	}
	row, _, err = st.GetProxy(model.SystemProxyID)
	if err != nil {
		t.Fatalf("GetProxy: %v", err)
	}
	if !row.Enabled || row.Type != "socks5" || row.Port != 1080 {
		t.Fatalf("row after re-enable = %+v, want enabled socks5 10.0.0.2:1080", row)
	}
}

func TestSyncSystemProxyReportsPACAndReadFailures(t *testing.T) {
	app, st := newSystemProxyTestApp(t)
	app.systemProxyRead = func() (hosts.SystemProxy, error) {
		return hosts.SystemProxy{}, hosts.ErrSystemProxyPAC
	}
	if !app.syncSystemProxy() {
		t.Fatal("PAC setup reported no change on the first sync")
	}
	row, _, err := st.GetProxy(model.SystemProxyID)
	if err != nil {
		t.Fatalf("GetProxy: %v", err)
	}
	if row.Enabled {
		t.Fatalf("PAC-backed row is enabled: %+v", row)
	}
	if note := app.systemProxyNote(); note == "" {
		t.Fatal("PAC setup has no explanatory note")
	}

	app.systemProxyRead = func() (hosts.SystemProxy, error) {
		return hosts.SystemProxy{}, errors.New("registry unavailable")
	}
	app.syncSystemProxy()
	if note := app.systemProxyNote(); note == "" {
		t.Fatal("registry read failure has no explanatory note")
	}
}

func TestSaveAndDeleteProxyRejectTheManagedRow(t *testing.T) {
	app, _ := newSystemProxyTestApp(t)
	if err := app.saveProxy(model.Proxy{ID: model.SystemProxyID, Name: "x", Type: "http", Host: "127.0.0.1", Port: 1}); !errors.Is(err, errSystemProxyManaged) {
		t.Fatalf("saveProxy(system) = %v, want errSystemProxyManaged", err)
	}
	if err := app.deleteProxy(model.SystemProxyID); !errors.Is(err, errSystemProxyManaged) {
		t.Fatalf("deleteProxy(system) = %v, want errSystemProxyManaged", err)
	}
}
