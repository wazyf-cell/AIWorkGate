// 问题6A（task-43）：工具别名指纹清洗的宿主接线。
//
// 持久化：<dataDir>\tool-alias.json（无凭据，不走 DPAPI；形状 = tool_alias.File）。
// 生效链：PUT /api/tool-alias → 校验 → 落盘 → 当前 /v1 处理器热替换生效表
// （SetToolAlias，atomic）；网关重建路径（redact 变更等）经 gatewayOptions 的
// Options.ToolAlias 读同一份文件，两条路径不漂移。Enabled=false 时网关停用整表
// （原名透传），自定义规则仍保留在文件里。
package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"aiworkgate/internal/gateway/tool_alias"
	"aiworkgate/internal/webui"
)

func (a *App) toolAliasPath() string {
	return filepath.Join(a.dataDir, "tool-alias.json")
}

// loadToolAliasFile 读取持久化文件；缺失/损坏按停用空表处理（fail-open 到关闭态）。
func (a *App) loadToolAliasFile() tool_alias.File {
	var f tool_alias.File
	b, err := os.ReadFile(a.toolAliasPath())
	if err != nil {
		return f // 缺失 = 默认停用
	}
	if json.Unmarshal(b, &f) != nil {
		return tool_alias.File{}
	}
	return f
}

// toolAliasTable 把持久化文件折算成网关生效表：Enabled=false 或空规则 = nil（停用）。
func (a *App) toolAliasTable() *tool_alias.Table {
	f := a.loadToolAliasFile()
	if !f.Enabled || len(f.Rules) == 0 {
		return nil
	}
	return tool_alias.Merge(f.Rules)
}

// toolAliasStatus serves GET /api/tool-alias: the persisted file plus the
// built-in default table reference and merge warnings.
func (a *App) toolAliasStatus() (webui.ToolAliasStatus, error) {
	f := a.loadToolAliasFile()
	status := webui.ToolAliasStatus{
		Enabled:   f.Enabled,
		Rules:     append([]tool_alias.Rule(nil), f.Rules...),
		UpdatedAt: f.UpdatedAt,
		Defaults:  len(tool_alias.DefaultRules()),
	}
	// 参考面：默认表 55 条只读透出，UI 展示「内置默认（只读）」区
	defaults := tool_alias.DefaultRules()
	status.DefaultRules = make([]tool_alias.Rule, len(defaults))
	copy(status.DefaultRules, defaults)
	// 合并告警（构建期别名冲突说明）随状态带出
	if tbl := a.toolAliasTable(); tbl != nil {
		status.Warnings = tbl.Warnings()
	}
	return status, nil
}

// saveToolAlias applies PUT /api/tool-alias: validate every rule, persist, then
// hot-swap the live table (保存即生效，无需重启网关).
func (a *App) saveToolAlias(file tool_alias.File) error {
	for _, r := range file.Rules {
		if err := tool_alias.Validate(r); err != nil {
			return fmt.Errorf("%w: %v", webui.ErrInvalidRequest, err)
		}
	}
	if file.Enabled && len(file.Rules) == 0 {
		return fmt.Errorf("%w: 启用工具别名至少需要一条规则", webui.ErrInvalidRequest)
	}
	// 归一化：from/to 去首尾空白（语义层拒绝空白已由 Validate 兜住，这里只裁剪）
	for i := range file.Rules {
		file.Rules[i].From = trimAll(file.Rules[i].From)
		file.Rules[i].To = trimAll(file.Rules[i].To)
	}
	file.UpdatedAt = time.Now().Unix()
	b, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(a.toolAliasPath(), b, 0o600); err != nil {
		return err
	}
	a.applyToolAliasLive()
	a.writeAudit("config", "tool_alias", filepath.Base(a.toolAliasPath()), "ok")
	return nil
}

// applyToolAliasLive hot-swaps the current /v1 handler's table. The gateway
// rebuild path picks the same file up via gatewayOptions.
func (a *App) applyToolAliasLive() {
	if setter, ok := a.currentGateway().(interface{ SetToolAlias(*tool_alias.Table) }); ok {
		setter.SetToolAlias(a.toolAliasTable())
	}
}

// trimAll 去除规则文本首尾空白。
func trimAll(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}
