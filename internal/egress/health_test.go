package egress

import (
	"fmt"
	"testing"
	"time"

	"aiworkgate/internal/model"
	"aiworkgate/internal/store"
)

type fakeHealthSource struct {
	assignments []model.AccountEgress
	proxies     map[string]model.Proxy
	logs        map[string]*model.ProxyLog
}

func (f *fakeHealthSource) ListAccountEgress(pluginID string) ([]model.AccountEgress, error) {
	return f.assignments, nil
}

func (f *fakeHealthSource) GetProxy(id string) (model.Proxy, string, error) {
	p, ok := f.proxies[id]
	if !ok {
		return model.Proxy{}, "", fmt.Errorf("store: get proxy %q: %w", id, store.ErrNotFound)
	}
	return p, "", nil
}

func (f *fakeHealthSource) LatestProxyLog(pluginID, accountID string, since int64) (*model.ProxyLog, error) {
	row := f.logs[pluginID+"\x00"+accountID]
	if row == nil || row.At < since {
		return nil, nil
	}
	return row, nil
}

const healthNow = int64(1_700_000_000)

func healthTestNow() func() time.Time {
	return func() time.Time { return time.Unix(healthNow, 0) }
}

func proxyAssignment(accountID, proxyID string) model.AccountEgress {
	return model.AccountEgress{PluginID: "p", AccountID: accountID, Mode: model.EgressProxy, ProxyID: proxyID}
}

// TestLookupRecentFailureMakesDead is the core of the P1: a tunnel failure
// inside the window marks the account's exit dead.
func TestLookupRecentFailureMakesDead(t *testing.T) {
	src := &fakeHealthSource{
		assignments: []model.AccountEgress{proxyAssignment("a", "px")},
		proxies:     map[string]model.Proxy{"px": {ID: "px", Name: "公司代理", Enabled: true}},
		logs: map[string]*model.ProxyLog{
			"p\x00a": {At: healthNow - 60, Result: model.ProxyResultFailed, Error: "dial http://u:p@host:1080: timeout"},
		},
	}
	h := NewHealth(src)
	h.SetNow(healthTestNow())
	ev := h.Lookup("p", "a")
	if !ev.Dead() || ev.ProxyLabel != "公司代理" {
		t.Fatalf("evidence = %+v, want dead with label", ev)
	}
	// The userinfo must not survive into the admin API.
	if want := "dial http://***@host:1080: timeout"; ev.Error != want {
		t.Fatalf("error = %q, want %q", ev.Error, want)
	}
}

// TestLookupRecoversOnNewerOK: an ok tunnel row after the failure is the proof
// that the proxy works again; no manual "clear" is needed.
func TestLookupRecoversOnNewerOK(t *testing.T) {
	src := &fakeHealthSource{
		assignments: []model.AccountEgress{proxyAssignment("a", "px")},
		proxies:     map[string]model.Proxy{"px": {ID: "px", Name: "px", Enabled: true}},
		logs: map[string]*model.ProxyLog{
			"p\x00a": {At: healthNow - 10, Result: model.ProxyResultOK},
		},
	}
	h := NewHealth(src)
	h.SetNow(healthTestNow())
	if ev := h.Lookup("p", "a"); ev.Status != HealthOK {
		t.Fatalf("evidence = %+v, want ok", ev)
	}
}

// TestLookupIgnoresStaleFailure: evidence outside the window is not current, so
// a forgotten failure cannot penalise an account forever.
func TestLookupIgnoresStaleFailure(t *testing.T) {
	src := &fakeHealthSource{
		assignments: []model.AccountEgress{proxyAssignment("a", "px")},
		proxies:     map[string]model.Proxy{"px": {ID: "px", Name: "px", Enabled: true}},
		logs: map[string]*model.ProxyLog{
			"p\x00a": {At: healthNow - int64(EvidenceWindow/time.Second) - 60, Result: model.ProxyResultFailed, Error: "old"},
		},
	}
	h := NewHealth(src)
	h.SetNow(healthTestNow())
	if ev := h.Lookup("p", "a"); ev.Status != HealthUnknown {
		t.Fatalf("evidence = %+v, want unknown", ev)
	}
}

