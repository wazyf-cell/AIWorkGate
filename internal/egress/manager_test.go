package egress

import (
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"aiworkgate/internal/model"
)

type fakeStore struct {
	mu          sync.Mutex
	proxies     []model.Proxy
	assignments []model.AccountEgress
}

func (s *fakeStore) ListProxies() ([]model.Proxy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]model.Proxy, len(s.proxies))
	copy(out, s.proxies)
	return out, nil
}

func (s *fakeStore) ListAccountEgress(pluginID string) ([]model.AccountEgress, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []model.AccountEgress
	for _, a := range s.assignments {
		if a.PluginID == pluginID {
			out = append(out, a)
		}
	}
	return out, nil
}

func (s *fakeStore) setProxies(proxies []model.Proxy) {
	s.mu.Lock()
	s.proxies = proxies
	s.mu.Unlock()
}

func TestManagerLifecycle(t *testing.T) {
	echo := newEchoServer(t)
	upstreamA := newFakeProxy(t, "")
	upstreamB := newFakeProxy(t, "")

	store := &fakeStore{
		proxies: []model.Proxy{
			{ID: "px_a", Name: "A", Type: "http", Host: "127.0.0.1", Port: upstreamA.Port(), Enabled: true},
			{ID: "px_off", Name: "off", Type: "http", Host: "127.0.0.1", Port: upstreamB.Port(), Enabled: false},
		},
		assignments: []model.AccountEgress{
			{PluginID: "plug", AccountID: "acc_1", Mode: model.EgressProxy, ProxyID: "px_a"},
			{PluginID: "other", AccountID: "acc_1", Mode: model.EgressProxy, ProxyID: "px_off"},
		},
	}
	m := NewManager(store)
	t.Cleanup(func() { _ = m.CloseAll() })

	if err := m.StartEnabled(); err != nil {
		t.Fatalf("StartEnabled: %v", err)
	}
	// StartEnabled only records configs: a proxy with no assigned account must
	// not hold a loopback port.
	if _, ok := m.LocalURL("px_a"); ok {
		t.Fatal("listener bound before any account asked for one")
	}
	urlA, ok := m.LocalURLFor("plug", "acc_1", "px_a")
	if !ok || urlA == "" {
		t.Fatalf("px_a forwarder not running (url %q, ok %v)", urlA, ok)
	}
	if _, ok := m.LocalURLFor("plug", "acc_1", "px_off"); ok {
		t.Fatal("disabled proxy must not get a listener")
	}
	if _, ok := m.LocalURLFor("plug", "acc_1", "px_missing"); ok {
		t.Fatal("unknown proxy must not get a listener")
	}
	if err := m.StartProxy(store.proxies[1]); !errors.Is(err, ErrProxyDisabled) {
		t.Fatalf("StartProxy(disabled) = %v, want ErrProxyDisabled", err)
	}

	// Egress file resolution against running forwarders.
	file, err := m.EgressFile("plug", true, model.EgressDirect, []string{"acc_1", "acc_2"})
	if err != nil {
		t.Fatalf("EgressFile: %v", err)
	}
	if file.Default.Mode != model.EgressBlocked {
		t.Fatalf("strict default = %q, want blocked", file.Default.Mode)
	}
	if got := file.Accounts["acc_1"]; got.Mode != model.EgressProxy || got.URL != urlA || got.ProxyID != "px_a" {
		t.Fatalf("acc_1 entry = %+v, want proxy url %q", got, urlA)
	}
	if got := file.Accounts["acc_2"]; got.Mode != model.EgressBlocked {
		t.Fatalf("acc_2 entry = %+v, want blocked", got)
	}

	// Updating a proxy restarts its listeners: old ones close, new ones serve
	// traffic, credentials/endpoint changes take effect.
	oldAddr, _ := proxyURLParts(t, urlA)
	updated := model.Proxy{ID: "px_a", Name: "A2", Type: "http", Host: "127.0.0.1", Port: upstreamB.Port(), Enabled: true}
	if err := m.StartProxy(updated); err != nil {
		t.Fatalf("restart px_a: %v", err)
	}
	newURL, ok := m.LocalURL("px_a")
	if !ok {
		t.Fatal("px_a not running after restart")
	}
	if c, err := net.DialTimeout("tcp", oldAddr, time.Second); err == nil {
		_ = c.Close()
		t.Fatal("old forwarder listener still open after restart")
	}

	resp, err := newTunnelClient(t, newURL).Get(echo.URL() + "/after-restart")
	if err != nil {
		t.Fatalf("GET after restart: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if got := upstreamB.Targets(); len(got) != 1 || got[0] != echo.Addr() {
		t.Fatalf("upstream B targets = %v, want [%s]", got, echo.Addr())
	}
	if got := upstreamA.Targets(); len(got) != 0 {
		t.Fatalf("upstream A received traffic after restart: %v", got)
	}

	if err := m.StopProxy("px_a"); err != nil {
		t.Fatalf("StopProxy: %v", err)
	}
	if _, ok := m.LocalURL("px_a"); ok {
		t.Fatal("px_a still reported after StopProxy")
	}
	if err := m.StopProxy("px_a"); err != nil {
		t.Fatalf("StopProxy twice: %v", err)
	}

	if err := m.CloseAll(); err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
	if len(m.RunningIDs()) != 0 {
		t.Fatalf("RunningIDs after CloseAll = %v", m.RunningIDs())
	}
	if err := m.StartProxy(updated); !errors.Is(err, ErrManagerClosed) {
		t.Fatalf("StartProxy after CloseAll = %v, want ErrManagerClosed", err)
	}
}

func TestManagerStartEnabledStopsRemovedProxies(t *testing.T) {
	upstream := newFakeProxy(t, "")
	proxy := model.Proxy{ID: "px_sync", Type: "http", Host: "127.0.0.1", Port: upstream.Port(), Enabled: true}
	store := &fakeStore{proxies: []model.Proxy{proxy}}

	m := NewManager(store)
	t.Cleanup(func() { _ = m.CloseAll() })

	if err := m.StartEnabled(); err != nil {
		t.Fatalf("StartEnabled: %v", err)
	}
	url, ok := m.LocalURLFor("plug", "acc_1", "px_sync")
	if !ok {
		t.Fatal("px_sync not running")
	}

	store.setProxies([]model.Proxy{{ID: "px_sync", Type: "http", Host: "127.0.0.1", Port: upstream.Port(), Enabled: false}})
	if err := m.StartEnabled(); err != nil {
		t.Fatalf("StartEnabled after disable: %v", err)
	}
	if _, ok := m.LocalURL("px_sync"); ok {
		t.Fatal("disabled proxy forwarder was not stopped by StartEnabled")
	}
	urlAddr, _ := proxyURLParts(t, url)
	if c, err := net.DialTimeout("tcp", urlAddr, time.Second); err == nil {
		_ = c.Close()
		t.Fatal("disabled proxy listener still open")
	}

	// Re-enabling brings it back on the next account request.
	store.setProxies([]model.Proxy{proxy})
	if err := m.StartEnabled(); err != nil {
		t.Fatalf("StartEnabled after enable: %v", err)
	}
	if _, ok := m.LocalURLFor("plug", "acc_1", "px_sync"); !ok {
		t.Fatal("re-enabled proxy not running")
	}
}

func TestManagerStartProxyRejectsEmptyID(t *testing.T) {
	m := NewManager(&fakeStore{})
	t.Cleanup(func() { _ = m.CloseAll() })
	if err := m.StartProxy(model.Proxy{Type: "http", Host: "127.0.0.1", Port: 1080, Enabled: true}); err == nil {
		t.Fatal("expected empty id error")
	}
}
