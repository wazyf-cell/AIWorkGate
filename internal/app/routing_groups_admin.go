package app

// P11 routing-group admin: read the config-declared groups with their resolved
// members, and write an edited list back to config.json. Both the members and
// the rules are previewed against the current model catalog so an editor sees
// what a pattern actually selects before saving.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"aiworkgate/internal/config"
	"aiworkgate/internal/scheduler"
	"aiworkgate/internal/webui"
)

type routingGroupView struct {
	ID          string                      `json:"id"`
	Display     string                      `json:"display,omitempty"`
	Match       []string                    `json:"match"`
	Disabled    bool                        `json:"disabled"`
	Rules       []routingRuleView           `json:"rules,omitempty"`
	Weights     map[string]int              `json:"weights,omitempty"`
	Intent      *config.RoutingIntentConfig `json:"intent,omitempty"`
	Members     []string                    `json:"members"`
	RuleMembers []routingRuleMembers        `json:"rule_members,omitempty"`
}

type routingRuleView struct {
	Name  string             `json:"name"`
	Match []string           `json:"match"`
	When  config.RoutingWhen `json:"when"`
}

type routingRuleMembers struct {
	Name    string   `json:"name"`
	Members []string `json:"members"`
}

// routingGroupUniverse lists the model ids a pattern can match, as
// "plugin/model", from the enabled rows of every installed plugin's catalog.
func (a *App) routingGroupUniverse() []string {
	universe := make([]string, 0)
	if a == nil || a.store == nil || a.pluginMgr == nil {
		return universe
	}
	seen := map[string]bool{}
	for _, status := range a.pluginMgr.List() {
		id := strings.TrimSpace(status.Manifest.ID)
		if id == "" || !status.Enabled {
			continue
		}
		rows, err := a.store.ListModelCatalog(id)
		if err != nil {
			continue
		}
		for _, row := range rows {
			if !row.Enabled {
				continue
			}
			modelID := strings.TrimSpace(row.ModelID)
			if modelID == "" {
				continue
			}
			entry := id + "/" + modelID
			if seen[entry] {
				continue
			}
			seen[entry] = true
			universe = append(universe, entry)
		}
	}
	sort.Strings(universe)
	return universe
}

func (a *App) routingGroupsView() (any, error) {
	cfg := a.configSnapshot()
	universe := a.routingGroupUniverse()
	groups := make([]routingGroupView, 0, len(cfg.RoutingGroups))
	for _, group := range cfg.RoutingGroups {
		view := routingGroupView{
			ID: group.ID, Display: group.Display, Match: group.Match,
			Disabled: group.Disabled, Weights: group.Weights, Intent: group.Intent,
			Members: memberIDs(resolveGroupMatch(group.Match, universe)),
		}
		for _, rule := range group.Rules {
			view.Rules = append(view.Rules, routingRuleView{Name: rule.Name, Match: rule.Match, When: rule.When})
			view.RuleMembers = append(view.RuleMembers, routingRuleMembers{
				Name:    rule.Name,
				Members: memberIDs(resolveGroupMatch(rule.Match, universe)),
			})
		}
		groups = append(groups, view)
	}
	upstreams := make([]map[string]any, 0, len(cfg.Decision.Upstreams))
	for _, upstream := range cfg.Decision.Upstreams {
		if upstream.Disabled {
			continue
		}
		upstreams = append(upstreams, map[string]any{
			"id": upstream.ID, "protocol": upstream.Protocol, "model_id": upstream.ModelID,
		})
	}
	return map[string]any{
		"groups":             groups,
		"universe":           universe,
		"decision_upstreams": upstreams,
	}, nil
}

func memberIDs(items []scheduler.Source) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.PluginID+"/"+item.Model)
	}
	return out
}

// saveRoutingGroups replaces config.routing_groups after a strict check: the
// normalizer silently drops unusable entries, which is fine for a hand-edited
// file but would hide an editor's mistake.
func (a *App) saveRoutingGroups(raw json.RawMessage) (any, error) {
	var groups []config.RoutingGroupConfig
	if err := json.Unmarshal(raw, &groups); err != nil {
		return nil, fmt.Errorf("%w: 路由组必须是数组：%v", webui.ErrInvalidRequest, err)
	}
	if err := validateRoutingGroups(groups); err != nil {
		return nil, err
	}
	next := a.configSnapshot()
	next.RoutingGroups = groups
	next.Normalize()
	payload, err := json.Marshal(next)
	if err != nil {
		return nil, err
	}
	if err := a.saveSettings(payload); err != nil {
		return nil, err
	}
	return a.routingGroupsView()
}

