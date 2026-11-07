package presets

// D-30④ 批量覆盖语法解析器：contexts / outputs / compacts 三处共用一套解析。
//
// 语法（逗号分隔，中点在前为总默认值）：
//
//	基础值 · 模型=值, 模型=值 …
//
// 例如 "128k · gpt-4o=200k, gemini-2.5-pro=1m" 解析为：
// 基础值 128k；gpt-4o → 200k；gemini-2.5-pro → 1m。
// 逗号分隔的段是 "模型=值"；没有 "·" 的输入整体按一个模型=值 段处理
// （没有基础值的场合）；没有 "=" 的段报错，避免静默丢配置。

import (
	"fmt"
	"strings"
)

// BatchOverride is one parsed batch spec.
type BatchOverride struct {
	// Base is the value before the "·", or "" when the spec had none.
	Base string
	// Models maps a model id to its override.
	Models map[string]string
}

// ParseBatchOverrides parses one batch spec of the shared syntax.
func ParseBatchOverrides(spec string) (BatchOverride, error) {
	out := BatchOverride{Models: map[string]string{}}
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return out, nil
	}
	if i := strings.Index(spec, "·"); i >= 0 {
		out.Base = strings.TrimSpace(spec[:i])
		spec = strings.TrimSpace(spec[i+len("·"):])
	}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		eq := strings.Index(part, "=")
		if eq < 0 {
			return out, fmt.Errorf("batch override %q: 段必须含 '='", part)
		}
		model := strings.TrimSpace(part[:eq])
		val := strings.TrimSpace(part[eq+1:])
		if model == "" {
			return out, fmt.Errorf("batch override %q: 模型名不能为空", part)
		}
		if val == "" {
			return out, fmt.Errorf("batch override %q: 值不能为空", part)
		}
		out.Models[model] = val
	}
	return out, nil
}

// ApplyBatchOverride returns v overridden for model if the parsed spec has
// one, else the base value.
func ApplyBatchOverride(o BatchOverride, model, v string) string {
	if v, ok := o.Models[model]; ok {
		return v
	}
	return o.Base
}
