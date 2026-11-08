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
		if len(group.Weights) > 0 {
			binding.Weights = group.Weights
		}
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
					MinTokens: rule.When.MinTokens, MaxTokens: rule.When.MaxTokens,
					Image: rule.When.Image, Thinking: rule.When.Thinking, Agent: rule.When.Agent,
					StartHour: rule.When.StartHour, EndHour: rule.When.EndHour,
				},
			})
		}
		if intent := group.Intent; intent != nil {
			binding.Intent = &scheduler.IntentBinding{
				Upstream:   intent.Upstream,
				Labels:     append([]string(nil), intent.Labels...),
				Route:      intent.Route,
				Default:    intent.Default,
				ProbeChars: intent.ProbeChars,
			}
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

// ── 自动分组（task-44，port of magpie provider/group.go:838-888 autoGroups）──
//
// 同一模型被两个及以上插件同时服务时，调度快照自动生成一个 "auto-" 前缀的
// 组：组员按 universe（插件目录）顺序轮转，无需用户手工建组。用户存了同名
// 组（在 Groups 页编辑过自动组并保存）时以用户的为准，且**尾随**：之后新
// 接入的插件只要服务同一个（归一后的）模型，就在用户成员之后追加加入
// （magpie 的 found-group follow，group_follow_test.go:44-115）。
//
// 与 magpie 的差距（有意收敛）：magpie 记录 Group.Seen 使「用户删掉的成员
// 不回流」；本仓配置结构（config.RoutingGroupConfig）没有该字段，因此被删
// 成员在它仍存在于目录时会重新尾随加入。需要严格语义时给配置加 seen 列表。

// sameModelKey 把模型名折到各厂商共识的名字上：小写、把 . / _ / 空格折叠成
// 连字符（deepseek-v4.1-flash 与 DeepSeek-V4.1-Flash 与 deepseek-v4-1-flash
// 同键）。这是 magpie catalog.SameModel 的保守子集——只折分隔符，不做
// 词典级拼写归并。
func sameModelKey(model string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(model)) {
		switch {
		case r == '-' || r == '.' || r == '_' || r == ' ':
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		default:
			b.WriteRune(r)
			lastDash = false
		}
	}
	return strings.Trim(b.String(), "-")
}

// autoGroupIDOf 是自动组的可路由 id："auto-" + 键的 slug（只留小写字母数字）。
func autoGroupIDOf(key string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(key) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return "auto-" + strings.Trim(b.String(), "-")
}

// autoGroupsOf 把 universe 按归一键分组；一个键被 ≥2 个插件服务才成组，
// 组员按 universe 顺序、每插件只占一名（同插件上报两种拼写不重复入组）。
func autoGroupsOf(universe []string) map[string]scheduler.Binding {
	var order []string
	by := map[string][]string{}
	perPlugin := map[string]bool{}
	for _, id := range universe {
		plugin, model, ok := strings.Cut(id, "/")
		if !ok || strings.TrimSpace(plugin) == "" || strings.TrimSpace(model) == "" {
			continue
		}
		key := sameModelKey(model)
		if key == "" {
			continue
		}
		slot := plugin + "/" + key
		if perPlugin[slot] {
			continue
		}
		perPlugin[slot] = true
		if _, fresh := by[key]; !fresh {
			order = append(order, key)
		}
		by[key] = append(by[key], id)
	}
	out := make(map[string]scheduler.Binding, len(order))
	for _, key := range order {
		es := by[key]
		plugins := map[string]bool{}
		for _, e := range es {
			p, _, _ := strings.Cut(e, "/")
			plugins[p] = true
		}
		if len(plugins) < 2 {
			continue
		}
		id := autoGroupIDOf(key)
		display, _, _ := strings.Cut(es[0], "/")
		out[id] = scheduler.Binding{Logical: id, Display: display, Items: sourcesOfIDs(es)}
	}
	return out
}

func sourcesOfIDs(ids []string) []scheduler.Source {
	out := make([]scheduler.Source, 0, len(ids))
	for _, id := range ids {
		plugin, model, ok := strings.Cut(id, "/")
		if !ok {
			continue
		}
		out = append(out, scheduler.Source{PluginID: plugin, Model: model})
	}
	return out
}

// appendAutoFollow 是用户改过的自动组的尾随：以该组成员的归一键为准，把
// universe 里新出现的同键成员（不含已在组内的）追加到用户成员之后。
func appendAutoFollow(binding scheduler.Binding, universe []string) scheduler.Binding {
	if len(binding.Items) == 0 {
		return binding
	}
	keys := map[string]bool{}
	for _, item := range binding.Items {
		if key := sameModelKey(item.Model); key != "" {
			keys[key] = true
		}
	}
	known := map[string]bool{}
	for _, item := range binding.Items {
		known[item.PluginID+"/"+item.Model] = true
	}
	var extra []scheduler.Source
	for _, id := range universe {
		if known[id] {
			continue
		}
		plugin, model, ok := strings.Cut(id, "/")
		if !ok || strings.TrimSpace(plugin) == "" || strings.TrimSpace(model) == "" {
			continue
		}
		if keys[sameModelKey(model)] {
			extra = append(extra, scheduler.Source{PluginID: plugin, Model: model})
		}
	}
	if len(extra) == 0 {
		return binding
	}
	binding.Items = append(append([]scheduler.Source{}, binding.Items...), extra...)
	return binding
}

// mergeAutoBindings 把用户静态组与自动组合并：auto- 前缀的用户组先尾随，
// 自动组只填补没有同名用户组的空位（用户组永远赢）。
func mergeAutoBindings(user map[string]scheduler.Binding, universe []string) map[string]scheduler.Binding {
	out := make(map[string]scheduler.Binding, len(user))
	for id, binding := range user {
		if strings.HasPrefix(id, "auto-") {
			binding = appendAutoFollow(binding, universe)
		}
		out[id] = binding
	}
	for id, auto := range autoGroupsOf(universe) {
		if _, taken := out[id]; !taken {
			out[id] = auto
		}
	}
	return out
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
	return mergeAutoBindings(resolveRoutingGroups(groups, universe), universe)
}