func validateRoutingGroups(groups []config.RoutingGroupConfig) error {
	ids := map[string]bool{}
	for i, group := range groups {
		group.ID = strings.TrimSpace(group.ID)
		if group.ID == "" {
			return fmt.Errorf("%w: 第 %d 个路由组缺少 id", webui.ErrInvalidRequest, i+1)
		}
		if strings.ContainsAny(group.ID, "/ \t") {
			return fmt.Errorf("%w: 路由组 id %q 不能包含斜杠或空白", webui.ErrInvalidRequest, group.ID)
		}
		if ids[group.ID] {
			return fmt.Errorf("%w: 路由组 id %q 重复", webui.ErrInvalidRequest, group.ID)
		}
		ids[group.ID] = true
	}
	for _, group := range groups {
		if err := validateRoutingMatch(group.ID, "match", group.Match); err != nil {
			return err
		}
		seenRules := map[string]bool{}
		for i, rule := range group.Rules {
			name := strings.TrimSpace(rule.Name)
			if name == "" {
				name = fmt.Sprintf("rule-%d", i+1)
			}
			if seenRules[name] {
				return fmt.Errorf("%w: 路由组 %q 的规则名 %q 重复", webui.ErrInvalidRequest, group.ID, name)
			}
			seenRules[name] = true
			if err := validateRoutingMatch(group.ID, "规则 "+name, rule.Match); err != nil {
				return err
			}
			if rule.When.StartHour != nil && (*rule.When.StartHour < 0 || *rule.When.StartHour > 24) {
				return fmt.Errorf("%w: 路由组 %q 的规则 %q 起始小时超出 0~24", webui.ErrInvalidRequest, group.ID, name)
			}
			if rule.When.EndHour != nil && (*rule.When.EndHour < 0 || *rule.When.EndHour > 24) {
				return fmt.Errorf("%w: 路由组 %q 的规则 %q 结束小时超出 0~24", webui.ErrInvalidRequest, group.ID, name)
			}
			if rule.When.MinChars < 0 || rule.When.MaxChars < 0 || rule.When.MinTokens < 0 || rule.When.MaxTokens < 0 {
				return fmt.Errorf("%w: 路由组 %q 的规则 %q 长度/Token 下限不能为负", webui.ErrInvalidRequest, group.ID, name)
			}
		}
		for member, weight := range group.Weights {
			if strings.TrimSpace(member) == "" || weight < 1 || weight > 1000 {
				return fmt.Errorf("%w: 路由组 %q 的权重 %q=%d 无效（成员必填，权重 1~1000）",
					webui.ErrInvalidRequest, group.ID, member, weight)
			}
		}
		if intent := group.Intent; intent != nil && intent.Enabled {
			if len(intent.Labels) == 0 || len(intent.Route) == 0 {
				return fmt.Errorf("%w: 路由组 %q 的意图规则需要至少一个标签和一条路由", webui.ErrInvalidRequest, group.ID)
			}
			labels := map[string]bool{}
			for _, label := range intent.Labels {
				label = strings.TrimSpace(label)
				if label == "" {
					return fmt.Errorf("%w: 路由组 %q 的意图标签不能为空", webui.ErrInvalidRequest, group.ID)
				}
				labels[label] = true
			}
			for label, target := range intent.Route {
				if !labels[strings.TrimSpace(label)] {
					return fmt.Errorf("%w: 路由组 %q 的意图路由 %q 不在标签列表中", webui.ErrInvalidRequest, group.ID, label)
				}
				target = strings.TrimSpace(target)
				if target == "" || !ids[target] {
					return fmt.Errorf("%w: 路由组 %q 的意图目标 %q 不是已定义的路由组",
						webui.ErrInvalidRequest, group.ID, target)
				}
			}
			if fallback := strings.TrimSpace(intent.Default); fallback != "" && !ids[fallback] {
				return fmt.Errorf("%w: 路由组 %q 的意图兜底 %q 不是已定义的路由组",
					webui.ErrInvalidRequest, group.ID, fallback)
			}
		}
	}
	return nil
}

func validateRoutingMatch(groupID, field string, patterns []string) error {
	usable := 0
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		if expr, ok := strings.CutPrefix(pattern, "re:"); ok {
			if _, err := regexp.Compile(strings.TrimSpace(expr)); err != nil {
				return fmt.Errorf("%w: 路由组 %q 的%s正则 %q 无效：%v",
					webui.ErrInvalidRequest, groupID, field, pattern, err)
			}
		}
		usable++
	}
	if usable == 0 {
		return fmt.Errorf("%w: 路由组 %q 的%s至少需要一条匹配模式", webui.ErrInvalidRequest, groupID, field)
	}
	return nil
}
