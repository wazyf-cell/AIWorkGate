package main

import (
	"path/filepath"
	"sync"
)

// saveAccounts writes accounts.dat atomically. Callers must not hold p.mu.
func (p *Plugin) saveAccounts() error {
	p.saveMu.Lock()
	defer p.saveMu.Unlock()
	p.mu.RLock()
	copies := make([]*Account, 0, len(p.accounts))
	for _, a := range p.accounts {
		copies = append(copies, deepCopyAccount(a))
	}
	p.mu.RUnlock()
	return saveAccounts(p.DataDir, copies)
}

func (p *Plugin) saveState() error {
	p.saveMu.Lock()
	defer p.saveMu.Unlock()
	p.stateMu.Lock()
	cp := *p.state
	p.stateMu.Unlock()
	return saveState(p.DataDir, &cp)
}

func (p *Plugin) uptimeSeconds() int64 {
	return int64(p.now().Sub(p.startedAt).Seconds())
}

func (p *Plugin) accountSnapshots() []*Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*Account, 0, len(p.accounts))
	for _, a := range p.accounts {
		out = append(out, deepCopyAccount(a))
	}
	return out
}

// findAccount returns a copy of the account with the given id.
func (p *Plugin) findAccount(id string) (*Account, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, a := range p.accounts {
		if a.ID == id {
			return deepCopyAccount(a), true
		}
	}
	return nil, false
}

func (p *Plugin) accountByIDLocked(id string) *Account {
	for _, a := range p.accounts {
		if a.ID == id {
			return a
		}
	}
	return nil
}

func (p *Plugin) applyRules(text string) (string, int, int) {
	p.rulesMu.RLock()
	rs := p.rules
	p.rulesMu.RUnlock()
	return rs.apply(text)
}

func (p *Plugin) addRuleHits(rules, strips int) {
	if rules+strips <= 0 {
		return
	}
	p.stateMu.Lock()
	p.state.RuleHits += int64(rules)
	p.state.StripHits += int64(strips)
	p.stateMu.Unlock()
	if err := p.saveState(); err != nil {
		p.logLine("warn", "save state failed", map[string]any{"error": err.Error()})
	}
}

func (p *Plugin) refreshLock(id string) *sync.Mutex {
	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()
	m, ok := p.refresh[id]
	if !ok {
		m = &sync.Mutex{}
		p.refresh[id] = m
	}
	return m
}

func (p *Plugin) dataPath(name string) string { return filepath.Join(p.DataDir, name) }
