// 问题6A（task-43）：GET/PUT /api/tool-alias —— 工具别名指纹清洗的管理端点。
//
// GET  返回持久化状态（enabled + 自定义规则）与内置默认表参考（只读 55 条）、
//      合并告警（别名冲突说明）。nil 回调答 501（与其它可选端点一致）。
// PUT  body = {"enabled":bool,"rules":[{from,to}...]}：逐条校验（from 非空、
//      to 非空别名或空串停用、禁多候选 |）后交宿主落盘并热替换网关生效表。
//      校验失败 → 400 invalid_request；宿主回执错误 → 400/500 由 writeUsageError
//      同款映射。
package webui

import (
	"errors"
	"net/http"

	"aiworkgate/internal/gateway/tool_alias"
)

// ToolAliasStatus 是 GET /api/tool-alias 的响应体。
type ToolAliasStatus struct {
	Enabled bool               `json:"enabled"`
	Rules   []tool_alias.Rule  `json:"rules"`
	// UpdatedAt unix 秒（未保存过 = 0）。
	UpdatedAt int64            `json:"updated_at,omitempty"`
	// Defaults 是内置默认表条数；DefaultRules 是默认表内容（只读参考，UI 展示）。
	Defaults     int           `json:"defaults"`
	DefaultRules []tool_alias.Rule `json:"default_rules,omitempty"`
	// Warnings 是合并期别名冲突说明（构建期先到先得的剔除记录）。
	Warnings []string `json:"warnings,omitempty"`
}

func (s *server) handleToolAlias(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet, http.MethodPut) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		if s.opts.ToolAliasGet == nil {
			s.unavailable(w)
			return
		}
		status, err := s.opts.ToolAliasGet()
		if err != nil {
			writeUsageError(w, "tool alias", err)
			return
		}
		writeJSON(w, http.StatusOK, status)
	case http.MethodPut:
		if s.opts.ToolAliasSet == nil {
			s.unavailable(w)
			return
		}
		var body tool_alias.File
		if !decodeJSON(w, r, &body) {
			return
		}
		for _, rule := range body.Rules {
			if err := tool_alias.Validate(rule); err != nil {
				writeError(w, http.StatusBadRequest, "tool alias: "+err.Error(), "invalid_request")
				return
			}
		}
		if err := s.opts.ToolAliasSet(body); err != nil {
			if errors.Is(err, ErrInvalidRequest) {
				writeError(w, http.StatusBadRequest, "tool alias: "+err.Error(), "invalid_request")
				return
			}
			writeError(w, http.StatusInternalServerError, "tool alias: "+err.Error(), "internal_error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": body.Enabled, "count": len(body.Rules)})
	}
}
