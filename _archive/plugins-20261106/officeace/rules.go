package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"
)

// RuleEntry is one literal replacement from the host rule pack.
type RuleEntry struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// StripEntry is one regex removed from reply text.
type StripEntry struct {
	Pattern string `json:"pattern"`
}

// RulesFile is the merged rule pack written by the host.
type RulesFile struct {
	Packs []string     `json:"packs"`
	Rules []RuleEntry  `json:"rules"`
	Strip []StripEntry `json:"strip"`
}

// RuleSet is the compiled form kept in memory.
type RuleSet struct {
	Packs  []string
	Rules  []RuleEntry
	Strips []*regexp.Regexp
}

func loadRules(path string) (RuleSet, error) {
	var rs RuleSet
	if strings.TrimSpace(path) == "" {
		return rs, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return rs, nil
		}
		return rs, fmt.Errorf("read rules: %w", err)
	}
	var rf RulesFile
	if err := json.Unmarshal(b, &rf); err != nil {
		return rs, fmt.Errorf("parse rules: %w", err)
	}
	rs.Packs = rf.Packs
	rs.Rules = rf.Rules
	for _, s := range rf.Strip {
		if strings.TrimSpace(s.Pattern) == "" {
			continue
		}
		re, err := regexp.Compile(s.Pattern)
		if err != nil {
			return RuleSet{}, fmt.Errorf("compile strip %q: %w", s.Pattern, err)
		}
		rs.Strips = append(rs.Strips, re)
	}
	return rs, nil
}

// apply performs literal replacements, then regex strips, and returns the
// number of replacements and strips that actually fired.
func (rs RuleSet) apply(text string) (string, int, int) {
	replaced, stripped := 0, 0
	for _, r := range rs.Rules {
		if r.From == "" {
			continue
		}
		if n := strings.Count(text, r.From); n > 0 {
			text = strings.ReplaceAll(text, r.From, r.To)
			replaced += n
		}
	}
	for _, re := range rs.Strips {
		if loc := re.FindAllStringIndex(text, -1); len(loc) > 0 {
			text = re.ReplaceAllString(text, "")
			stripped += len(loc)
		}
	}
	if stripped > 0 {
		text = strings.Join(strings.Fields(text), " ")
	}
	return text, replaced, stripped
}

func (p *Plugin) applyRules(text string) (string, int, int) {
	p.rulesMu.RLock()
	rs := p.rules
	p.rulesMu.RUnlock()
	return rs.apply(text)
}

func (p *Plugin) reload() (reloadResult, error) {
	res := reloadResult{OK: true}
	now := p.now()

	rs, rulesErr := loadRules(p.rulesPath)
	if rulesErr == nil {
		p.rulesMu.Lock()
		p.rules = rs
		p.rulesMu.Unlock()
	} else {
		res.Errors = append(res.Errors, rulesErr.Error())
	}

	ef, egressErr := loadEgress(p.egressPath)
	if egressErr == nil {
		p.egressMu.Lock()
		p.egress = ef
		p.egressMu.Unlock()
	} else {
		res.Errors = append(res.Errors, egressErr.Error())
	}

	p.resetTransports()

	p.rulesMu.RLock()
	activeRules := p.rules
	p.rulesMu.RUnlock()
	p.egressMu.RLock()
	activeEgress := p.egress
	p.egressMu.RUnlock()

	res.OK = len(res.Errors) == 0
	res.Packs = activeRules.Packs
	res.Rules = len(activeRules.Rules)
	res.Strip = len(activeRules.Strips)
	res.EgressAccounts = len(activeEgress.Accounts)
	res.Strict = activeEgress.Strict
	disc := p.discovery()
	res.OfficeAceHome = disc.Home
	res.OfficeAceFound = disc.Found
	res.At = now.Unix()

	p.mu.Lock()
	p.state.LastReload = now.Unix()
	p.state.RulesCount = res.Rules
	p.state.StripCount = res.Strip
	p.state.EgressAccs = res.EgressAccounts
	p.mu.Unlock()
	if err := p.save(); err != nil {
		return res, err
	}
	return res, nil
}

// reloadResult is the POST /reload response.
type reloadResult struct {
	OK             bool     `json:"ok"`
	Packs          []string `json:"packs"`
	Rules          int      `json:"rules"`
	Strip          int      `json:"strip"`
	EgressAccounts int      `json:"egress_accounts"`
	Strict         bool     `json:"strict"`
	OfficeAceHome  string   `json:"officeace_home,omitempty"`
	OfficeAceFound bool     `json:"officeace_found"`
	At             int64    `json:"at"`
	Errors         []string `json:"errors,omitempty"`
}
