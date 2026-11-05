package config

// P10-2 redact 段的缺省与规范化（task-21）。

import (
	"strings"
	"testing"
)

func TestRedactConfigDefaults(t *testing.T) {
	cfg := Default()
	if cfg.Redact.Enabled {
		t.Error("redact.enabled 缺省应为 false（本阶段先可配后默认开）")
	}
	if !cfg.Redact.RouteAware {
		t.Error("redact.route_aware 缺省应为 true")
	}
	if len(cfg.Redact.Kinds) != 0 || len(cfg.Redact.Categories) != 0 {
		t.Error("kinds/categories 缺省应为空 = 默认表")
	}
	cfg.Normalize()
	if cfg.Redact.KeyFile != "" && !strings.HasSuffix(cfg.Redact.KeyFile, "redact.key") {
		t.Errorf("key_file 缺省应落在 <dataDir>\\redact.key：%q", cfg.Redact.KeyFile)
	}
	// 显式覆盖在 Load 的 decode-over-Default 语义下生效（这里只验 Normalize 的规范化）
	explicit := Default()
	explicit.Redact.Enabled = true
	explicit.Redact.RouteAware = false
	explicit.Redact.Kinds = []string{"  EMAIL ", "EMAIL", "PHONE"}
	explicit.Normalize()
	if !explicit.Redact.Enabled || explicit.Redact.RouteAware {
		t.Error("显式设置未生效")
	}
	if len(explicit.Redact.Kinds) != 2 || explicit.Redact.Kinds[0] != "EMAIL" {
		t.Errorf("kinds 去重/trim 失败：%v", explicit.Redact.Kinds)
	}
}