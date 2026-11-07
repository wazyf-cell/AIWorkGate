package webui

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// P10 收尾：日志脱敏管理端点。config.json 的 redact 段此前只能手改（enabled
// 缺省 false），这里给设置页一个读写口；生效链（保存 → 热更新网关）由宿主
// 回调负责，本包保持 transport-only。
//
// Shapes:
//
//	GET /api/redact  -> RedactStatus
//	PUT /api/redact  -> RedactStatus（部分更新：缺省字段保持原值）
//
// key_file 不进这个端点：它是指纹密钥的落盘路径，不是可编辑的开关，继续走
// config.json 的高级路径；词表（kinds/categories）本身不是凭据，可以原样读写。

// RedactStatus is the GET/PUT response. Kinds/Categories 为空数组表示「用规则
// 默认表」，不是「什么都不脱」。
type RedactStatus struct {
	Enabled    bool     `json:"enabled"`
	RouteAware bool     `json:"route_aware"`
	Kinds      []string `json:"kinds"`
	Categories []string `json:"categories"`
}

// RedactUpdate is a partial update: every field is optional, so the SPA can
// flip one switch without resending the lists.
type RedactUpdate struct {
	Enabled    *bool     `json:"enabled"`
	RouteAware *bool     `json:"route_aware"`
	Kinds      *[]string `json:"kinds"`
	Categories *[]string `json:"categories"`
}

// redactListLimit bounds one identifier list; the rule table has ~30 kinds and
// 10 categories, so anything larger is a paste accident, not an intent.
const redactListLimit = 64

// validateRedactUpdate normalizes kinds (upper case) and categories (lower
// case) in place, dedupes, and rejects shapes outside the identifier grammar
// with a Chinese message. The kind vocabulary is open (internal/redact's rule
// table grows with it and unknown kinds are ignored by the engine), so the
// transport layer checks shape only; the host checks the category ids against
// the real table.
func validateRedactUpdate(update *RedactUpdate) error {
	if update.Kinds != nil {
		normalized, err := normalizeRedactList(*update.Kinds, true)
		if err != nil {
			return err
		}
		update.Kinds = &normalized
	}
	if update.Categories != nil {
		normalized, err := normalizeRedactList(*update.Categories, false)
		if err != nil {
			return err
		}
		update.Categories = &normalized
	}
	return nil
}

// normalizeRedactList trims, folds case, dedupes and validates one identifier
// list. kinds are MASKING kinds (SECRET/EMAIL/…), categories are rule-table
// ids (ssn/passport/…) — the empty string means "default table" and stays.
func normalizeRedactList(raw []string, kinds bool) ([]string, error) {
	what := "类别"
	grammar := "小写字母开头的标识符（如 ssn、iban、ip）"
	if kinds {
		what = "脱敏类型"
		grammar = "大写字母开头的标识符（如 SECRET、EMAIL）"
	}
	if len(raw) > redactListLimit {
		return nil, fmt.Errorf("%s列表最多 %d 项", what, redactListLimit)
	}
	out := make([]string, 0, len(raw))
	seen := map[string]bool{}
	for _, item := range raw {
		value := strings.TrimSpace(item)
		if value == "" {
			continue
		}
		if kinds {
			value = strings.ToUpper(value)
		} else {
			value = strings.ToLower(value)
		}
		if !redactIdentifier(value, kinds) {
			return nil, fmt.Errorf("%s无效：%q（应为%s）", what, item, grammar)
		}
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out, nil
}

// redactIdentifier reports whether value matches the identifier grammar:
// kinds are [A-Z][A-Z0-9_]*, categories [a-z][a-z0-9_]*, both ≤ 32 chars.
func redactIdentifier(value string, kind bool) bool {
	if value == "" || len(value) > 32 {
		return false
	}
	for i, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9' && i > 0:
		case r == '_' && i > 0:
		default:
			return false
		}
	}
	first := value[0]
	if kind {
		return first >= 'A' && first <= 'Z'
	}
	return first >= 'a' && first <= 'z'
}

// handleRedact serves GET/PUT /api/redact.
func (s *server) handleRedact(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet, http.MethodPut) {
		return
	}
	if r.Method == http.MethodGet {
		if s.opts.GetRedact == nil {
			s.unavailable(w)
			return
		}
		status, err := s.opts.GetRedact()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "读取脱敏设置失败："+err.Error(), "internal_error")
			return
		}
		writeJSON(w, http.StatusOK, normalizeRedactStatus(status))
		return
	}

	if s.opts.SaveRedact == nil {
		s.unavailable(w)
		return
	}
	var update RedactUpdate
	if !decodeJSON(w, r, &update) {
		return
	}
	if err := validateRedactUpdate(&update); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request")
		return
	}
	status, err := s.opts.SaveRedact(update)
	if err != nil {
		if errors.Is(err, ErrInvalidRequest) {
			writeError(w, http.StatusBadRequest, err.Error(), "invalid_request")
			return
		}
		writeError(w, http.StatusInternalServerError, "保存脱敏设置失败："+err.Error(), "internal_error")
		return
	}
	writeJSON(w, http.StatusOK, normalizeRedactStatus(status))
}

// normalizeRedactStatus keeps the two identifier lists JSON arrays even when
// the host has no rules configured yet.
func normalizeRedactStatus(status RedactStatus) RedactStatus {
	if status.Kinds == nil {
		status.Kinds = []string{}
	}
	if status.Categories == nil {
		status.Categories = []string{}
	}
	return status
}
