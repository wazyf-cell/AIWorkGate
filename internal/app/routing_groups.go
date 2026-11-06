package app

// P11 pattern routing groups. A group is a routable model id whose members are
// re-resolved on every snapshot: `*`/`?` globs expand across the whole id
// (slashes included) and `re:<expr>` uses Go regexp. Members keep the order of
// the match list, so a group is also an explicit source order.

import (
	"regexp"
	"sort"
	"strings"

	"aiworkgate/internal/config"
	"aiworkgate/internal/scheduler"
)

// resolveRoutingGroups expands the configured groups against the current model
// universe ("plugin/model"). Empty groups are not published.
func resolveRoutingGroups(groups []config.RoutingGroupConfig, universe []string) map[string]scheduler.Binding {
	out := make(map[string]scheduler.Binding, len(groups))
	if len(groups) == 0 || len(universe) == 0 {
		return out
	}
	sorted := append([]string(nil), universe...)
	sort.Strings(sorted)
	for _, group := range groups {
		if group.Disabled || strings.TrimSpace(group.ID) == "" {
			continue
		}
		items := resolveGroupMatch(group.Match, sorted)
		if len(items) == 0 {
			continue
		}
		display := strings.TrimSpace(group.Display)
		if display == "" {
			display = group.ID
		}
		binding := scheduler.Binding{Logical: group.ID, Display: display, Items: items}
		for _, rule := range group.Rules {
			ruleItems := resolveGroupMatch(rule.Match, sorted)
			if len(ruleItems) == 0 {
				continue
			}
			binding.Rules = append(binding.Rules, scheduler.BindingRule{
				Name:  rule.Name,
				Items: ruleItems,
				When: scheduler.BindingWhen{
					MinChars: rule.When.MinChars, MaxChars: rule.When.MaxChars,
					Image: rule.When.Image, Thinking: rule.When.Thinking, Agent: rule.When.Agent,
					StartHour: rule.When.StartHour, EndHour: rule.When.EndHour,
				},
			})
		}
		out[group.ID] = binding
	}
	return out
}

func resolveGroupMatch(match []string, universe []string) []scheduler.Source {
	var items []scheduler.Source
	seen := map[string]bool{}
	for _, pattern := range match {
		for _, id := range universe {
			if seen[id] || !routingPatternMatch(pattern, id) {
				continue
			}
			plugin, model, ok := strings.Cut(id, "/")
			if !ok || strings.TrimSpace(plugin) == "" || strings.TrimSpace(model) == "" {
				continue
			}
			seen[id] = true
			items = append(items, scheduler.Source{PluginID: plugin, Model: model})
		}
	}
	return items
}

// routingPatternMatch supports exact ids, `*`/`?` globs over the whole id, and
// `re:` regular expressions. Matching is case-sensitive, like model ids.
func routingPatternMatch(pattern, id string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return false
	}
	if expr, ok := strings.CutPrefix(pattern, "re:"); ok {
		re, err := regexp.Compile(strings.TrimSpace(expr))
		return err == nil && re.MatchString(id)
	}
	if pattern == "*" {
		return true
	}
	if !strings.ContainsAny(pattern, "*?") {
		return pattern == id
	}
	return globMatch(pattern, id)
}

// globMatch turns `*` (any run, including '/') and `?` (one character) into an
// anchored regular expression. Model ids contain slashes, which rules out
// path.Match.
func globMatch(pattern, id string) bool {
	var b strings.Builder
	b.WriteString("^")
	for _, r := range pattern {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	return err == nil && re.MatchString(id)
}

// routingGroupBindings reads the current config and expands the groups against
// the snapshot's own pools.
func (a *App) routingGroupBindings(snap *scheduler.Snapshot) map[string]scheduler.Binding {
	if a == nil || snap == nil {
		return nil
	}
	groups := a.configSnapshot().RoutingGroups
	if len(groups) == 0 {
		return nil
	}
	var universe []string
	for id, pool := range snap.Pools {
		if pool == nil || !pool.UpstreamEnabled {
			continue
		}
		for modelID := range pool.Models {
			id = strings.TrimSpace(id)
			modelID = strings.TrimSpace(modelID)
			if id == "" || modelID == "" {
				continue
			}
			universe = append(universe, id+"/"+modelID)
		}
	}
	return resolveRoutingGroups(groups, universe)
}
