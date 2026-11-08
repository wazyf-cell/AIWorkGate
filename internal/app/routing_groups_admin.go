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
	"aiworkgate/internal/model"
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

// ── 一键模板（task-44，port of magpie provider/grouptemplate.go）──
//
// 三个模板按**当前目录**现场选模型，而不是硬编码名单：难题→最强、其余→便宜
// （smart）；默认便宜、长任务转最强（thrifty）；最多三个插件各取最强、一家
// 不可用切下一家（steady）。强弱用插件上报的费率倍率（高=贵=旗舰）与上下文
// 长度近似，便宜用「倍率 ≤ 最强的 1/4」或 id 里的小模型词（mini/flash/…），
// 语义沿用 magpie 的 smallWords/sizeWord（grouptemplate.go:77-108），只是把
// 美元价换成了倍率——aiworkgate 目录没有厂商美元价。
//
// 模板只产预填草稿（match + rules），由前端放进编辑器、用户检查后保存；
// 这里不做任何落盘。

// routingSmallWords 是便宜模型 id 里常见的词；tinyWords 是小到不该单独扛
// agent 回合的。与 magpie grouptemplate.go:78-79 同表。
var (
	routingSmallWords = map[string]bool{
		"mini": true, "nano": true, "flash": true, "haiku": true, "lite": true,
		"small": true, "air": true, "turbo": true, "fast": true, "instant": true,
		"tiny": true, "highspeed": true,
	}
	routingTinyWords = map[string]bool{"nano": true, "lite": true, "tiny": true}
	routingSizeWord  = regexp.MustCompile(`^(\d+(?:\.\d+)?)b$`)
)

// routingSizeOf 读模型 id 自报的身材：small，以及 tiny（≤15b）。
func routingSizeOf(model string) (small, tiny bool) {
	for _, w := range strings.FieldsFunc(strings.ToLower(model), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '.'
	}) {
		if m := routingSizeWord.FindStringSubmatch(w); m != nil {
			var b float64
			fmt.Sscan(m[1], &b)
			small, tiny = small || b <= 72, tiny || b <= 15
			continue
		}
		small = small || routingSmallWords[w]
		tiny = tiny || routingTinyWords[w]
	}
	return small, tiny
}

// templateModel 是目录里一个可入模板的模型及模板给它记的权 重。
type templateModel struct {
	id         string // "plugin/model"
	plugin     string
	multiplier float64 // 插件上报的费率倍率；0 = 未上报，不参与价格比较
	free       bool
	small      bool
	tiny       bool
	context    int
}

// templateModelsOf 把启用的目录行折成模板候选（跳过已移除的行）。
func templateModelsOf(catalog []model.ModelCatalogEntry) []templateModel {
	out := make([]templateModel, 0, len(catalog))
	for _, row := range catalog {
		if row.RemovedAt != 0 {
			continue
		}
		small, tiny := routingSizeOf(row.ModelID)
		out = append(out, templateModel{
			id: row.PluginID + "/" + row.ModelID, plugin: row.PluginID,
			multiplier: row.Multiplier, free: row.Free,
			small: small, tiny: tiny, context: row.ContextLength,
		})
	}
	return out
}

// templateStronger 排强模型：已知费率高者优先，其次上下文更长，最后 id 稳定。
func templateStronger(a, b templateModel) bool {
	if a.multiplier > 0 && b.multiplier > 0 && a.multiplier != b.multiplier {
		return a.multiplier > b.multiplier
	}
	if (a.multiplier > 0) != (b.multiplier > 0) {
		return a.multiplier > 0
	}
	if a.context != b.context {
		return a.context > b.context
	}
	return a.id < b.id
}

// strongestTemplateModel 是目录里最强的模型：small 词的模型不当旗舰。
func strongestTemplateModel(models []templateModel) (templateModel, bool) {
	found := false
	var best templateModel
	for _, m := range models {
		if m.small {
			continue
		}
		if !found || templateStronger(m, best) {
			best, found = m, true
		}
	}
	return best, found
}

// cheapestTemplateBeside 是配在最强旁边的便宜模型：倍率 ≤ 最强的 1/4，或
// （两边都没价时）small 词的模型。tiny 的排最后但不禁用。
func cheapestTemplateBeside(models []templateModel, strong templateModel) (templateModel, bool) {
	found := false
	var best templateModel
	for _, m := range models {
		if m.id == strong.id {
			continue
		}
		fits := strong.multiplier > 0 && m.multiplier > 0 && m.multiplier <= strong.multiplier/4
		if !fits && m.small && !strong.small {
			fits = true
		}
		if !fits {
			continue
		}
		if !found || templateCheaper(m, best) {
			best, found = m, true
		}
	}
	return best, found
}

func templateCheaper(a, b templateModel) bool {
	// tiny 惩罚先于价格（magpie grouptemplate.go:187-193 的排序：tiny 最后），
	// 一个非 tiny 的次便宜模型胜过 tiny 的最便宜模型。
	if a.tiny != b.tiny {
		return !a.tiny
	}
	if a.multiplier > 0 && b.multiplier > 0 && a.multiplier != b.multiplier {
		return a.multiplier < b.multiplier
	}
	return a.id < b.id
}

