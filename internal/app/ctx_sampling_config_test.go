package app

import (
	"testing"

	"aiworkgate/internal/config"
	"aiworkgate/internal/gateway"
)

// task-67：采样/启发式双开关持久化——写 config → 读回 → 运行态门控一致。
func TestCtxSamplingPersistedRoundTrip(t *testing.T) {
	app, _ := newSchedulerApp(t)
	app.dataDir = t.TempDir() // config.Save/Load 的落盘位置（newSchedulerApp 不带 dataDir）
	app.cfgMu.Lock()
	app.cfg = config.Default()
	app.cfgMu.Unlock()
	// 缺省关。
	enabled, heuristics := app.ctxSamplingStatus()
	if enabled || heuristics {
		t.Fatalf("defaults = (%v, %v), want (false, false)", enabled, heuristics)
	}
	if gateway.CtxSamplingEnabled() || gateway.CtxHeuristicsEnabled() {
		t.Fatal("in-memory gates on by default")
	}
	// 开采样（不动启发式 → nil 保持存储值 false）。
	if err := app.setCtxSamplingPersisted(true, nil); err != nil {
		t.Fatalf("setCtxSamplingPersisted(true): %v", err)
	}
	enabled, heuristics = app.ctxSamplingStatus()
	if !enabled || heuristics {
		t.Fatalf("after enable = (%v, %v), want (true, false)", enabled, heuristics)
	}
	if !gateway.CtxSamplingEnabled() {
		t.Fatal("in-memory sampling gate not flipped")
	}
	// config.json 落盘核验（绕过内存快照，直接读文件）。
	cfg, err := config.Load(app.dataDir)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if !cfg.Gateway.CtxSampling || cfg.Gateway.CtxHeuristics {
		t.Fatalf("config.json = sampling:%v heuristics:%v, want true/false", cfg.Gateway.CtxSampling, cfg.Gateway.CtxHeuristics)
	}
	// 开启发式 + 关采样（一次 POST 双字段）。
	off, heuristicsOn := false, true
	if err := app.setCtxSamplingPersisted(off, &heuristicsOn); err != nil {
		t.Fatalf("setCtxSamplingPersisted(off, heuristics): %v", err)
	}
	enabled, heuristics = app.ctxSamplingStatus()
	if enabled || !heuristics {
		t.Fatalf("after second write = (%v, %v), want (false, true)", enabled, heuristics)
	}
	cfg, err = config.Load(app.dataDir)
	if err != nil {
		t.Fatalf("config.Load 2: %v", err)
	}
	if cfg.Gateway.CtxSampling || !cfg.Gateway.CtxHeuristics {
		t.Fatalf("config.json after 2 = sampling:%v heuristics:%v, want false/true", cfg.Gateway.CtxSampling, cfg.Gateway.CtxHeuristics)
	}
}
