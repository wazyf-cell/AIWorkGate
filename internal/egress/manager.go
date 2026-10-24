package egress

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"aiworkgate/internal/model"
)

var (
	// ErrProxyDisabled is returned when starting a proxy the user disabled.
	ErrProxyDisabled = errors.New("egress: proxy is disabled")
	// ErrManagerClosed is returned when starting a proxy after CloseAll.
	ErrManagerClosed = errors.New("egress: manager is closed")
)

// Store is the subset of the host store the egress manager needs. It is an
// interface so the egress package stays independent of the SQLite store.
type Store interface {
	ListProxies() ([]model.Proxy, error)
	ListAccountEgress(pluginID string) ([]model.AccountEgress, error)
}

// Manager owns one Forwarder per (plugin, account, proxy) triple and serves the
// local URLs that end up in a plugin's egress.json.
//
// Per-account listeners cost one loopback port each, and they are the only way
// the host can attribute a tunnel: a listener shared by several accounts sees
// the target but not who dialled it.
//
// The proxy slot "direct" (see DirectProxyID) is the synthetic entry of an
// account with no upstream proxy. Its listeners exist only while direct logging
// is enabled: binding one loopback port per direct account is a real cost, and
// until something actually dials through it, it buys nothing.
type Manager struct {
	store Store

	mu            sync.Mutex
	proxies       map[string]model.Proxy
	forwarders    map[forwardKey]*Forwarder
	sink          LogSink
	closed        bool
	directLogging bool
}

// forwardKey identifies one local listener.
type forwardKey struct {
	proxy   string
	plugin  string
	account string
}

// NewManager creates a manager. Proxies are not started implicitly; call
// StartEnabled at boot or StartProxy for individual (re)starts.
func NewManager(store Store) *Manager {
	return &Manager{
		store:      store,
		proxies:    make(map[string]model.Proxy),
		forwarders: make(map[forwardKey]*Forwarder),
	}
}

// SetLogSink installs the proxy traffic sink used by every listener this
// manager creates, including listeners that already exist.
func (m *Manager) SetLogSink(sink LogSink) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sink = sink
}

// SetDirectLogging turns the synthetic "direct" listeners on or off. Off is the
// default: an account in direct mode then keeps dialling by itself, exactly as
// before this feature existed. Turning it on makes LocalURLFor accept
// DirectProxyID, so egress.json and the plugin environment can point direct
// traffic at a listener that records it. Turning it off stops the listeners it
// had created; nothing dials through them afterwards. A listener that refuses to
// close is still forgotten, so the worst case is a leaked loopback port until
// the host restarts; that is reported here rather than swallowed.
func (m *Manager) SetDirectLogging(enabled bool) error {
	m.mu.Lock()
	if m.directLogging == enabled {
		m.mu.Unlock()
		return nil
	}
	m.directLogging = enabled
	var stopped []*Forwarder
	if !enabled {
		for key, f := range m.forwarders {
			if !IsDirectProxyID(key.proxy) {
				continue
			}
			delete(m.forwarders, key)
			stopped = append(stopped, f)
		}
	}
	m.mu.Unlock()

	var errs []error
	for _, f := range stopped {
		if err := f.Stop(); err != nil {
			errs = append(errs, fmt.Errorf("egress: stop direct listener: %w", err))
		}
	}
	return errors.Join(errs...)
}

// DirectLoggingEnabled reports whether "direct" listeners may be created.
func (m *Manager) DirectLoggingEnabled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.directLogging
}

// DirectURLFor is LocalURLFor for an account with no upstream proxy: it returns
// the loopback listener that records the account's direct traffic, or false
// when direct logging is off. Passing "" as accountID asks for the listener that
// backs the plugin's default (not account-scoped) egress.
func (m *Manager) DirectURLFor(pluginID, accountID string) (string, bool) {
	return m.LocalURLFor(pluginID, accountID, DirectProxyID)
}

// StartEnabled records the enabled proxies and stops listeners whose proxy was
// disabled or removed. Listeners are created on demand by LocalURLFor (when a
// plugin's egress.json is written), so a proxy with no assigned account never
// binds a port. It is safe to call at boot and after a proxy list change.
func (m *Manager) StartEnabled() error {
	if m.store == nil {
		return errors.New("egress: manager has no store")
	}
	proxies, err := m.store.ListProxies()
	if err != nil {
		return fmt.Errorf("egress: list proxies: %w", err)
	}

	wanted := make(map[string]model.Proxy)
	for _, p := range proxies {
		if p.Enabled {
			wanted[p.ID] = p
		}
	}

	m.mu.Lock()
	m.proxies = wanted
	var stale []*Forwarder
	for key, f := range m.forwarders {
		// The synthetic "direct" slot is not a row in the proxies table, so it
		// is never listed as wanted and must not be swept as stale.
		if key.proxy == DirectProxyID {
			continue
		}
		if _, ok := wanted[key.proxy]; !ok {
			delete(m.forwarders, key)
			stale = append(stale, f)
		}
	}
	m.mu.Unlock()

	var errs []error
	for _, f := range stale {
		if err := f.Stop(); err != nil {
			errs = append(errs, fmt.Errorf("egress: stop stale forwarder: %w", err))
		}
	}
	return errors.Join(errs...)
}