// steadyTemplateModels 是最多三个插件各自最强的模型，按目录（插件）顺序。
func steadyTemplateModels(models []templateModel) []templateModel {
	best := map[string]templateModel{}
	var order []string
	for _, m := range models {
		cur, ok := best[m.plugin]
		if !ok {
			order = append(order, m.plugin)
			best[m.plugin] = m
			continue
		}
		if templateStronger(m, cur) {
			best[m.plugin] = m
		}
	}
	out := make([]templateModel, 0, 3)
	for i, plugin := range order {
		if i >= 3 {
			break
		}
		out = append(out, best[plugin])
	}
	return out
}

// routingTemplateView 是下发给编辑器的一个模板。
type routingTemplateView struct {
	Kind        string            `json:"kind"`
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Match       []string          `json:"match,omitempty"`
	Rules       []routingRuleView `json:"rules,omitempty"`
	// Why 非空表示当前目录组不出这个模板，按钮置灰并显示原因。
	Why string `json:"why,omitempty"`
}

// buildRoutingTemplates 按当前目录出三个模板；出不了的带 Why。
func buildRoutingTemplates(catalog []model.ModelCatalogEntry) []routingTemplateView {
	models := templateModelsOf(catalog)
	noModels := "目录里还没有可用模型：先在模型池接入模型"
	strong, hasStrong := strongestTemplateModel(models)
	cheap, hasCheap := cheapestTemplateBeside(models, strong)
	steady := steadyTemplateModels(models)

	smart := routingTemplateView{
		Kind: "smart-split", Name: "难易分流",
		Description: "难题走最强模型，短问（≤2000 字符）走便宜模型；未接判据上游时按请求长度启发。",
	}
	thrifty := routingTemplateView{
		Kind: "cheap-first", Name: "省钱优先",
		Description: "默认全走便宜模型，长任务（≥8000 字符）自动转最强模型。",
	}
	steadyT := routingTemplateView{
		Kind: "never-stuck", Name: "永不卡死",
		Description: "最多三个插件各取最强模型，一家不可用或限流时自动切下一家。",
	}
	if !hasStrong {
		smart.Why, thrifty.Why, steadyT.Why = noModels, noModels, noModels
		return []routingTemplateView{smart, thrifty, steadyT}
	}
	if !hasCheap {
		smart.Why = "目录里没有比 " + strong.id + " 便宜的可分担模型"
		thrifty.Why = smart.Why
	}
	if len(steady) < 2 {
		steadyT.Why = "需要至少两个插件各有一个模型，才能在一家不可用时切换"
	}
	if hasCheap {
		smart.Match = []string{strong.id, cheap.id}
		smart.Rules = []routingRuleView{{
			Name: "easy-turn", Match: []string{cheap.id},
			When: config.RoutingWhen{MaxChars: 2000},
		}}
		thrifty.Match = []string{cheap.id, strong.id}
		thrifty.Rules = []routingRuleView{{
			Name: "hard-turn", Match: []string{strong.id},
			When: config.RoutingWhen{MinChars: 8000},
		}}
	}
	if len(steady) >= 2 {
		steadyT.Match = make([]string, 0, len(steady))
		for _, m := range steady {
			steadyT.Match = append(steadyT.Match, m.id)
		}
	}
	return []routingTemplateView{smart, thrifty, steadyT}
}

// routingGroupCatalog 是模板用的目录快照：启用行 + 覆盖合并后的倍率，按
// 插件顺序（universe 的字母序按插件名排，等价于稳定顺序）。
func (a *App) routingGroupCatalog() []model.ModelCatalogEntry {
	if a == nil || a.store == nil || a.pluginMgr == nil {
		return nil
	}
	var out []model.ModelCatalogEntry
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
			row.PluginID = id
			out = append(out, row)
		}
	}
	return out
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
	// 自动分组预览（task-44）：与快照同一套求解（mergeAutoBindings 的自动
	// 部分），标出被用户同名组接管的项。这里只读不写，自动组不进 config。
	autoMap := autoGroupsOf(universe)
	autoGroups := make([]routingAutoGroupView, 0, len(autoMap))
	userIDs := map[string]bool{}
	for _, group := range cfg.RoutingGroups {
		userIDs[group.ID] = true
	}
	for id, binding := range autoMap {
		members := memberIDs(binding.Items)
		autoGroups = append(autoGroups, routingAutoGroupView{
			ID: id, Display: binding.Display, Members: members, Edited: userIDs[id],
		})
	}
	sort.Slice(autoGroups, func(i, j int) bool { return autoGroups[i].ID < autoGroups[j].ID })
	return map[string]any{
		"groups":             groups,
		"universe":           universe,
		"decision_upstreams": upstreams,
		"auto_groups":        autoGroups,
		"templates":          buildRoutingTemplates(a.routingGroupCatalog()),
	}, nil
}

// routingAutoGroupView 是一个自动组在管理页的只读预览。Edited 表示用户已存
// 了同名组：那一份接管路由，自动组不再单独生效（尾随逻辑改挂到用户的组上）。
type routingAutoGroupView struct {
	ID      string   `json:"id"`
	Display string   `json:"display,omitempty"`
	Members []string `json:"members"`
	Edited  bool     `json:"edited"`
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