// TestLookupManualCheckWinsWhenNewer: the proxies page "检测" writes
// last_check_at; a successful check after the failure clears it.
func TestLookupManualCheckWinsWhenNewer(t *testing.T) {
	src := &fakeHealthSource{
		assignments: []model.AccountEgress{proxyAssignment("a", "px")},
		proxies: map[string]model.Proxy{
			"px": {ID: "px", Name: "px", Enabled: true, LastCheckAt: healthNow - 5, LastExitIP: "1.2.3.4"},
		},
		logs: map[string]*model.ProxyLog{
			"p\x00a": {At: healthNow - 60, Result: model.ProxyResultFailed, Error: "timeout"},
		},
	}
	h := NewHealth(src)
	h.SetNow(healthTestNow())
	if ev := h.Lookup("p", "a"); ev.Status != HealthOK || ev.At != healthNow-5 {
		t.Fatalf("evidence = %+v, want ok from the manual check", ev)
	}
}

// TestLookupUnresolvableOrDisabledProxyIsDead: a deleted or disabled proxy
// cannot carry the account's traffic, so the exit is not healthy.
func TestLookupUnresolvableOrDisabledProxyIsDead(t *testing.T) {
	missing := &fakeHealthSource{
		assignments: []model.AccountEgress{proxyAssignment("a", "gone")},
		proxies:     map[string]model.Proxy{},
	}
	disabled := &fakeHealthSource{
		assignments: []model.AccountEgress{proxyAssignment("a", "px")},
		proxies:     map[string]model.Proxy{"px": {ID: "px", Name: "px", Enabled: false}},
	}
	for name, src := range map[string]*fakeHealthSource{"missing": missing, "disabled": disabled} {
		h := NewHealth(src)
		h.SetNow(healthTestNow())
		ev := h.Lookup("p", "a")
		if !ev.Dead() || ev.Error == "" {
			t.Fatalf("%s: evidence = %+v, want dead with a reason", name, ev)
		}
	}
}

// TestLookupDirectAndUnassignedAccountsHaveNoVerdict: only explicit proxy
// assignments are evidenced.
func TestLookupDirectAndUnassignedAccountsHaveNoVerdict(t *testing.T) {
	src := &fakeHealthSource{
		assignments: []model.AccountEgress{
			{PluginID: "p", AccountID: "a", Mode: model.EgressDirect},
		},
		proxies: map[string]model.Proxy{},
	}
	h := NewHealth(src)
	h.SetNow(healthTestNow())
	if ev := h.Lookup("p", "a"); ev.Mode != model.EgressDirect || ev.Dead() {
		t.Fatalf("direct evidence = %+v", ev)
	}
	if ev := h.Lookup("p", "other"); ev.Mode != "" || ev.Dead() {
		t.Fatalf("unassigned evidence = %+v", ev)
	}
}

// TestLookupCachesUntilInvalidated: the scheduler calls this under its lock, so
// lookups must not re-read the store inside the TTL.
func TestLookupCachesUntilInvalidated(t *testing.T) {
	src := &fakeHealthSource{
		assignments: []model.AccountEgress{proxyAssignment("a", "px")},
		proxies:     map[string]model.Proxy{"px": {ID: "px", Name: "px", Enabled: true}},
		logs: map[string]*model.ProxyLog{
			"p\x00a": {At: healthNow - 10, Result: model.ProxyResultFailed, Error: "timeout"},
		},
	}
	h := NewHealth(src)
	h.SetNow(healthTestNow())
	if !h.Lookup("p", "a").Dead() {
		t.Fatal("want dead")
	}
	src.logs["p\x00a"] = &model.ProxyLog{At: healthNow - 1, Result: model.ProxyResultOK}
	if !h.Lookup("p", "a").Dead() {
		t.Fatal("cached verdict must survive inside the TTL")
	}
	h.Invalidate("p", "a")
	if h.Lookup("p", "a").Dead() {
		t.Fatal("invalidated verdict must be re-read")
	}
}