// StartProxy records (or replaces) the upstream config of p and restarts every
// listener bound to it, so credential or endpoint changes take effect
// immediately. Disabled proxies are rejected.
func (m *Manager) StartProxy(p model.Proxy) error {
	id := strings.TrimSpace(p.ID)
	if id == "" {
		return errors.New("egress: proxy id is empty")
	}
	if IsDirectProxyID(id) {
		// "direct" is the reserved slot of the direct listeners; a real proxy
		// answering to it would take over their identity in proxy_logs.
		return fmt.Errorf("egress: proxy id %q is reserved for direct egress", DirectProxyID)
	}
	if !p.Enabled {
		return fmt.Errorf("%w: %s", ErrProxyDisabled, id)
	}

	type live struct {
		key forwardKey
		id  Identity
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrManagerClosed
	}
	m.proxies[id] = p
	var existing []live
	var old []*Forwarder
	for key, f := range m.forwarders {
		if key.proxy != id {
			continue
		}
		existing = append(existing, live{key: key, id: Identity{PluginID: key.plugin, AccountID: key.account}})
		old = append(old, f)
		delete(m.forwarders, key)
	}
	m.mu.Unlock()

	var errs []error
	for _, f := range old {
		if err := f.Stop(); err != nil {
			errs = append(errs, fmt.Errorf("egress: stop previous forwarder for %s: %w", id, err))
		}
	}
	for _, item := range existing {
		if _, err := m.startLocked(item.key, item.id, p); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// StopProxy stops and forgets every listener bound to id. Unknown IDs are
// ignored.
func (m *Manager) StopProxy(id string) error {
	m.mu.Lock()
	delete(m.proxies, id)
	var stopped []*Forwarder
	for key, f := range m.forwarders {
		if key.proxy == id {
			delete(m.forwarders, key)
			stopped = append(stopped, f)
		}
	}
	m.mu.Unlock()

	var errs []error
	for _, f := range stopped {
		if err := f.Stop(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// LocalURLFor returns the loopback URL of the listener serving one
// (plugin, account, proxy) triple, creating it on first use. The boolean is
// false when the proxy is unknown or disabled, when the manager is closed, or
// when proxyID is the synthetic DirectProxyID and direct logging is off.
func (m *Manager) LocalURLFor(pluginID, accountID, proxyID string) (string, bool) {
	key := forwardKey{
		proxy:   strings.TrimSpace(proxyID),
		plugin:  strings.TrimSpace(pluginID),
		account: strings.TrimSpace(accountID),
	}
	if key.proxy == "" {
		return "", false
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return "", false
	}
	if f, ok := m.forwarders[key]; ok {
		u := f.LocalURL()
		m.mu.Unlock()
		return u, u != ""
	}
	// The "direct" slot has no row in the proxies table: its configuration is
	// the synthetic DirectProxy, and it only exists while direct logging is on.
	direct := IsDirectProxyID(key.proxy)
	directLogging := m.directLogging
	p, ok := m.proxies[key.proxy]
	m.mu.Unlock()

	switch {
	case direct && !directLogging:
		return "", false
	case direct:
		key.proxy = DirectProxyID
		p = DirectProxy()
	case !ok || !p.Enabled:
		return "", false
	}

	u, err := m.startLocked(key, Identity{PluginID: key.plugin, AccountID: key.account}, p)
	if err != nil {
		return "", false
	}
	return u, true
}

// startLocked binds one listener and records it. The manager lock is not held
// while binding: Start takes the forwarder's own lock and binding under the
// manager lock would serialise every account's first egress write.
func (m *Manager) startLocked(key forwardKey, id Identity, p model.Proxy) (string, error) {
	m.mu.Lock()
	sink := m.sink
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return "", ErrManagerClosed
	}
	fw := &Forwarder{}
	u, err := fw.StartFor(p, id, sink)
	if err != nil {
		return "", fmt.Errorf("egress: start proxy %s: %w", key.proxy, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		_ = fw.Stop()
		return "", ErrManagerClosed
	}
	// A concurrent caller may have created the same listener; keep theirs.
	if existing, ok := m.forwarders[key]; ok {
		if eu := existing.LocalURL(); eu != "" {
			_ = fw.Stop()
			return eu, nil
		}
	}
	m.forwarders[key] = fw
	return u, nil
}

// LocalURL returns the loopback URL of any running listener for proxyID. New
// code should use LocalURLFor so the traffic can be attributed to an account.
func (m *Manager) LocalURL(proxyID string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, f := range m.forwarders {
		if key.proxy != proxyID {
			continue
		}
		if u := f.LocalURL(); u != "" {
			return u, true
		}
	}
	return "", false
}

// RunningIDs lists proxy IDs that currently have at least one listener.
func (m *Manager) RunningIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := make(map[string]bool, len(m.forwarders))
	ids := make([]string, 0, len(m.forwarders))
	for key, f := range m.forwarders {
		if seen[key.proxy] || f.LocalURL() == "" {
			continue
		}
		seen[key.proxy] = true
		ids = append(ids, key.proxy)
	}
	return ids
}

// CloseAll stops every forwarder and rejects further starts. It is idempotent.
func (m *Manager) CloseAll() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	all := make([]*Forwarder, 0, len(m.forwarders))
	for key, f := range m.forwarders {
		all = append(all, f)
		delete(m.forwarders, key)
	}
	m.mu.Unlock()

	var errs []error
	for _, f := range all {
		if err := f.Stop(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// EgressFile builds the file for one plugin using the store's assignments and
// the listeners created for its accounts.
func (m *Manager) EgressFile(pluginID string, strict bool, defaultMode string, accounts []string) (model.EgressFile, error) {
	if m.store == nil {
		return model.EgressFile{}, errors.New("egress: manager has no store")
	}
	assignments, err := m.store.ListAccountEgress(pluginID)
	if err != nil {
		return model.EgressFile{}, fmt.Errorf("egress: list account egress for %s: %w", pluginID, err)
	}
	return BuildEgressFile(pluginID, strict, defaultMode, accounts, assignments, m.LocalURLFor), nil
}
